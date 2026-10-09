#!/usr/bin/env bash
# DRILL FOR THE NIGHTLY-DRIFT DEAD-MAN. Each arm both PASSES and REFUSES (the gate-drill house rule).
# The check judges the DATE IN THE ARTIFACT NAME, never mtime — the first draft used mtime and its
# real-tree arm read a July artifact as "0h old" in a fresh worktree (checkout-time), a dead-man
# blind in exactly the environment that needs it. These arms pin the name-based semantics.
set -euo pipefail
cd "$(dirname "$0")/.."
G=scripts/check-nightly-drift.sh
fail=0
check() { # name, want_rc, env-prefixed command...
  local name="$1" want="$2"; shift 2
  local out rc=0
  out="$("$@" 2>&1)" || rc=$?
  if [ "$rc" -eq "$want" ]; then
    echo "  ok: $name (rc=$rc)"
  else
    echo "  FAIL: $name — want rc=$want got rc=$rc"
    printf '%s\n' "$out" | sed 's/^/      /'
    fail=1
  fi
}

echo "== nightly-drift dead-man drill =="
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT

# Arm 1: a same-day trend artifact passes.
mkdir -p "$T/fresh/2026-08-14-trend-abc123"
check "a same-day trend artifact passes" 0 \
  env TG_DRIFT_HISTORY_DIR="$T/fresh" TG_DRIFT_TODAY=2026-08-14 bash "$G"

# Arm 2: a 6-day-old artifact REFUSES, naming the age (the real 08-09..14 starvation shape).
mkdir -p "$T/stale/2026-08-08-trend-def456"
check "a 6-day-old trend artifact REFUSES" 1 \
  env TG_DRIFT_HISTORY_DIR="$T/stale" TG_DRIFT_TODAY=2026-08-14 bash "$G"

# Arm 3: NO trend artifact at all REFUSES (absence is named, never silent); change dirs don't count.
mkdir -p "$T/empty/2026-08-14-change-zzz"
check "no trend artifact (only change dirs) REFUSES" 1 \
  env TG_DRIFT_HISTORY_DIR="$T/empty" TG_DRIFT_TODAY=2026-08-14 bash "$G"

# Arm 4: a missing history dir REFUSES.
check "a missing history dir REFUSES" 1 \
  env TG_DRIFT_HISTORY_DIR="$T/nonexistent" bash "$G"

# Arm 5: the bound is honored both directions (2 days old: passes at bound 2, refuses at bound 1).
mkdir -p "$T/edge/2026-08-12-trend-eee789"
check "2-days-old under the 2d bound passes" 0 \
  env TG_DRIFT_HISTORY_DIR="$T/edge" TG_DRIFT_TODAY=2026-08-14 bash "$G"
check "2-days-old under a 1d bound REFUSES" 1 \
  env TG_DRIFT_HISTORY_DIR="$T/edge" TG_DRIFT_TODAY=2026-08-14 TG_DRIFT_MAX_AGE_DAYS=1 bash "$G"

# Arm 6: the NEWEST name wins (a stale dir beside a fresh one must not red).
mkdir -p "$T/mixed/2026-07-30-trend-old111" "$T/mixed/2026-08-14-trend-new222"
check "newest-by-name wins over a stale sibling" 0 \
  env TG_DRIFT_HISTORY_DIR="$T/mixed" TG_DRIFT_TODAY=2026-08-14 bash "$G"

# Arm 7: an unparseable date REFUSES rather than guessing.
mkdir -p "$T/garbled/9999-99-99-trend-bad000"
check "an unparseable newest date REFUSES" 1 \
  env TG_DRIFT_HISTORY_DIR="$T/garbled" TG_DRIFT_TODAY=2026-08-14 bash "$G"

if [ "$fail" -ne 0 ]; then echo "nightly-drift dead-man drill: FAIL"; exit 1; fi
echo "nightly-drift dead-man drill: PASS (8 arms)"
