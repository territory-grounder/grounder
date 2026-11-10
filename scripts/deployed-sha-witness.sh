#!/usr/bin/env bash
# DEPLOYED-SHA WITNESS (TG-428). Does PRODUCTION run main's code?
#
# Merged-and-green is not deployed. 2026-08-07 (TG-417): cosign-verify broke CD, the deploy job SKIPPED,
# and the estate sat on 63e2ba60 while main kept moving — every per-MR gate stayed green because every
# per-MR gate examines the TREE, and the tree was fine. The gap between "main's tip" and "what the host
# actually runs" drifts without a commit, so only a scheduled read of the RUNNING containers can see it.
#
# MECHANISM. DEPLOYED side: an AWX ad-hoc `shell` command against the Territory Grounder inventory
# (default 11 — exactly dc1tg01) asks docker which registry images the three TG containers run, and
# each answer comes back as a self-identifying `RUNNING=<image:tag>` line so the parse never depends on
# AWX output framing. MAIN side: $MAIN_SHA (CI passes CI_COMMIT_SHORT_SHA; the scheduled pipeline's
# checkout IS main's tip), falling back to `git rev-parse` — plus the tip's age, because a mismatch
# minutes after a merge is a deploy IN FLIGHT, not an incident. Only a mismatch OLDER than the grace
# window (WITNESS_GRACE_MIN, default 120m) is a failure.
#
# ★ NO GO-TEMPLATE/JINJA BRACES IN module_args, EVER (see check-deploy-host-cosign.sh): AWX renders
#   module_args through Jinja and rejects the job outright — `docker ps --format` with braces can never
#   run there. The probe below is a brace-free docker-inspect text parse.
#
# EXIT CODES — a blind witness is not a passing witness and not a failing one:
#   0  in sync, or mismatch within the grace window (deploy may be in flight)
#   1  mismatch beyond grace — the estate runs stale code
#   2  tooling error (jq/curl missing on the runner)
#   3  WITNESS BLIND — the deployed sha or main tip could not be read; refusing to certify
#
# ENV
#   AWX_HOST               default https://awx.example.net
#   AWX_TOKEN              required when not hooked — the bearer the deploy job uses
#   TG_WITNESS_INVENTORY   default 11 (Territory Grounder — exactly one host)
#   MAIN_SHA               main's tip sha (CI: CI_COMMIT_SHORT_SHA); default `git rev-parse --short=8`
#   WITNESS_GRACE_MIN      default 120 — a mismatch younger than this is "deploy may be in flight"
#
# SKIP-CI TIPS (TG-539). A tip whose commit message carries the skip-ci marker builds NO image — the
# nightly trend-baseline commit is one every day — so "deployed == tip" is the WRONG expectation for it:
# prod correctly runs the newest ancestor that BUILDS. The witness therefore resolves that ancestor
# (git rev-list walking FULL messages, else the commits API via CI_JOB_TOKEN) and compares the estate
# against IT, naming both shas. A skip-tip whose buildable ancestor cannot be resolved is BLIND, never a
# verdict. GitLab scans the WHOLE message, so a marker in a body skips a build exactly as one in the
# title — the resolution reads %B for the same reason, and this file spells the marker only in assembled
# form (its first draft carried it verbatim in the commit SUBJECT, and GitLab skipped the fix's own
# pipeline — the reviewer caught the fix shipping as an instance of its own bug). 2026-08-24 (job 477837)
# showed the original failure: deployed=28608210 vs tip=4fb3df98 (a skip-ci trend commit) went red with
# the tip age unreadable — the age read now falls back to CI_COMMIT_TIMESTAMP, present in every job env.
#
# TEST HOOKS (deterministic drill, no AWX):
#   WITNESS_DEPLOYED_TAGS  space-separated running tags; overrides the AWX read (set-but-empty = the
#                          probe answered nothing, which must be BLIND, not PASS)
#   WITNESS_MAIN_SHA       overrides MAIN_SHA/git
#   WITNESS_TIP_AGE_S      overrides the git tip-age read (seconds)
#   WITNESS_TIP_MESSAGE    overrides the tip full-message read (for the skip-ci classification)
#   WITNESS_BUILDABLE_SHA  overrides the buildable-ancestor resolution (set-but-empty = resolution
#                          answered nothing, which must be BLIND, not PASS)
set -uo pipefail

AWX_HOST="${AWX_HOST:-https://awx.example.net}"
INVENTORY="${TG_WITNESS_INVENTORY:-11}"
GRACE_MIN="${WITNESS_GRACE_MIN:-120}"

echo "== deployed-sha witness =="

blind() { echo "WITNESS BLIND: $1 — refusing to certify"; exit 3; }

# ---- MAIN side: what SHOULD be running ----------------------------------------------------------------
if [ -n "${WITNESS_MAIN_SHA:-}" ]; then
  main_sha="$WITNESS_MAIN_SHA"
elif [ -n "${MAIN_SHA:-}" ]; then
  main_sha="$MAIN_SHA"
else
  main_sha="$(git rev-parse --short=8 HEAD 2>/dev/null || true)"
fi
[ -n "$main_sha" ] || blind "no main tip (MAIN_SHA unset and git cannot name HEAD)"

# iso_to_epoch parses an ISO-8601 timestamp (Z or ±HH:MM offset) to epoch seconds, PORTABLY: GNU
# `date -d` first, else jq — which this witness already hard-requires — with the offset applied by hand
# (jq's mktime is UTC; strptime on the local part alone would be wrong by the offset, and the grace
# window is 120m, so a ±2h server-offset error is a full grace width). Empty output = unparseable.
# The 2026-08-25 played run proved the need: the ci-deploy-tools image has BusyBox date (no -d ISO
# parsing) AND no git, so both age paths died and a mismatch morning still went BLIND — the original
# TG-539 failure, surviving in exactly the image the scheduled job runs in.
# WITNESS_NO_GNU_DATE (test hook) skips the GNU attempt so the drill can prove the jq path anywhere.
iso_to_epoch() {
  local ts="$1" out
  if [ -z "${WITNESS_NO_GNU_DATE:-}" ]; then
    out=$(date -d "$ts" +%s 2>/dev/null) && { printf '%s' "$out"; return 0; }
  fi
  # Pure-arithmetic epoch — deliberately NOT jq's strptime/mktime, whose C-library round-trip proved
  # TZ-tainted on this build (a +02:00 August timestamp came back one hour forward: "tip is -30m old").
  # A days-since-epoch formula has no library to disagree with; cross-checked 6/6 against GNU date
  # incl. leap-day, negative half-hour offsets, and century edges.
  jq -rn --arg t "$ts" '
    ($t | capture("^(?<Y>[0-9]{4})-(?<Mo>[0-9]{2})-(?<D>[0-9]{2})T(?<H>[0-9]{2}):(?<Mi>[0-9]{2}):(?<S>[0-9]{2})(\\.[0-9]+)?(?<tz>Z|[+-][0-9]{2}:?[0-9]{2})$")) as $m
    | ($m.Y|tonumber) as $y | ($m.Mo|tonumber) as $mo | ($m.D|tonumber) as $d
    | def leaps(y): ((y/4)|floor) - ((y/100)|floor) + ((y/400)|floor);
      ([0,31,59,90,120,151,181,212,243,273,304,334][$mo-1]) as $cum
    | (if $mo > 2 and (($y % 4 == 0 and $y % 100 != 0) or $y % 400 == 0) then 1 else 0 end) as $leapday
    | ((($y - 1970) * 365 + (leaps($y - 1) - leaps(1969)) + $cum + $leapday + ($d - 1)) * 86400
       + ($m.H|tonumber)*3600 + ($m.Mi|tonumber)*60 + ($m.S|tonumber)) as $utc
    | (if $m.tz == "Z" then 0
       else (($m.tz | capture("(?<s>[+-])(?<h>[0-9]{2}):?(?<mi>[0-9]{2})")) as $o
             | (if $o.s == "-" then -1 else 1 end) * (($o.h|tonumber)*3600 + ($o.mi|tonumber)*60))
       end) as $off
    | $utc - $off' 2>/dev/null || true
}

if [ -n "${WITNESS_TIP_AGE_S:-}" ]; then
  age_s="$WITNESS_TIP_AGE_S"
else
  tip_ct="$(git show -s --format=%ct HEAD 2>/dev/null || true)"
  # Git-independent fallback (TG-539): the scheduled job's git read came back empty while the job env
  # always carries CI_COMMIT_TIMESTAMP — an unreadable age turned an ordinary mismatch into BLIND.
  if [ -z "$tip_ct" ] && [ -n "${CI_COMMIT_TIMESTAMP:-}" ]; then
    tip_ct="$(iso_to_epoch "$CI_COMMIT_TIMESTAMP")"
  fi
  if [ -n "$tip_ct" ]; then age_s=$(( $(date +%s) - tip_ct )); else age_s=""; fi
fi

# ---- skip-ci tip → resolve the newest BUILDABLE ancestor (TG-539) -------------------------------------
# The deploy expectation for a tip that builds no image is its newest image-building ancestor, not itself.
# GitLab's skip detection scans the FULL commit message, not the subject — a marker in the body skips the
# pipeline exactly as one in the title does — so every check here reads the full message (%B) too. The
# marker is assembled from pieces so this script's own commit can never trigger the detection it models.
skip_re="\[skip$(printf ' ')ci\]|\[ci$(printf ' ')skip\]"
if [ -n "${WITNESS_TIP_MESSAGE+x}" ]; then
  tip_message="$WITNESS_TIP_MESSAGE"
else
  tip_message="$(git show -s --format=%B HEAD 2>/dev/null || true)"
  [ -n "$tip_message" ] || tip_message="${CI_COMMIT_MESSAGE:-${CI_COMMIT_TITLE:-}}"
fi

cmp_sha="$main_sha"
tip_note=""
if printf '%s' "$tip_message" | grep -qiE "$skip_re"; then
  if [ -n "${WITNESS_BUILDABLE_SHA+x}" ]; then
    build_sha="$WITNESS_BUILDABLE_SHA"
  else
    # git first: walk ancestors newest-first, keep the first whose FULL message carries no skip marker.
    build_sha=""
    for cand in $(git rev-list -50 HEAD 2>/dev/null || true); do
      if ! git show -s --format=%B "$cand" 2>/dev/null | grep -qiE "$skip_re"; then
        build_sha="$(printf '%s' "$cand" | cut -c1-8)"
        break
      fi
    done
    # commits API second (the scheduled runner where git reads come back empty). The API's `message`
    # field is the full message, matching the git path's %B semantics.
    if [ -z "$build_sha" ] && [ -n "${CI_API_V4_URL:-}" ] && [ -n "${CI_PROJECT_ID:-}" ] && [ -n "${CI_JOB_TOKEN:-}" ]; then
      command -v jq   >/dev/null 2>&1 || { echo "TOOLING ERROR: jq is required for the buildable-ancestor API fallback"; exit 2; }
      command -v curl >/dev/null 2>&1 || { echo "TOOLING ERROR: curl is required for the buildable-ancestor API fallback"; exit 2; }
      build_sha="$(curl -sk -m 30 -H "JOB-TOKEN: $CI_JOB_TOKEN" \
        "${CI_API_V4_URL%/}/projects/${CI_PROJECT_ID}/repository/commits?ref_name=${CI_COMMIT_BRANCH:-main}&per_page=50" \
        | jq -r --arg re "$skip_re" '[.[] | select(((.message // .title) | test($re; "i")) | not)][0].id // empty' 2>/dev/null \
        | cut -c1-8 || true)"
    fi
  fi
  [ -n "$build_sha" ] || blind "tip $main_sha carries the skip-ci marker (builds no image) and its last BUILDABLE ancestor could not be resolved"
  cmp_sha="$build_sha"
  tip_note=" (tip $main_sha carries the skip-ci marker, builds no image; expecting last buildable $build_sha)"
fi

# ---- DEPLOYED side: what IS running -------------------------------------------------------------------
if [ -n "${WITNESS_DEPLOYED_TAGS+x}" ]; then
  tags="$WITNESS_DEPLOYED_TAGS"
  src="test hook"
else
  command -v jq   >/dev/null 2>&1 || { echo "TOOLING ERROR: jq is required to talk to the AWX API"; exit 2; }
  command -v curl >/dev/null 2>&1 || { echo "TOOLING ERROR: curl is required to talk to the AWX API"; exit 2; }
  [ -n "${AWX_TOKEN:-}" ] || blind "could not read the deployed sha (AWX_TOKEN is not set)"

  # Brace-free by construction: docker inspect prints `"Image": "registry…:<tag>"` only for the
  # registry-pinned Config.Image (the top-level Image is an untagged sha256 id and never matches).
  probe='docker inspect territory-grounder-worker-1 territory-grounder-worker-actuate-1 territory-grounder-grounder-1 2>/dev/null | grep -o "\"Image\": \"registry[^\"]*\"" | tr -d "\" " | sed "s/^Image:/RUNNING=/"'
  body=$(jq -nc --arg args "$probe" '{module_name:"shell", module_args:$args, credential:24}')
  resp=$(curl -sk -m 60 -X POST -H "Authorization: Bearer $AWX_TOKEN" -H 'Content-Type: application/json' \
    -d "$body" "${AWX_HOST%/}/api/v2/inventories/${INVENTORY}/ad_hoc_commands/" || true)
  id=$(printf '%s' "$resp" | jq -r '.id // empty' 2>/dev/null || true)
  [ -n "$id" ] || blind "could not read the deployed sha (AWX ad-hoc launch failed on inventory $INVENTORY)"

  status="unknown"
  for _ in $(seq 1 24); do
    status=$(curl -sk -m 30 -H "Authorization: Bearer $AWX_TOKEN" \
      "${AWX_HOST%/}/api/v2/ad_hoc_commands/${id}/" | jq -r '.status // "unknown"' 2>/dev/null || echo unknown)
    case "$status" in successful|failed|error|canceled) break ;; esac
    sleep 5
  done
  [ "$status" = "successful" ] || blind "could not read the deployed sha (AWX ad-hoc $id ended '$status')"

  out=$(curl -sk -m 60 -H "Authorization: Bearer $AWX_TOKEN" \
    "${AWX_HOST%/}/api/v2/ad_hoc_commands/${id}/stdout/?format=txt" || true)
  # The tag is everything after the LAST colon of each self-identifying RUNNING= line.
  tags=$(printf '%s\n' "$out" | grep -o 'RUNNING=[^ ]*' | sed -e 's/^RUNNING=//' -e 's/.*://' | tr '\n' ' ')
  src="AWX ad-hoc $id (inventory $INVENTORY)"
fi

# VACUITY FLOOR — the drill's killing-mutation target. Zero RUNNING= markers means docker named no
# containers, the ad-hoc hit the wrong host, or the output was truncated. With this check deleted the
# comparison loop below iterates zero times, finds zero mismatches, and certifies "in sync" over an
# estate it never saw — found-nothing and found-nothing-wrong are different claims.
if [ -z "$(echo $tags)" ]; then
  blind "could not read the deployed sha (no RUNNING= marker came back from $src)"
fi

# ---- verdict ------------------------------------------------------------------------------------------
n=0
stale=""
for t in $tags; do
  n=$((n+1))
  case "$t" in "$cmp_sha"*) continue ;; esac   # deployed tag carries the expected sha (or a longer form)
  case "$cmp_sha" in "$t"*) continue ;; esac   # expected sha carries the deployed tag as its prefix
  stale="$stale $t"
done
deployed_disp=$(echo $tags | tr ' ' '\n' | sort -u | paste -sd, -)

if [ -z "$stale" ]; then
  if [ -n "$age_s" ]; then age_disp="$(( age_s / 3600 ))h"; else age_disp="unknown"; fi
  echo "deployed-sha witness: PASS — deployed=$deployed_disp main=$cmp_sha, tip age $age_disp — in sync ($n/$n running container image(s) on main's sha, via $src)$tip_note"
  exit 0
fi

# A mismatch needs the tip age to tell "stale" from "deploy in flight"; without it the witness is blind.
[ -n "$age_s" ] || blind "mismatch (deployed=$deployed_disp main=$cmp_sha) but the tip age is unreadable, so stale cannot be told from in-flight"

age_min=$(( age_s / 60 ))
if [ "$age_min" -gt "$GRACE_MIN" ]; then
  echo "deployed-sha witness: FAIL — deployed=$deployed_disp main=$cmp_sha, tip age $(( age_s / 3600 ))h (${age_min}m > grace ${GRACE_MIN}m) — estate runs stale code ($(echo $stale | wc -w)/$n running container image(s) off main's sha, via $src)$tip_note"
  echo "  merged+green is not deployed: check the deploy job (it SKIPS silently when AWX is down or cosign-verify fails — TG-417)."
  exit 1
fi
echo "deployed-sha witness: PASS — deployed=$deployed_disp main=$cmp_sha — deploy may be in flight (tip is ${age_min}m old, grace ${GRACE_MIN}m; $(echo $stale | wc -w)/$n image(s) not yet on main's sha, via $src)$tip_note"
exit 0
