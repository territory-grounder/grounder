#!/usr/bin/env bash
# DRILL FOR THE IMAGE-PIN GATE (TG-283). A gate nobody drills is a gate nobody knows can fail, and this one
# spent its whole life unable to fail: it matched images by keyword allowlist (busybox was not on the list)
# and, over a directory with no deploy/ in it, printed "PASS" and returned 0. TG-159 was closed on that.
#
# Same convention as scripts/lint-protected-paths_test.sh and scripts/lint-eval-evidence_test.sh: every arm
# of the gate is proven to both PASS and REFUSE, against fixtures built here rather than against repository
# history (a test coupled to what the tree happens to contain is flaky by construction).
#
# KILLING MUTATION (executed 2026-08-04): delete the `n_files -eq 0` vacuity floor from
# scripts/lint-image-pins.sh and this drill goes RED on "an empty tree REFUSES (the TG-159 vacuous PASS)" —
# the arm that says a supply-chain gate examining zero files must never report success. Restore ⇒ green.
# Second mutation: restore the old keyword allowlist and "a KNOWN-UNPINNED busybox is REFUSED" goes RED,
# naming the uid-0 container that read-write mounts ./secrets.
set -uo pipefail
cd "$(dirname "$0")/.."
G=scripts/lint-image-pins.sh
fail=0
ran=0

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# check <name> <want-rc> <root> [<expected-substring-in-output>]
check() {
  local name="$1" want="$2" root="$3" want_msg="${4:-}"
  local out rc
  out="$(IMAGE_PIN_ROOT="$root" bash "$G" 2>&1)"; rc=$?
  ran=$((ran + 1))
  if [ "$rc" -ne "$want" ]; then
    echo "  FAIL: $name — want rc=$want got rc=$rc"
    printf '%s\n' "$out" | sed 's/^/      /'
    fail=1
    return
  fi
  # The message is part of the contract. A gate that fails with an unreadable reason gets "fixed" by
  # deleting it — this asserts the output names WHAT is wrong, not just that something is.
  if [ -n "$want_msg" ] && ! printf '%s' "$out" | grep -qF "$want_msg"; then
    echo "  FAIL: $name — rc was right but the output never said '$want_msg'"
    printf '%s\n' "$out" | sed 's/^/      /'
    fail=1
    return
  fi
  echo "  ok: $name (rc=$rc)"
}

# fixture <name> — makes $TMP/<name>/deploy and echoes the root path
fixture() { mkdir -p "$TMP/$1/deploy"; printf '%s' "$TMP/$1"; }

echo "== image-pin gate drill =="

# (1) THE REAL TREE PASSES. Without this the whole drill could be satisfied by a gate that refuses
#     everything, which is just as useless as one that accepts everything.
check "the repository's own deployed path passes" 0 "." "image-pin gate: PASS"

# (2) THE TG-283 DEFECT ITSELF: an empty tree used to print PASS and return 0. It must now REFUSE and say
#     the matcher is broken, because "found nothing" and "found nothing wrong" are different claims.
empty=$(fixture empty); rmdir "$empty/deploy"
check "an empty tree REFUSES (the TG-159 vacuous PASS)" 1 "$empty" "scanned ZERO files"

# (3) Files present, but no image reference in any of them ⇒ the REF matcher is broken, not the paths.
noref=$(fixture noref)
printf 'services:\n  x:\n    restart: "no"\n' > "$noref/deploy/docker-compose.yml"
check "a deploy tree with no image line at all REFUSES" 1 "$noref" "ZERO image references"

# (4) THE MISS THAT REOPENED TG-159: busybox:stable, the uid-0 container that read-write mounts ./secrets,
#     was invisible to the old keyword allowlist. Deny-by-default must catch it by name.
bb=$(fixture busybox)
printf 'services:\n  postgres:\n    image: postgres:16@sha256:%064d\n  secrets-perms:\n    image: busybox:stable\n' 0 \
  > "$bb/deploy/docker-compose.yml"
check "a KNOWN-UNPINNED busybox is REFUSED" 1 "$bb" "busybox:stable"

# (5) …and the same tree with the digest added PASSES, so the refusal is about the pin and nothing else.
bbp=$(fixture busybox-pinned)
printf 'services:\n  postgres:\n    image: postgres:16@sha256:%064d\n  secrets-perms:\n    image: busybox:stable@sha256:%064d\n' 0 1 \
  > "$bbp/deploy/docker-compose.yml"
check "the same tree with the digest pinned passes" 0 "$bbp" "image-pin gate: PASS"

# (6) An unpinned base in a Dockerfile is caught too — deploy/console/Dockerfile was outside the old gate's
#     hard-coded scan set entirely, so nginx floated under the only container published on 0.0.0.0.
df=$(fixture dockerfile)
printf 'services:\n  postgres:\n    image: postgres:16@sha256:%064d\n' 0 > "$df/deploy/docker-compose.yml"
mkdir -p "$df/deploy/console"
printf 'FROM nginx:1.27-alpine\nRUN true\n' > "$df/deploy/console/Dockerfile"
check "an unpinned FROM in a NESTED Dockerfile is found and REFUSED" 1 "$df" "nginx:1.27-alpine"

# (7) A multi-stage build stage name is not an image and must not be reported — otherwise the gate is noise
#     and gets switched off. `FROM golang:… AS build` … `FROM build` is the shape in deploy/Dockerfile.
ms=$(fixture multistage)
printf 'services:\n  postgres:\n    image: postgres:16@sha256:%064d\n' 0 > "$ms/deploy/docker-compose.yml"
printf 'FROM golang:1.25@sha256:%064d AS build\nRUN true\nFROM build\n' 0 > "$ms/deploy/Dockerfile"
check "a multi-stage stage name is not treated as an unpinned image" 0 "$ms" "image-pin gate: PASS"

# (8) An unexpandable variable names a different image depending on who runs the build. Fail closed rather
#     than guess — this is the evasion route a keyword allowlist could never have seen either.
var=$(fixture variable)
printf 'services:\n  postgres:\n    image: postgres:16@sha256:%064d\n' 0 > "$var/deploy/docker-compose.yml"
printf 'ARG BASE\nFROM ${BASE}\n' > "$var/deploy/Dockerfile"
check "an unresolvable \${VAR} base is REFUSED, not assumed pinned" 1 "$var" "unresolvable variable"

# (9) EXEMPTION VACUITY: a tree in which every image is the pipeline's own output leaves the gate enforcing
#     nothing, so it must refuse rather than report a PASS that covers zero third-party images.
exempt=$(fixture allexempt)
printf 'services:\n  worker:\n    image: ${TG_WORKER_IMAGE:-registry.example/worker}:${TG_TAG:-latest}\n' \
  > "$exempt/deploy/docker-compose.yml"
check "a tree of ONLY our own images REFUSES (nothing left to enforce)" 1 "$exempt" "EXEMPTION rule ate"

# (10) …and that exemption still works when a real third-party image is present, or every pipeline would
#      red on its own build output (whose digest does not exist until the build runs).
mix=$(fixture mixed)
printf 'services:\n  worker:\n    image: ${TG_WORKER_IMAGE:-registry.example/worker}:${TG_TAG:-latest}\n  db:\n    image: postgres:16@sha256:%064d\n' 0 \
  > "$mix/deploy/docker-compose.yml"
check "our own \${TG_*_IMAGE}:\${TG_TAG} output stays exempt alongside a pinned third-party image" 0 "$mix" \
  "1 third-party image"

# (11) LANDMARK: the deployed stack is deploy/docker-compose.yml. A scan that finds files but not that one
#      is non-empty and still blind, which is the failure mode the vacuity floor alone does not catch.
nolm=$(fixture nolandmark)
printf 'FROM golang:1.25@sha256:%064d\n' 0 > "$nolm/deploy/Dockerfile"
check "a scan set missing deploy/docker-compose.yml REFUSES" 1 "$nolm" "NOT in the scan set"

# The drill's own vacuity floor: if the fixtures stopped being built (a mktemp failure, a renamed helper),
# every `check` above could vanish and this script would still exit 0 with a cheerful banner.
if [ "$ran" -lt 11 ]; then
  echo "image-pin gate drill: FAIL — only $ran assertion(s) ran; the drill itself is vacuous"
  exit 1
fi

if [ "$fail" -eq 0 ]; then
  echo "image-pin gate drill: PASS ($ran assertions)"
else
  echo "image-pin gate drill: FAIL"
fi
exit "$fail"
