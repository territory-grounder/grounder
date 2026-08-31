#!/usr/bin/env bash
# THE NIGHTLY TREND-WATCH DEAD-MAN (TG-493 follow-up, 2026-08-14). The 03:40 nightly eval-drift had
# been lock-starved (exit 75) EVERY night since 2026-08-09 and nothing noticed for five days: the
# job's absence was silent — no artifact simply meant no line in anyone's log. Absence must be a
# NAMED state (the house rule: silence hides which defect).
#
# The check: the newest eval/history/<YYYY-MM-DD>-trend-<sha>/ directory, judged by the DATE IN ITS
# NAME — deliberately NOT the file mtime, because a fresh clone/worktree stamps checkout-time on
# every path and an mtime check reads a July artifact as minutes old (this script's own first draft
# had exactly that blindness; the drill's real-tree arm caught it). The name is checkout-invariant
# and is written by the nightly itself. Bound: the newest trend date must be within MAX_AGE_DAYS
# (default 2 — today or yesterday; the nightly runs at 03:40 so a same-day artifact exists by any
# working hour, and yesterday's covers a pre-03:40 session start).
#
# Test hooks: TG_DRIFT_HISTORY_DIR overrides the scanned dir; TG_DRIFT_TODAY (YYYY-MM-DD) overrides
# "today"; TG_DRIFT_MAX_AGE_DAYS overrides the bound.
set -euo pipefail
cd "$(dirname "$0")/.."

HIST="${TG_DRIFT_HISTORY_DIR:-eval/history}"
MAX_D="${TG_DRIFT_MAX_AGE_DAYS:-2}"
TODAY="${TG_DRIFT_TODAY:-$(date +%F)}"

echo "== nightly drift dead-man =="
if [ ! -d "$HIST" ]; then
  echo "  RED: $HIST does not exist — no trend artifact has EVER landed here."
  echo "nightly-drift dead-man: FAIL"
  exit 1
fi

newest=""
for d in "$HIST"/????-??-??-trend-*/; do
  [ -d "$d" ] || continue
  base=$(basename "$d")
  date_part=${base:0:10}
  if [ -z "$newest" ] || [ "$date_part" \> "$newest" ]; then newest=$date_part; fi
done

if [ -z "$newest" ]; then
  echo "  RED: no *-trend-* artifact exists in $HIST at all — the nightly has NEVER completed"
  echo "  (or the naming moved). Repair: run 'make eval-drift' by hand in a quiet window and read"
  echo "  why the 03:40 cron is not producing (lock starvation was the 2026-08-09..14 cause;"
  echo "  the 03:30-06:00 gateway-lock reservation exists so daytime sessions cannot repeat it)."
  echo "nightly-drift dead-man: FAIL"
  exit 1
fi

# Whole days between the newest artifact's named date and today, via epoch at midnight UTC.
newest_epoch=$(date -u -d "$newest" +%s 2>/dev/null || echo 0)
today_epoch=$(date -u -d "$TODAY" +%s 2>/dev/null || echo 0)
if [ "$newest_epoch" -eq 0 ] || [ "$today_epoch" -eq 0 ]; then
  echo "  RED: could not parse dates (newest='$newest', today='$TODAY') — refusing to guess."
  echo "nightly-drift dead-man: FAIL"
  exit 1
fi
age_d=$(( (today_epoch - newest_epoch) / 86400 ))

if [ "$age_d" -gt "$MAX_D" ]; then
  echo "  RED: newest trend artifact is dated $newest — ${age_d} day(s) old (bound ${MAX_D}d)."
  echo "  The nightly eval-drift is NOT completing. Known cause class: gateway-lock starvation"
  echo "  (exit 75 at 03:40 — see TG-493). Repair: check the nightly's log for Error 75, honor the"
  echo "  03:30-06:00 lock reservation, and run 'make eval-drift' by hand once to refresh the"
  echo "  baseline. A missing nightly means drift against the committed baseline is UNMEASURED —"
  echo "  every day without it widens the blind window."
  echo "nightly-drift dead-man: FAIL"
  exit 1
fi

echo "  ok: newest trend artifact is dated $newest (${age_d} day(s) old, bound ${MAX_D}d)"
echo "nightly-drift dead-man: PASS"
