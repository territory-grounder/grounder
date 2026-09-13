#!/usr/bin/env bash
# inject-tier1-diskfill.sh — Benchmark-Ladder Tier 1 fault injector (disk pressure).
#
# Tier 1 raises `/` usage on a monitored host into LibreNMS alert rule 22's BOUNDED window
# ("Space on / is >= 90% and < 95% in use", warning, delay 60s). That rule routes to transports
# 4 (predecessor) AND 7 (territory-grounder), so BOTH agentic systems ingest the same disk alert —
# the head-to-head pair. It is a REVERSIBLE fault: a single fallocate'd file, removed by --clean.
#
# IMPORTANT — the window is bounded [90%, 95%): overshooting past 95% will NOT match rule 22, so the
# injector targets ~92% and refuses to exceed 94%. LibreNMS reads `/` via SNMP storage polling
# (~5-min cadence), so the alert fires at the next poll after the fill + the 60s delay, not instantly —
# start tools/shadowbench/outage-watch.sh alongside to capture the real arrival on both systems.
#
# Usage:
#   inject-tier1-diskfill.sh [HOST] [TARGET_PCT]   inject (default HOST=dc1librespeed01, TARGET_PCT=92)
#   inject-tier1-diskfill.sh --dry-run [HOST] [PCT] compute + print the fill, allocate NOTHING (safe)
#   inject-tier1-diskfill.sh --clean [HOST]         remove the fill file, restore baseline
#
# Read-only until the fallocate line; --dry-run and --clean never contend with a live triage/eval.
set -u
KEY="${SSH_KEY:-$HOME/.ssh/one_key}"
FILL=/var/tmp/tg-tier1-fill.img          # fixed path so --clean is deterministic
MODE=inject
case "${1:-}" in --dry-run) MODE=dry; shift;; --clean) MODE=clean; shift;; esac
HOST="${1:-dc1librespeed01}"
TARGET="${2:-92}"
S(){ ssh -i "$KEY" -o ConnectTimeout=12 -o StrictHostKeyChecking=no "root@$HOST" "$@"; }

if [ "$MODE" = clean ]; then
  echo "TIER1 CLEAN on $HOST: removing $FILL"
  S "rm -f '$FILL'; sync; df -h / | awk 'NR==2{print \"  restored: used=\"\$3\" use%=\"\$5}'"
  echo "  (the alert clears at the next LibreNMS storage poll ~5min)"
  exit 0
fi

# Guard the target inside the rule's bounded window with margin (rule 22 = [90,95); stay <=94).
if ! [ "$TARGET" -ge 90 ] 2>/dev/null || [ "$TARGET" -gt 94 ]; then
  echo "refusing TARGET_PCT=$TARGET — rule 22 matches only [90,95); use 90..94 (default 92)"; exit 2
fi

# Read fs geometry (bytes) + compute the fill. CRITICAL METRIC: LibreNMS storage_perc — the EXACT value
# rule 22 evaluates — is Used/SIZE, where SIZE is the full filesystem size INCLUDING root-reserved blocks.
# That is LOWER than df's own Use% (Used/(Used+Available), which excludes reserved). So we must target
# Used/SIZE, not df Use%. Verified on librespeed: df-98% == storage_perc 92% (fires); df-93% == storage_perc
# 87% (< 90% -> alert never fires). The FREE_AFTER floor is checked against real df-available.
read -r SIZE USED AVAIL USEPCT < <(S "df -B1 --output=size,used,avail,pcent / | awk 'NR==2{gsub(/%/,\"\",\$4);print \$1,\$2,\$3,\$4}'" 2>/dev/null)
if [ -z "${SIZE:-}" ] || [ -z "${USED:-}" ]; then echo "could not read df on $HOST (unreachable?)"; exit 3; fi
TARGET_USED=$(( SIZE * TARGET / 100 ))          # storage_perc = Used/SIZE (the rule-22 metric)
FILL_BYTES=$(( TARGET_USED - USED ))
FREE_AFTER=$(( AVAIL - FILL_BYTES ))            # real df-available remaining after the fill
hn(){ awk -v b="$1" 'BEGIN{printf "%.2fG", b/1073741824}'; }
echo "TIER1 disk-fill on $HOST"
echo "  now:     size=$(hn $SIZE) used=$(hn $USED) (storage_perc≈$(( USED*100/SIZE ))%, ${USEPCT}% df)"
echo "  target:  ${TARGET}% -> used=$(hn $TARGET_USED)  => fallocate $(hn $FILL_BYTES)  (free-after=$(hn $FREE_AFTER))"

if [ "$FILL_BYTES" -le 0 ]; then echo "  already at/above ${TARGET}% — nothing to do"; exit 0; fi
if [ "$FREE_AFTER" -lt 104857600 ]; then echo "  refusing: <100MB free after fill would risk the host"; exit 4; fi
if [ "$MODE" = dry ]; then echo "  [dry-run] no allocation performed."; exit 0; fi

echo "  allocating $FILL ..."
S "fallocate -l '$FILL_BYTES' '$FILL' && sync; df -h / | awk 'NR==2{print \"  now: used=\"\$3\" use%=\"\$5}'"
echo "  INJECTED. Start:  tools/shadowbench/outage-watch.sh 20 15   (watch both systems ingest)"
echo "  Alert fires at next SNMP storage poll (~5min) + 60s delay. Clean up:  $0 --clean $HOST"
