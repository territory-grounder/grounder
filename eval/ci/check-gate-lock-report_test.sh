#!/usr/bin/env bash
# Oracle for eval-gate.sh's ran-vs-never-ran decision.
#
# THE DEFECT (observed 2026-08-06). flock exits 1 on lock timeout. The inner run's arm-integrity ABORT
# ("refusing to pool a contended/429 arm", TG-64/TG-65) ALSO exits 1. The wrapper used to treat every
# `rc == 1` with an existing lock file as a timeout, so a gate that RAN and REFUSED was announced as:
#
#   eval-gate: COULD NOT ACQUIRE THE GATEWAY LOCK ... THE GATE DID NOT RUN; this is NOT a gate failure
#
# and exited 75 (EX_TEMPFAIL, retryable). A real refusal read as "just retry", pointing the reader at
# `fuser` instead of at the saturated model gateway that actually caused it. The script's own comment
# names this exact confusion — "'the gate said no' versus 'the gate never ran' are the two answers this
# repo has most often confused" — which is what makes getting it wrong here worth a gate of its own.
#
# Both cases below are real executions of the real script. No gateway, no box: the inner run exits at the
# TG_EVAL_SELFTEST_INNER_RC hook, which sits immediately after the marker stamp.
set -uo pipefail
cd "$(dirname "$0")/../.."

GATE="eval/eval-gate.sh"
fail=0
say_fail() { echo "  FAIL: $*"; fail=1; }

echo "== eval-gate lock-report oracle =="

if [ ! -f "$GATE" ]; then
  echo "  FAIL: $GATE does not exist"
  echo "gate-lock-report: FAIL"
  exit 1
fi

# ---- case 1: the inner run RAN and failed. Must pass the code through and say nothing about the lock.
out="$(TG_EVAL_SELFTEST_INNER_RC=1 bash "$GATE" change 2>&1)"; rc=$?
if [ "$rc" != 1 ]; then
  say_fail "an inner run that exited 1 was reported as $rc (expected the code passed through untouched)"
fi
if printf '%s' "$out" | grep -q "COULD NOT ACQUIRE THE GATEWAY LOCK"; then
  say_fail "a gate that RAN and failed was announced as a lock timeout — this is the defect"
fi
if printf '%s' "$out" | grep -q "THE GATE DID NOT RUN"; then
  say_fail "a gate that RAN was reported as never having run"
fi
if ! printf '%s' "$out" | grep -q "selftest: inner run reached"; then
  say_fail "vacuity floor: the inner run was never reached, so case 1 proves nothing"
fi

# ---- case 2: a genuine timeout. Hold the lock from another process; the inner must never start.
LOCK="${TG_EVAL_GATE_LOCK:-/tmp/tg-gateway.lock}"
HOLDLOG="$(mktemp)"
flock "$LOCK" -c 'sleep 20' >"$HOLDLOG" 2>&1 &
holder=$!
sleep 1
out2="$(TG_EVAL_LOCK_WAIT=1 TG_EVAL_SELFTEST_INNER_RC=1 bash "$GATE" change 2>&1)"; rc2=$?
kill "$holder" 2>/dev/null
wait "$holder" 2>/dev/null
rm -f "$HOLDLOG"

if [ "$rc2" != 75 ]; then
  say_fail "a genuine lock timeout exited $rc2 (expected 75, EX_TEMPFAIL — retryable and distinct from a verdict)"
fi
if ! printf '%s' "$out2" | grep -q "COULD NOT ACQUIRE THE GATEWAY LOCK"; then
  say_fail "a genuine lock timeout did not say so — the marker check must not swallow the real case"
fi
if printf '%s' "$out2" | grep -q "selftest: inner run reached"; then
  say_fail "negative control: the inner run started while the lock was held elsewhere, so case 2 is not testing a timeout"
fi

# ---- case 3 (TG-503): an ANCESTOR holds the lock — a wrapper flock'd the same file, exactly like the
# nightly cron `flock -n /tmp/tg-gateway.lock -c '... make eval-drift ...'`. Re-taking it here would block
# on our own parent and time out (the 2026-08-08..16 stall: 13 consecutive exit-75 aborts, 8-day-blind
# baseline). The gate must detect the ancestor's lock and run serialized UNDER it, reaching the inner run.
# TG_EVAL_LOCK_WAIT=3 bounds the failure: if the fix regresses, the inner flock times out in 3s, not 3600s.
LK3="$(mktemp)"
out3="$(flock -n "$LK3" -c "TG_EVAL_LOCK=$LK3 TG_EVAL_LOCK_WAIT=3 TG_EVAL_SELFTEST_INNER_RC=0 bash $GATE change" 2>&1)"; rc3=$?
rm -f "$LK3"
if [ "$rc3" != 0 ]; then
  say_fail "TG-503: the gate exited $rc3 (expected 0) when an ANCESTOR held its lock — it must run serialized under it, not deadlock"
fi
if ! printf '%s' "$out3" | grep -q "already held by an ancestor"; then
  say_fail "TG-503: the gate did not detect the ancestor-held lock (the self-deadlock guard did not fire)"
fi
if ! printf '%s' "$out3" | grep -q "selftest: inner run reached"; then
  say_fail "TG-503: the inner run was never reached under an ancestor's lock — the deadlock is not fixed"
fi
if printf '%s' "$out3" | grep -q "COULD NOT ACQUIRE THE GATEWAY LOCK"; then
  say_fail "TG-503: an ancestor-held lock was mis-reported as a timeout — the gate deadlocked against its own parent"
fi

if [ "$fail" != 0 ]; then
  echo "gate-lock-report: FAIL"
  exit 1
fi
echo "  ok — a gate that ran passes its code through; a gate that never ran exits 75 and says so"
echo "gate-lock-report: PASS"
