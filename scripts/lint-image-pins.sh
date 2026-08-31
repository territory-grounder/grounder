#!/usr/bin/env bash
# Supply-chain gate (TG-159, REOPENED as TG-283): every THIRD-PARTY base/runtime image in the deployed path
# MUST be pinned by @sha256. A floating tag silently changes the trusted computing base under a signed
# governance system — most acutely `litellm:main-stable`, which holds every provider API key. Resolve a
# digest with:
#   docker buildx imagetools inspect <repo>:<tag> --format '{{.Manifest.Digest}}'
# then write it as  repo:tag@sha256:<digest>  (keep the tag for readability; the digest is what's enforced).
#
# ★ WHY THIS FILE WAS REWRITTEN (TG-283, 2026-08-04). TG-159 was marked Fixed on the previous version, and
# that version could not see the thing it most needed to see. It worked by ALLOWLIST:
#
#     grep -iE 'golang|distroless|pgvector|litellm|postgres:|temporalio|prom/|grafana/'
#
# so an image whose name was not already on that list was invisible. `busybox:stable` was not on the list.
# It is live on dc1tg01 (measured: `docker ps` shows territory-grounder-secrets-perms-1 running
# busybox:stable), it runs as **uid 0**, and it read-write mounts ./secrets and ./knowledge — the single
# most dangerous unpinned image in the stack was the one the gate was structurally unable to name. So was
# `nginx:1.27-alpine` in deploy/console/Dockerfile, the base of the ONLY container published on 0.0.0.0.
#
# Worse, the gate PASSED VACUOUSLY: `grep -h ... 2>/dev/null` over files that do not exist yields empty,
# empty is not "unpinned", so `bash scripts/lint-image-pins.sh` in an empty directory printed PASS and
# returned 0. A gate that reports success while examining zero files is not a gate — the same defect class
# that let the protected-path gate pass on every main commit for weeks (see scripts/lint-protected-paths.sh).
#
# The rewrite inverts the logic to DENY-BY-DEFAULT and adds a vacuity floor at every stage of the funnel:
#   - zero files scanned, zero image refs found, or zero third-party images left after the exemption ⇒ FAIL,
#     loudly, saying which matcher is broken. The gate can no longer be green about nothing.
#   - every image ref is third-party UNLESS it is provably ours, so a new image is caught by DEFAULT
#     instead of needing someone to remember to extend a keyword list.
set -u

# Relative to the CALLER's directory, deliberately — `make all` and .gitlab-ci.yml both invoke this from the
# repository root, and keeping the original contract keeps the TG-283 reproduction honest: running the gate
# from a directory with no deploy/ must now FAIL (it used to print PASS). IMAGE_PIN_ROOT is the test hook.
ROOT="${IMAGE_PIN_ROOT:-.}"

# ── the scan set ────────────────────────────────────────────────────────────────────────────────────────
# DISCOVERED, not listed. The old gate hard-coded two paths, so deploy/console/Dockerfile — which builds the
# console image this pipeline ships — was never read by it. `find` means a Dockerfile or compose file added
# under deploy/ tomorrow is scanned the day it lands, with nobody having to remember this file exists.
#
# deploy/claude-proxy/ USED TO BE EXCLUDED and no longer is (TG-310). The old reason was sound and is worth
# keeping written down: that directory is the SIDECAR stack on a different host, `deploy-sidecar` triggers on
# `changes: [deploy/claude-proxy/**/*]`, and pinning its bases from inside a lint fix would have rebuilt and
# redeployed the sidecar the estate's agents run on — as a side effect of a supply-chain cleanup. That needed
# its own change window, so the bases were tracked separately rather than ridden in.
#
# They are pinned now, in a commit whose whole purpose is the sidecar, so the exclusion has no remaining
# justification — and leaving it would mean the gate permanently cannot see the one image that executes
# model-directed work on the box holding an OpenBao root token. The scan is now the whole of deploy/.
files=$(find "$ROOT/deploy" \
          \( -type f \( -name 'Dockerfile' -o -name 'Dockerfile.*' \
                        -o -name '*compose*.yml' -o -name '*compose*.yaml' \) -print \) 2>/dev/null \
        | sed 's|^\./||' | sort)

n_files=$(printf '%s' "$files" | grep -c . || true)
if [ "$n_files" -eq 0 ]; then
  echo "FAIL: image-pin gate scanned ZERO files under $ROOT/deploy — the FILE matcher is broken (or the"
  echo "      deployed path moved). A supply-chain gate that examines nothing must never report PASS; that"
  echo "      is exactly how TG-159 was closed while busybox:stable ran unpinned as uid 0 in production."
  exit 1
fi

# ── the landmark ────────────────────────────────────────────────────────────────────────────────────────
# deploy/docker-compose.yml IS the deployed stack. If the scan is not reading it, every "PASS" below is a
# statement about the wrong files — non-empty, and still blind. One named landmark, checked, so a rename or
# a bad prune cannot quietly narrow the gate to the Dockerfiles.
if ! printf '%s\n' "$files" | grep -q 'deploy/docker-compose\.yml$'; then
  echo "FAIL: deploy/docker-compose.yml is NOT in the scan set — the gate is not reading the deployed stack."
  printf '%s\n' "$files" | sed 's/^/      scanned: /'
  exit 1
fi

# ── extraction ──────────────────────────────────────────────────────────────────────────────────────────
# Emits  file:line<TAB>raw-ref  for every image reference: `FROM <ref> [AS stage]` in a Dockerfile and
# `image: <ref>` in a compose file. --platform= flags, quotes and the `AS <stage>` suffix are stripped;
# multi-stage internal references (`FROM builder`) are dropped by the stage-name filter below.
# `< /dev/null` is load-bearing: with an empty $files, grep would read STDIN and the gate would HANG instead
# of reporting. Measured while executing the killing mutation for the vacuity floor — the floors above make
# it unreachable in practice, but a gate must not depend on another check having already fired.
refs=$(grep -nE '^[[:space:]]*(FROM|image:)[[:space:]]' $files < /dev/null 2>/dev/null \
  | sed -E 's/[[:space:]]*(#.*)?$//' \
  | sed -E 's/[[:space:]]*(FROM|image:)[[:space:]]+/\t/' \
  | sed -E 's/--platform=[^[:space:]]+[[:space:]]+//' \
  | sed -E 's/[[:space:]]+[Aa][Ss][[:space:]]+[A-Za-z0-9_.-]+$//' \
  | sed -E 's/"//g; s/'"'"'//g')

n_refs=$(printf '%s' "$refs" | grep -c . || true)
if [ "$n_refs" -eq 0 ]; then
  echo "FAIL: image-pin gate found ZERO image references in $n_files scanned file(s) — the IMAGE-REF matcher"
  echo "      is broken. Files were read, so this is the regex, not the path."
  printf '%s\n' "$files" | sed 's/^/      scanned: /'
  exit 1
fi

# Multi-stage build stages (`FROM golang:... AS build` → later `COPY --from=build`, or `FROM build`) are
# internal names, not images. Collected from the same scan so the list cannot drift from the Dockerfiles.
stages=$(grep -hoiE '[[:space:]][Aa][Ss][[:space:]]+[A-Za-z0-9_.-]+[[:space:]]*$' $files < /dev/null 2>/dev/null \
  | awk '{print $2}' | sort -u)

# ── classification ──────────────────────────────────────────────────────────────────────────────────────
# THIRD-PARTY BY DEFAULT. The single exemption is the pipeline's OWN output, and it is recognised
# structurally rather than by name: `${TG_*_IMAGE:-…}:${TG_TAG:-…}` is the registry+tag pair this pipeline
# pushes (.gitlab-ci.yml sets TG_GROUNDER_IMAGE / TG_WORKER_IMAGE / TG_CONSOLE_IMAGE and deploys $SHA).
# Both halves are required: a bare `${TG_ANYTHING_IMAGE:-alpine}` with a literal tag is NOT our output and
# stays third-party. Those images are this pipeline's own build output (the cosign sign/verify chain was
# retired 2026-08-10, TG-417); digest-pinning them in the tree is impossible (the digest does not exist
# until the build runs) and unnecessary.
own_re='\$\{TG_[A-Z0-9_]*IMAGE(:-[^}]*)?\}.*:\$\{TG_TAG(:-[^}]*)?\}'

# LOCALLY-BUILT COMPOSE IMAGES ARE NOT THIRD-PARTY, and this is recognised STRUCTURALLY (TG-310).
#
# A compose service carrying `build:` produces its own image; its `image:` key is the NAME that build is
# tagged with, not something pulled from a registry. `tg-claude-proxy:1.2.0` is exactly that — the sidecar's
# local alias, which the deploy retags a signed <short-sha> image over. Demanding a @sha256 on it is
# impossible in the same way it is impossible for the pipeline's own output: the digest does not exist
# until the build runs.
#
# Parsed with awk rather than a YAML library: this gate runs in a minimal CI image where PyYAML is
# absent, and a parser that silently returns nothing there would drop the exemption and fail the
# build for the wrong reason — which is exactly what the first version of this did.
#
# Derived from the compose files themselves rather than a name allowlist, so a service that STOPS building
# locally and starts pulling loses the exemption automatically — the failure mode a hardcoded name list has.
built_locally=$(for f in $(printf '%s\n' "$files" | grep -E 'compose.*\.ya?ml$' || true); do
  awk '
    # Service blocks sit at two-space indent under `services:`; their keys at four. Track the current
    # service, remember whether it declared build:, and emit its image: only if it did.
    /^  [A-Za-z0-9_.-]+:[[:space:]]*$/ { if (svc && bld && img != "") print img; svc=$0; bld=0; img=""; next }
    /^    build:/                      { bld=1; next }
    /^    image:[[:space:]]/           { img=$2; next }
    END                                { if (svc && bld && img != "") print img }
  ' "$ROOT/$f" 2>/dev/null || true
done | tr -d '"' | sort -u)

third_party=""
unpinned=""
while IFS= read -r line; do
  [ -n "$line" ] || continue
  loc=${line%%$'\t'*}
  ref=${line#*$'\t'}
  [ -n "$ref" ] || continue

  # our own build output → exempt
  if printf '%s' "$ref" | grep -qE "$own_re"; then continue; fi
  # an internal multi-stage name → not an image at all
  if [ -n "$stages" ] && printf '%s\n' "$stages" | grep -qxF "$ref"; then continue; fi
  # a compose image this repo BUILDS rather than pulls → not third-party
  if [ -n "$built_locally" ] && printf '%s\n' "$built_locally" | grep -qxF "$ref"; then continue; fi

  # Expand `${VAR:-default}` so a defaulted third-party ref is judged on what it actually resolves to.
  resolved=$(printf '%s' "$ref" | sed -E 's/\$\{[A-Za-z_][A-Za-z0-9_]*:-([^}]*)\}/\1/g')

  # ALSO expand a plain `${VAR}` against an `ARG VAR=default` DECLARED IN THE SAME FILE (TG-310).
  #
  # This gate's own failure message already told the author to use "a defaulted ARG" — and then did not
  # read one, so the advice it gave could not clear it. `ARG X=<pinned>` + `FROM img:${X}` is the ordinary
  # Dockerfile idiom (it is how the sidecar is written), and refusing to resolve it pushes authors toward
  # inlining `${X:-…}` in every FROM, which duplicates the pin per stage — more places to forget.
  #
  # Only the file's OWN ARG defaults count. A build-arg supplied at `docker build --build-arg` time can
  # still override it, which is exactly why an ARG with NO default stays unresolvable and fails below:
  # that one genuinely names a different image depending on who invokes the build.
  file_path=${loc%%:*}
  while printf '%s' "$resolved" | grep -q '[$]'; do
    var=$(printf '%s' "$resolved" | sed -nE 's/.*\$\{([A-Za-z_][A-Za-z0-9_]*)\}.*/\1/p' | head -1)
    [ -n "$var" ] || break
    argdef=$(grep -E "^[[:space:]]*ARG[[:space:]]+${var}=" "$ROOT/$file_path" 2>/dev/null | head -1 | sed -E "s/^[[:space:]]*ARG[[:space:]]+${var}=//")
    [ -n "$argdef" ] || break
    resolved=$(printf '%s' "$resolved" | sed "s|\${${var}}|${argdef}|g")
  done

  third_party="${third_party}${loc}	${ref}
"
  # An unexpandable variable is UNPINNED by construction: `FROM ${BASE}` names a different image depending
  # on who invokes the build, which is the drift this gate exists to stop. Fail closed rather than guess.
  if printf '%s' "$resolved" | grep -q '[$]'; then
    unpinned="${unpinned}${loc}	${ref}   (unresolvable variable — give it a literal repo:tag@sha256 or a defaulted ARG)
"
  elif ! printf '%s' "$resolved" | grep -q '@sha256:'; then
    unpinned="${unpinned}${loc}	${ref}
"
  fi
done <<EOF
$refs
EOF

n_third=$(printf '%s' "$third_party" | grep -c . || true)
if [ "$n_third" -eq 0 ]; then
  echo "FAIL: image-pin gate classified ALL $n_refs image reference(s) as exempt — the EXEMPTION rule ate"
  echo "      the whole scan. A gate whose allowlist covers everything enforces nothing."
  exit 1
fi

n_unpinned=$(printf '%s' "$unpinned" | grep -c . || true)
if [ "$n_unpinned" -gt 0 ]; then
  echo "FAIL: $n_unpinned unpinned third-party image(s) — pin by @sha256 (repo:tag@sha256:<digest>):"
  printf '%s' "$unpinned" | sed 's/^/  /'
  echo "  resolve a digest with: docker buildx imagetools inspect <repo>:<tag> --format '{{.Manifest.Digest}}'"
  exit 1
fi

echo "image-pin gate: PASS — $n_third third-party image(s) across $n_files file(s), all pinned by @sha256"
echo "  ($((n_refs - n_third)) exempt: this pipeline's own \${TG_*_IMAGE}:\${TG_TAG} output and build stages)"
