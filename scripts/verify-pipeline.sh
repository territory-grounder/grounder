#!/usr/bin/env bash
# verify-pipeline.sh — the anti-hit-and-run check: a push is not finished until its pipeline is.
#
# WHY THIS EXISTS (2026-07-31). MR pipelines were being watched and merged on green, but pushes
# straight to main were fire-and-forget: commit, push, move to the next thing. Main then
# accumulated a wall of red that nobody looked at, and the one job actually failing
# (baseline-freshness) was invisible among commits that had "succeeded" from the author's point of
# view. A red pipeline nobody reads is the same defect class as a green that means nothing.
#
# WHY IT READS THE API AND NOT `glab ci list` TEXT (TG-434, 2026-08-10). The original matched the
# commit by grepping its sha out of the CLI table. A glab upgrade dropped the sha column
# (State/IID/Ref/Created), so the grep could never match ANY commit and every invocation timed out
# to NO VERDICT — the instrument that exists to prevent unread red was itself unreadable, which is
# the exact desensitization it guards against. The API's `pipelines?sha=` is format-stable; the
# CLI's table is presentation.
#
# CONTRACT: run this after every push (main or branch). It blocks until the pipeline for that
# commit finishes, then prints the failing jobs and exits non-zero. Work is not done while it is
# red — either fix it, or state in the same breath why the red is expected and what clears it.
# Exit: 0 green · 1 red/canceled (jobs named) · 2 tooling absent · 3 still running at deadline ·
# 4 NO pipeline exists for the sha at deadline (its own state — "never started" must not read as
# "still working", TG-365).
#
# Usage:
#   scripts/verify-pipeline.sh                 # the pipeline for HEAD on the current branch
#   scripts/verify-pipeline.sh <sha>           # a specific commit (short or full)
#   TIMEOUT_MIN=25 scripts/verify-pipeline.sh  # override the wait bound (default 20 min)
set -uo pipefail
cd "$(dirname "$0")/.."

command -v glab >/dev/null || { echo "verify-pipeline: glab not installed — verify the pipeline manually before calling the work done."; exit 2; }
command -v python3 >/dev/null || { echo "verify-pipeline: python3 not installed — verify the pipeline manually before calling the work done."; exit 2; }

SHA="${1:-$(git rev-parse HEAD)}"
# The API matches the FULL sha; expand an abbreviated one from the local object store when possible.
if [ "${#SHA}" -lt 40 ]; then
  SHA="$(git rev-parse "$SHA" 2>/dev/null || printf '%s' "$SHA")"
fi
SHORT="${SHA:0:8}"
TIMEOUT_MIN="${TIMEOUT_MIN:-20}"
# TIMEOUT_S and VERIFY_POLL_S are drill hooks (verify-pipeline_test.sh): seconds-granular deadline and
# poll interval so the arms run in seconds. Operators use TIMEOUT_MIN.
deadline=$(( $(date +%s) + ${TIMEOUT_S:-$(( TIMEOUT_MIN * 60 ))} ))

echo "== verify-pipeline: waiting for the pipeline of $SHORT (max ${TIMEOUT_MIN}m) =="
status=""
pid=""
while [ "$(date +%s)" -lt "$deadline" ]; do
  # Newest pipeline FOR THIS COMMIT, by sha — a concurrent pipeline for someone else's push can
  # never be mistaken for ours. An empty answer stays "no pipeline", never a verdict.
  read -r pid status < <(glab api "projects/:fullpath/pipelines?sha=$SHA&per_page=1" 2>/dev/null | python3 -c '
import json, sys
try:
    ps = json.load(sys.stdin)
except Exception:
    ps = []
print("{} {}".format(ps[0]["id"], ps[0]["status"]) if ps else "- absent")
') || true
  case "${status:-absent}" in
    success|failed|canceled) break ;;
    *) : ;; # created/pending/running/absent — keep waiting
  esac
  sleep "${VERIFY_POLL_S:-20}"
done

case "${status:-absent}" in
  absent|-|"")
    echo "verify-pipeline: NO PIPELINE exists for $SHORT after ${TIMEOUT_MIN}m."
    echo "  Either the push never triggered one ([skip ci]? rules?) or the sha is wrong. This is NOT"
    echo "  'still running' — nothing has started. Do NOT report this work as done."
    exit 4
    ;;
  success)
    echo "verify-pipeline: PASS — pipeline $pid green for $SHORT."
    exit 0
    ;;
  failed|canceled)
    echo "verify-pipeline: $status — pipeline $pid for $SHORT. Failing jobs:"
    glab api "projects/:fullpath/pipelines/$pid/jobs?per_page=50" 2>/dev/null | python3 -c '
import json, sys
try:
    js = json.load(sys.stdin)
except Exception:
    js = []
bad = [j for j in js if j.get("status") not in ("success", "skipped", "manual")]
for j in bad:
    print("    {}: {}".format(j.get("name", "?"), j.get("status", "?")))
if not bad:
    print("    (no failing job listed — the pipeline-level status is authoritative)")
'
    echo
    echo "  A push is not finished while its pipeline is red. Fix it, or state explicitly why this red"
    echo "  is expected AND what will clear it — never leave it unexplained for the next reader."
    exit 1
    ;;
  *)
    echo "verify-pipeline: NO VERDICT within ${TIMEOUT_MIN}m for $SHORT (pipeline $pid still $status)."
    echo "  Do NOT report this work as done. Re-run with a longer TIMEOUT_MIN, or check the pipeline directly."
    exit 3
    ;;
esac
