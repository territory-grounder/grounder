#!/usr/bin/env bash
# open-regression-issue.sh — open-or-update the TG tracking issue when the scheduled eval gate detects a
# regression (TG-43). Best-effort and FAIL-SAFE: if no tracker is configured (missing YT_URL/YT_TOKEN), it
# prints and exits 0 so the pipeline is never reddened by a missing variable. The scheduled job decides the
# final pipeline status; this script only records.
#
# $1 = path to a text report (the evalgate table + verdict) to attach to the issue.
#
# Env: YT_URL (e.g. https://youtrack.example), YT_TOKEN (permanent token, masked CI var),
#      YT_PROJECT (default "TG"). Uses only the YouTrack REST API — no literal secrets.
set -uo pipefail

REPORT_FILE="${1:-}"
YT_URL="${YT_URL:-}"
YT_TOKEN="${YT_TOKEN:-}"
YT_PROJECT="${YT_PROJECT:-TG}"
MARKER="[eval-gate] scheduled regression on the on-box grounding eval"

REPORT="(no report captured)"
[ -n "$REPORT_FILE" ] && [ -f "$REPORT_FILE" ] && REPORT="$(cat "$REPORT_FILE")"
STAMP="$(date -u +%FT%TZ)"

if [ -z "$YT_URL" ] || [ -z "$YT_TOKEN" ]; then
  echo "no YouTrack tracker configured (set YT_URL + YT_TOKEN CI variables) — regression NOT filed:"
  echo "----"; echo "$REPORT"; echo "----"
  exit 0
fi

AUTH="Authorization: Bearer ${YT_TOKEN}"
JQ() { jq "$@"; }

# Find an existing UNRESOLVED issue carrying our marker (idempotent open-or-update).
EXISTING=$(curl -sk -H "$AUTH" \
  --data-urlencode "query=project: ${YT_PROJECT} #Unresolved summary: ${MARKER}" \
  --data-urlencode "fields=idReadable,summary" \
  -G "${YT_URL%/}/api/issues" 2>/dev/null | JQ -r '.[0].idReadable // empty' 2>/dev/null || true)

BODY_TEXT="Scheduled eval gate detected a regression at ${STAMP} (pipeline ${CI_PIPELINE_URL:-n/a}, commit ${CI_COMMIT_SHORT_SHA:-n/a}).

\`\`\`
${REPORT}
\`\`\`

The on-box grounding eval fell below the committed baseline (eval/baseline-scorecard.json). Investigate the last prompt/skill/model change; run \`make eval-gate\` locally to reproduce. This issue was opened automatically by the eval-gate-scheduled CI job."

if [ -n "$EXISTING" ]; then
  echo "updating existing YouTrack issue $EXISTING"
  COMMENT=$(JQ -nc --arg t "$BODY_TEXT" '{text:$t}')
  curl -sk -X POST -H "$AUTH" -H "Content-Type: application/json" \
    -d "$COMMENT" "${YT_URL%/}/api/issues/${EXISTING}/comments?fields=id" >/dev/null 2>&1 \
    && echo "commented on $EXISTING" || echo "warn: could not comment on $EXISTING"
  exit 0
fi

# Resolve the project's internal id (create needs the id, not the short name).
PROJ_ID=$(curl -sk -H "$AUTH" \
  --data-urlencode "query=${YT_PROJECT}" --data-urlencode "fields=id,shortName" \
  -G "${YT_URL%/}/api/admin/projects" 2>/dev/null \
  | JQ -r --arg p "$YT_PROJECT" '.[] | select(.shortName==$p) | .id' 2>/dev/null | head -1 || true)
if [ -z "$PROJ_ID" ]; then
  echo "warn: could not resolve YouTrack project '$YT_PROJECT' — regression NOT filed:"
  echo "$REPORT"; exit 0
fi

NEW=$(JQ -nc --arg pid "$PROJ_ID" --arg s "$MARKER ($STAMP)" --arg d "$BODY_TEXT" \
  '{project:{id:$pid}, summary:$s, description:$d}')
CREATED=$(curl -sk -X POST -H "$AUTH" -H "Content-Type: application/json" \
  -d "$NEW" "${YT_URL%/}/api/issues?fields=idReadable" 2>/dev/null | JQ -r '.idReadable // empty' 2>/dev/null || true)
if [ -n "$CREATED" ]; then
  echo "opened YouTrack issue $CREATED"
else
  echo "warn: could not open a YouTrack issue — regression report:"; echo "$REPORT"
fi
exit 0
