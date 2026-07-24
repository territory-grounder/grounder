#!/usr/bin/env bash
# run.sh — headless driver for the shadow head-to-head benchmark (shadowbench v2).
#
# WHAT IT DOES (fully scripted — no Claude workflow in the loop)
#   1. extract_predecessor.py --json    → the predecessor's real agentic incidents for --date (LOCAL, read-only).
#   2. TG_FORMAT=jsononly extract_tg.sh  → TG's real triage rows for the shadow window (SSH, read-only).
#   3. Align incidents by SUBJECT-host + loose time (default ±12h).
#   4. For each aligned PAIR (and each SINGLE-SIDED incident, tagged) → judge.py (blind, unified rubric).
#   5. APPEND one verdict line per judged incident to scorecard.jsonl (dedup by date+incident; idempotent-ish).
#   6. Print a ROLLING aggregate over the whole scorecard: predecessor vs TG mean per dimension, n, coverage.
#
# It produces a REAL number as incidents accumulate. With sparse data it honestly reports single-sided
# entries and "no aligned pairs yet" rather than a fabricated head-to-head.
#
# SAFETY: read-only against both systems (SELECT-only / sqlite ro; no mutation). Credentials are never
# printed — judge.py + the extractors redact; the LiteLLM master key stays on the box.
#
# USAGE
#   ./run.sh                                   # date=today, window=2026-07-18 17:20 UTC onward
#   DATE=2026-07-18 ./run.sh
#   SHADOW_FROM='2026-07-18 00:00:00+00' DATE=2026-07-18 ./run.sh
#   SCORECARD=/path/to/scorecard.jsonl ./run.sh   # e.g. a throwaway file for testing
#   ALIGN_HOURS=24 ./run.sh                     # widen the loose-time alignment window
#   DRY_JUDGE=1 ./run.sh                        # build prompts but do NOT call LiteLLM (offline check)
set -euo pipefail

SB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export DATE="${DATE:-$(date -u +%F)}"
export SHADOW_FROM="${SHADOW_FROM:-2026-07-18 17:20:00+00}"
export ALIGN_HOURS="${ALIGN_HOURS:-12}"
export SCORECARD="${SCORECARD:-$SB_DIR/scorecard.jsonl}"
export DRY_JUDGE="${DRY_JUDGE:-0}"

# passed through to judge.py / extract_tg.sh
export TG_HOST="${TG_HOST:-dc1tg01}"
export SSH_KEY="${SSH_KEY:-$HOME/.ssh/one_key}"
export SSH_USER="${SSH_USER:-root}"
export ENV_PATH="${ENV_PATH:-/srv/tg/deploy/.env}"
export MODEL="${MODEL:-primary}"

WORK="$(mktemp -d)"
trap 'rm -f "$WORK"/*.json 2>/dev/null; rmdir "$WORK" 2>/dev/null || true' EXIT
export SB_DIR WORK

echo "# shadowbench v2 — headless run  (date=$DATE  window≥$SHADOW_FROM  align=±${ALIGN_HOURS}h)"

# --- 1. predecessor (local, read-only) --------------------------------------
if ! python3 "$SB_DIR/extract_predecessor.py" --date "$DATE" --json > "$WORK/pred.json" 2>"$WORK/pred.err"; then
  echo "WARN: extract_predecessor failed (no shadow log for $DATE?):" >&2
  sed -E 's/(Token|Bearer) [A-Za-z0-9._-]+/\1 <REDACTED>/g' "$WORK/pred.err" >&2 || true
  echo '{"incidents":[]}' > "$WORK/pred.json"
fi

# --- 2. TG (SSH, read-only) -------------------------------------------------
if TG_FORMAT=jsononly "$SB_DIR/extract_tg.sh" > "$WORK/tg.json" 2>"$WORK/tg.err"; then
  :
else
  echo "WARN: extract_tg failed (box unreachable?); proceeding predecessor-single-sided." >&2
  echo '[]' > "$WORK/tg.json"
fi
# guard: ensure tg.json is a JSON array even if psql printed nothing
python3 - <<'PYGUARD'
import json, os
p = os.path.join(os.environ["WORK"], "tg.json")
try:
    txt = open(p).read().strip()
    json.loads(txt) if txt else (_ for _ in ()).throw(ValueError)
except Exception:
    open(p, "w").write("[]")
PYGUARD

# --- 3-6. align → judge → append → aggregate --------------------------------
python3 "$SB_DIR/_driver.py"
