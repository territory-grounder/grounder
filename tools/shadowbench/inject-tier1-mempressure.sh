#!/usr/bin/env bash
# inject-tier1-mempressure.sh — Benchmark-Ladder Tier 1 fault injector (memory pressure).
#
# Companion to inject-tier1-diskfill.sh. Raises a monitored host's memory usage into LibreNMS alert rule 21's
# window ("Linux High Memory Usage, >= 90% in use", warning). Verified rule 21 is UNBOUNDED above 90% (unlike
# disk rule 22's [90,95) window), so we target ~91% with margin and need no upper guard. Rules 21 and 22 both
# route to transports 4 (predecessor) AND 7 (territory-grounder), so BOTH agentic systems ingest the alert —
# the head-to-head pair. LibreNMS reads memory via SNMP (~5-min poll), so the alert fires at the next poll
# after the fill, not instantly — run tools/shadowbench/outage-watch.sh alongside.
#
# ROBUST METRIC: the fault allocates ANONYMOUS memory (a python bytearray, all pages touched → resident RSS),
# NOT a tmpfs/`/dev/shm` file. tmpfs pages are accounted under "Cached", so a shm fill would NOT register as
# "used" if LibreNMS deducts cache; anonymous RSS counts as used under every mem metric (total-free,
# total-available, and real-used = total-free-buffers-cached). The hog HOLDS for HOLD_SECS then EXITS on its
# own (auto-release) — an OOM safety net so the host self-recovers even if SSH is lost before --clean.
#
# Usage:
#   inject-tier1-mempressure.sh [HOST] [TARGET_PCT] [HOLD_SECS]   inject (default librespeed01, 91%, 720s)
#   inject-tier1-mempressure.sh --dry-run [HOST] [PCT]           compute + print the fill, allocate NOTHING
#   inject-tier1-mempressure.sh --clean [HOST]                   kill the hog now, restore baseline
#
# Read-only until the allocation line; --dry-run and --clean never contend with a live triage/eval.
set -u
KEY="${SSH_KEY:-$HOME/.ssh/one_key}"
PIDFILE=/var/tmp/tg-tier1-mem.pid        # fixed path so --clean is deterministic
HOGPY=/var/tmp/tg-tier1-mem-hog.py
PY_OVERHEAD=$((24 * 1024 * 1024))        # ~24MB python interpreter RSS on top of the bytearray
MIN_FREE=$((64 * 1024 * 1024))           # refuse if <64MB available would remain after the fill (OOM guard)
MODE=inject
case "${1:-}" in --dry-run) MODE=dry; shift;; --clean) MODE=clean; shift;; esac
HOST="${1:-dc1librespeed01}"
TARGET="${2:-91}"
HOLD="${3:-720}"
S(){ ssh -i "$KEY" -o ConnectTimeout=12 -o StrictHostKeyChecking=no "root@$HOST" "$@"; }

if [ "$MODE" = clean ]; then
  echo "TIER1 MEM CLEAN on $HOST: killing hog + removing $PIDFILE"
  S "if [ -f '$PIDFILE' ]; then kill \$(cat '$PIDFILE') 2>/dev/null; rm -f '$PIDFILE'; fi; rm -f '$HOGPY'; \
     free -b | awk '/Mem:/{printf \"  restored: real-used=%d%%\n\", (\$2-\$7)*100/\$2}'"
  echo "  (the alert clears at the next LibreNMS memory poll ~5min)"
  exit 0
fi

# Guard the target: rule 21 is >=90% unbounded; stay in [90,96] with margin (avoid needless OOM risk).
if ! [ "$TARGET" -ge 90 ] 2>/dev/null || [ "$TARGET" -gt 96 ]; then
  echo "refusing TARGET_PCT=$TARGET — rule 21 fires at >=90%; use 90..96 (default 91)"; exit 2
fi

# Read memory geometry (bytes) at runtime. TOTAL and AVAILABLE are what LibreNMS' SNMP mem poll derives its
# perc from; we size the anonymous fill against the STRICTEST metric (total-available) so the alert fires
# whichever formula LibreNMS uses, then floor the remaining available memory to protect the host.
read -r TOTAL USED AVAIL < <(S "free -b | awk '/Mem:/{print \$2, \$3, \$7}'" 2>/dev/null)
if [ -z "${TOTAL:-}" ] || [ -z "${AVAIL:-}" ]; then echo "could not read free on $HOST (unreachable?)"; exit 3; fi
REAL_USED=$(( TOTAL - AVAIL ))                       # non-reclaimable used (the (total-available) metric)
TARGET_USED=$(( TOTAL * TARGET / 100 ))
FILL_BYTES=$(( TARGET_USED - REAL_USED - PY_OVERHEAD ))
AVAIL_AFTER=$(( AVAIL - FILL_BYTES - PY_OVERHEAD ))
hn(){ awk -v b="$1" 'BEGIN{printf "%.0fMB", b/1048576}'; }
echo "TIER1 mem-pressure on $HOST"
echo "  now:     total=$(hn $TOTAL) real-used=$(hn $REAL_USED) ($(( REAL_USED*100/TOTAL ))%) available=$(hn $AVAIL)"
echo "  target:  ${TARGET}% -> anon-fill $(hn $FILL_BYTES) (+~$(hn $PY_OVERHEAD) py)  => available-after=$(hn $AVAIL_AFTER)"

if [ "$FILL_BYTES" -le 0 ]; then echo "  already at/above ${TARGET}% real-used — nothing to do"; exit 0; fi
if [ "$AVAIL_AFTER" -lt "$MIN_FREE" ]; then echo "  refusing: <$(hn $MIN_FREE) available after fill would risk OOM"; exit 4; fi
if [ "$MODE" = dry ]; then echo "  [dry-run] no allocation performed."; exit 0; fi

# Push a tiny, static hog (quoted heredoc — no shell expansion) then run it detached; it holds then auto-exits.
S "cat > '$HOGPY' <<'PY'
import sys, time
n = int(sys.argv[1]); hold = int(sys.argv[2])
buf = bytearray(n)              # zero-filled => every page touched => resident anonymous RSS
for i in range(0, n, 4096):     # belt-and-suspenders: fault each page in
    buf[i] = 1
time.sleep(hold)                # hold the pressure, then release on exit (auto-recovery safety net)
PY
setsid nohup python3 '$HOGPY' '$FILL_BYTES' '$HOLD' >/dev/null 2>&1 &
echo \$! > '$PIDFILE'
sleep 2
free -b | awk '/Mem:/{printf \"  now: real-used=%d%% available=%dMB (hog pid \", (\$2-\$7)*100/\$2, \$7/1048576}'
cat '$PIDFILE' | tr -d '\n'; echo ')'"
echo "  INJECTED (auto-releases in ${HOLD}s). Start:  tools/shadowbench/outage-watch.sh 20 15"
echo "  Alert fires at next SNMP memory poll (~5min). Clean up early:  $0 --clean $HOST"
