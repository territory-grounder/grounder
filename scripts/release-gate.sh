#!/usr/bin/env bash
# release-gate.sh — the Phase-4 §5 RELEASE GATE (docs/TESTING-AND-BENCHMARK.md §5, TG-5).
#
# §5 defines "done, and TG may deploy it" as SIX conditions ANDed together. They lived only as prose, so
# release readiness was a human recollection. This makes them a machine check with three honest verdicts per
# condition — and, critically, it distinguishes RED from BLOCKED:
#
#   GREEN   — the condition is verified from an artifact that exists.
#   RED     — the condition is verified and FAILS. The release is refused.
#   BLOCKED — the condition CANNOT be verified here: the evidence it needs is not a committed artifact
#             (it lives in the DB, or is produced only by an on-box eval run). NOT a pass.
#
# A MEASUREMENT NEVER FAIL-SAFES TO GREEN (the tgledger rule): a gate that cannot see must not certify.
# Overall exit: 0 only when every condition is GREEN; 1 if any condition is RED; 3 if none are RED but any is
# BLOCKED ("cannot certify" — the same exit-3 blind convention `make ledger` uses).
#
# It runs NO eval. It reads committed records and runs deterministic local checks only, so it is safe in CI
# and on a developer box, and it never needs the model gateway.
#
# Usage: scripts/release-gate.sh            # full report, exit per the rule above
#        RELEASE_GATE_ROOT=<dir> ...        # (drill) treat <dir> as the repo root
#        RELEASE_GATE_NO_GO=1 ...           # (drill) skip the two limbs that need the Go module. They report
#                                           #   BLOCKED, never GREEN, so the flag can never manufacture a pass.
set -uo pipefail
cd "${RELEASE_GATE_ROOT:-$(dirname "$0")/..}"

red=0
blocked=0
green=0

# say <state> <condition> <detail>
say() {
  case "$1" in
    GREEN)   green=$((green + 1)) ;;
    RED)     red=$((red + 1)) ;;
    BLOCKED) blocked=$((blocked + 1)) ;;
  esac
  printf '  [%-7s] %s\n            %s\n' "$1" "$2" "$3"
}

echo "== Phase-4 §5 release gate =="

# ---------------------------------------------------------------------------------------------------------
# §5.1 — Regression corpus passes, and no >20pt regression-vs-holdout gap on the most recent sealed-holdout run.
# ---------------------------------------------------------------------------------------------------------
latest_change="$(ls -d eval/history/*-change-* 2>/dev/null | sort | tail -1)"
if [ -z "$latest_change" ] || [ ! -f "$latest_change/verdict.json" ]; then
  say BLOCKED "§5.1 regression corpus" "no committed change-gate verdict under eval/history/ — nothing to read"
else
  verdict_state="$(python3 - "$latest_change/verdict.json" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception as e:
    print("UNREADABLE", e); raise SystemExit
outcome = str(d.get("outcome", "")).lower()
# A record's own outcome is the authority. `inconclusive` is NOT a pass for a RELEASE (it is accepted for a
# merge under the under-powered-sample rule, TG-500) — a release may not rest on a measurement that could not
# discriminate, so it reads as BLOCKED, never GREEN.
if d.get("overall_pass") is True and outcome in ("pass", "ok", ""):
    print("PASS", d.get("overall_candidate"), d.get("overall_delta"))
elif outcome in ("inconclusive", "qualified-inconclusive"):
    print("INCONCLUSIVE", d.get("overall_candidate"), d.get("overall_delta"))
else:
    print("FAIL", outcome, d.get("overall_candidate"))
PY
)"
  case "$verdict_state" in
    PASS*)         say GREEN   "§5.1 regression corpus" "$latest_change: $verdict_state" ;;
    INCONCLUSIVE*) say BLOCKED "§5.1 regression corpus" "$latest_change is INCONCLUSIVE — a release may not rest on a measurement that could not discriminate" ;;
    UNREADABLE*)   say RED     "§5.1 regression corpus" "$latest_change/verdict.json is unreadable: $verdict_state" ;;
    *)             say RED     "§5.1 regression corpus" "$latest_change: $verdict_state" ;;
  esac
fi

holdout_dir="$(ls -d eval/history/*-holdout-* 2>/dev/null | sort | tail -1)"
if [ -n "$holdout_dir" ] && [ -f "$holdout_dir/regression.json" ] && [ -f "$holdout_dir/holdout.json" ]; then
  # DELEGATE, never reimplement. The §1.3 gap is not a subtraction: eval/gate normalizes each scorecard
  # (a pre-v2 card whose zero-proposal run dropped a dimension reads ~8.6pt high) and refuses a degraded
  # arm outright. Recomputing that here would fork the measurement — two implementations of one number is
  # how a gate and its subject start disagreeing. tools/evalgate is the authority; we read its exit code.
  if [ -n "${RELEASE_GATE_NO_GO:-}" ]; then
    say BLOCKED "§5.1 holdout gap (>20pt fails)" "holdout record present; gap check skipped (RELEASE_GATE_NO_GO)"
  else
    hg_out="$(go run ./tools/evalgate --baseline eval/baseline-scorecard.json \
      --candidate "$holdout_dir/regression.json" --holdout "$holdout_dir/holdout.json" 2>&1)"
    case "$?" in
      0) say GREEN   "§5.1 holdout gap (>20pt fails)" "$(printf '%s' "$hg_out" | tail -1) [$holdout_dir]" ;;
      1) say RED     "§5.1 holdout gap (>20pt fails)" "$(printf '%s' "$hg_out" | tail -1) [$holdout_dir]" ;;
      *) say BLOCKED "§5.1 holdout gap (>20pt fails)" "tools/evalgate could not evaluate the record: $(printf '%s' "$hg_out" | tail -1)" ;;
    esac
  fi
elif [ -n "$holdout_dir" ]; then
  say BLOCKED "§5.1 holdout gap (>20pt fails)" "$holdout_dir exists but carries no regression.json + holdout.json pair — nothing to compare"
else
  say BLOCKED "§5.1 holdout gap (>20pt fails)" "no sealed-holdout run is committed under eval/history/ (produced by 'make eval-holdout') — the overfitting check cannot be evaluated"
fi

# ---------------------------------------------------------------------------------------------------------
# §5.2 — Whole-trajectory VISR at/above the release floor per stratum and per tenant + the 5-mode experiment.
# ---------------------------------------------------------------------------------------------------------
say BLOCKED "§5.2 VISR + 5-mode experiment" "OWNER-DEFERRED [R5] 2026-08-25 (graduation plan): the 200-300-incident replay corpus is not built; revisit post-cutover. Stays BLOCKED — a deferral is a ruling about WHEN, never evidence, so it can neither certify nor be silently dropped from this report"

# ---------------------------------------------------------------------------------------------------------
# §5.3 — Boundary-coverage map: >=1 adversarial test per declared trust boundary, all green.
# ---------------------------------------------------------------------------------------------------------
if [ -n "${RELEASE_GATE_NO_GO:-}" ]; then
  say BLOCKED "§5.3 boundary-coverage map" "skipped (RELEASE_GATE_NO_GO): the drill exercises the decision logic, not the Go module"
elif out="$(go test ./core/fuzzcorpus/ -run 'Boundary|NoFuzzTarget' -count=1 2>&1)"; then
  say GREEN "§5.3 boundary-coverage map" "core/fuzzcorpus registry + inverse scanner pass (every declared boundary wired; no fuzz target outside the map)"
else
  say RED "§5.3 boundary-coverage map" "$(printf '%s' "$out" | tail -1)"
fi

# ---------------------------------------------------------------------------------------------------------
# §5.4 — Judge calibration floors (TPR/TNR >= 0.70 at the LOWER bound) hold, read from the newest COMMITTED
#        judgecal artifact (cmd/judgecal --json-out → eval/history/<date>-judgecal/calibration.json). The
#        gate still reads no live DB: the record travels in git, aged. The judge-DEATH half of §5.4 is a
#        LIVE property and stays with the live dead-man (judge-liveness schedule halts graduation;
#        alert.rules.yml) — a committed file cannot certify liveness and must not pretend to.
# ---------------------------------------------------------------------------------------------------------
CAL_MAX_AGE_DAYS="${RELEASE_GATE_CAL_MAX_AGE_DAYS:-14}"
# The glob is ANCHORED to the exact <date>-judgecal convention: an unanchored *-judgecal* let any
# name merely containing the token (a -backup, a -scratch decoy) win the lexical sort and certify
# or refuse off the wrong record (review finding 2026-08-25, demonstrated in both directions).
cal_dir="$(ls -d eval/history/????-??-??-judgecal 2>/dev/null | sort | tail -1)"
cal="${cal_dir:+$cal_dir/calibration.json}"
if ! command -v python3 >/dev/null 2>&1; then
  echo "TOOLING ERROR: python3 is required to read the judgecal record"; exit 2
fi
if [ -z "$cal" ] || [ ! -f "$cal" ]; then
  say BLOCKED "§5.4 judge calibration floors" "no committed judgecal record under eval/history/ (produced by 'go run ./cmd/judgecal -json-out eval/history/<date>-judgecal/calibration.json') — the floors cannot be evaluated"
else
  cal_out="$(python3 - "$cal" "$CAL_MAX_AGE_DAYS" <<'PYEOF'
import datetime as dt, json, sys
try:
    d = json.load(open(sys.argv[1]))
    tpr, tnr = d["tpr"], d["tnr"]
    n = int(d.get("confusion", {}).get("tp", 0)) + int(d.get("confusion", {}).get("fp", 0)) \
        + int(d.get("confusion", {}).get("tn", 0)) + int(d.get("confusion", {}).get("fn", 0))
    gen = dt.datetime.fromisoformat(d["generated_at"].replace("Z", "+00:00"))
    # Seconds, not timedelta.days: integer-floor days made the documented 14d bound behave as ~15d
    # (a 14.99d-old record read as 14 and passed) — review finding 2026-08-25.
    age_s = (dt.datetime.now(dt.timezone.utc) - gen).total_seconds()
    age_disp = f"{age_s / 86400:.1f}d"
    summary = (f"TPR {tpr.get('Value', 0):.3f} (lo {tpr.get('Lo', 0):.3f}) TNR {tnr.get('Value', 0):.3f} "
               f"(lo {tnr.get('Lo', 0):.3f}) on n={n}, bar {d.get('bar')}, generated {d['generated_at']}")
    if n == 0:
        print("UNCAL|" + (d.get("bar_reason") or "no labelled items") + f" [{sys.argv[1]}]")
    elif age_s > float(sys.argv[2]) * 86400:
        print(f"STALE|record is {age_disp} old (bound {sys.argv[2]}d) — {summary}")
    elif d.get("meets_bar"):
        print("PASS|" + summary)
    else:
        print("FAIL|" + (d.get("bar_reason") or "floors not met") + " — " + summary)
except Exception as e:  # noqa: BLE001 — an unreadable record must refuse, whatever broke
    print(f"UNREADABLE|{e}")
PYEOF
)"
  cal_state="${cal_out%%|*}"; cal_msg="${cal_out#*|}"
  case "$cal_state" in
    PASS)       say GREEN   "§5.4 judge calibration floors" "$cal_msg [$cal]" ;;
    FAIL)       say RED     "§5.4 judge calibration floors" "$cal_msg [$cal]" ;;
    UNCAL)      say BLOCKED "§5.4 judge calibration floors" "judge UNCALIBRATED (n=0) — not the same as failing: $cal_msg" ;;
    STALE)      say BLOCKED "§5.4 judge calibration floors" "$cal_msg — re-run judgecal on-box and commit the fresh record" ;;
    *)          say RED     "§5.4 judge calibration floors" "record unreadable ($cal_msg) [$cal] — an unreadable record refuses, never certifies" ;;
  esac
fi

# ---------------------------------------------------------------------------------------------------------
# §5.5 — Provenance: every generated artifact carries generated_at + source hash + coverage scope, and the
#        published contracts match the generator (no hand-written numbers).
# ---------------------------------------------------------------------------------------------------------
prov_fail=""
for f in docs/contracts/openapi.yaml docs/contracts/asyncapi.yaml; do
  [ -f "$f" ] || { prov_fail="$f missing"; break; }
  for field in generated_at source_hash coverage_scope; do
    grep -q "$field:" "$f" || prov_fail="$f has no $field"
  done
done
if [ -n "$prov_fail" ]; then
  say RED "§5.5 provenance" "$prov_fail"
elif [ -n "${RELEASE_GATE_NO_GO:-}" ]; then
  say BLOCKED "§5.5 provenance" "contract fields present; generator comparison skipped (RELEASE_GATE_NO_GO)"
elif out="$(go run ./tools/gencontracts/cmd/gencontracts -check 2>&1)"; then
  say GREEN "§5.5 provenance" "$(printf '%s' "$out" | tail -1)"
else
  say RED "§5.5 provenance" "$(printf '%s' "$out" | tail -1)"
fi

# ---------------------------------------------------------------------------------------------------------
# §5.6 — The synthetic canary is green BUT IS NOT COUNTED AS AUTHORITY. Reported, never a certifying limb.
# ---------------------------------------------------------------------------------------------------------
printf '  [%-7s] %s\n            %s\n' "ADVISORY" "§5.6 synthetic canary" \
  "reported only — §5 states the canary is never the deployment authority, so it can neither certify nor refuse here"

# ---------------------------------------------------------------------------------------------------------
# Verdict. A gate that cannot report "nothing to check" is not a gate: the counts are always printed.
# ---------------------------------------------------------------------------------------------------------
echo
echo "  conditions: $green GREEN, $red RED, $blocked BLOCKED (canary advisory, never counted)"
if [ "$red" -gt 0 ]; then
  echo "release gate: REFUSED — $red condition(s) FAIL" >&2
  exit 1
fi
if [ "$blocked" -gt 0 ]; then
  echo "release gate: CANNOT CERTIFY — $blocked condition(s) have no readable evidence (exit 3). A measurement that cannot see must not certify." >&2
  exit 3
fi
echo "release gate: PASS — every §5 condition verified GREEN"
