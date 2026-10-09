#!/usr/bin/env bash
# MERGE-GATE SETTING WITNESS (TG-428). Is only_allow_merge_if_pipeline_succeeds still ON?
#
# The setting is the ONE server-side latch that stops a red pipeline from merging to main, and it lives
# in the GitLab project config, not in this tree: flipping it off produces no commit, no MR, no pipeline
# — nothing any tree-scoped gate can see. The red-main fall-through it prevents is not hypothetical on
# this project; the only way to notice the latch going soft is to read it back, on a schedule, from the
# server that enforces it.
#
# EXIT CODES — a blind witness certifies nothing, in either direction:
#   0  the setting is true — merge gate ON
#   1  the setting is false — red-main fall-through is re-armed
#   2  tooling error (jq/curl missing on the runner)
#   3  MERGE GATE WITNESS BLIND — no token / unreachable / unparseable; the setting's state is UNKNOWN
#
# TOKEN RESOLUTION (first match wins): GITLAB_API_TOKEN, then TG_READONLY_API_TOKEN (both PRIVATE-TOKEN
# headers), then CI_JOB_TOKEN (JOB-TOKEN header). NOTE: a job token usually CANNOT read project settings
# (403), which is reported as BLIND, not as OFF and not as ON — provision TG_READONLY_API_TOKEN
# (read_api) in CI/CD variables to clear it.
#
# TEST HOOK: MERGE_GATE_JSON overrides the fetch entirely (set-but-empty = the fetch answered nothing,
# which must be BLIND, not PASS).
set -uo pipefail

GL_URL="${GITLAB_URL:-https://gitlab.example.net}"
PROJECT_PATH="products/territory-grounder/grounder"
PROJECT_ENC="products%2Fterritory-grounder%2Fgrounder"

echo "== merge-gate setting witness ($PROJECT_PATH) =="

blind() { echo "MERGE GATE WITNESS BLIND: $1 — cannot certify the setting"; exit 3; }

command -v jq >/dev/null 2>&1 || { echo "TOOLING ERROR: jq is required to parse the project JSON"; exit 2; }

if [ -n "${MERGE_GATE_JSON+x}" ]; then
  json="$MERGE_GATE_JSON"
  src="MERGE_GATE_JSON test hook"
else
  command -v curl >/dev/null 2>&1 || { echo "TOOLING ERROR: curl is required to reach the GitLab API"; exit 2; }
  if [ -n "${GITLAB_API_TOKEN:-}" ]; then
    hdr="PRIVATE-TOKEN: $GITLAB_API_TOKEN"; src="GITLAB_API_TOKEN"
  elif [ -n "${TG_READONLY_API_TOKEN:-}" ]; then
    hdr="PRIVATE-TOKEN: $TG_READONLY_API_TOKEN"; src="TG_READONLY_API_TOKEN"
  elif [ -n "${CI_JOB_TOKEN:-}" ]; then
    hdr="JOB-TOKEN: $CI_JOB_TOKEN"; src="CI_JOB_TOKEN"
  else
    blind "no API token (set GITLAB_API_TOKEN or TG_READONLY_API_TOKEN, or run in CI for CI_JOB_TOKEN)"
  fi
  tmp=$(mktemp)
  http=$(curl -sk -m 30 -H "$hdr" -o "$tmp" -w '%{http_code}' "${GL_URL%/}/api/v4/projects/${PROJECT_ENC}" || echo 000)
  json=$(cat "$tmp" 2>/dev/null || true)
  rm -f "$tmp"
  case "$http" in
    200) : ;;
    000) blind "could not reach ${GL_URL%/} (network error or timeout)" ;;
    401|403) blind "HTTP $http from the projects API via $src (token cannot read project settings)" ;;
    *)   blind "HTTP $http from the projects API via $src" ;;
  esac
fi

# VACUITY FLOOR: an empty body parses to nothing — the fetch answered NOTHING, and nothing is not "true".
[ -n "$json" ] || blind "empty project JSON from $src"

val=$(printf '%s' "$json" | jq -r '.only_allow_merge_if_pipeline_succeeds' 2>/dev/null) || val=""
case "$val" in
  true)
    echo "merge gate: ON (only_allow_merge_if_pipeline_succeeds, checked live) — 1/1 setting read on $PROJECT_PATH via $src"
    exit 0
    ;;
  false)
    echo "MERGE GATE OFF — red-main fall-through is re-armed; re-enable only_allow_merge_if_pipeline_succeeds"
    echo "  (read live from $PROJECT_PATH via $src: Settings → Merge requests → 'Pipelines must succeed')"
    exit 1
    ;;
  *)
    blind "the project JSON from $src has no boolean only_allow_merge_if_pipeline_succeeds (got '${val:-nothing}')"
    ;;
esac
