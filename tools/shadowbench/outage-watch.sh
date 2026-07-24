#!/usr/bin/env bash
# outage-watch.sh — TG-75 observability guarantee for the Benchmark-Ladder cascade tiers (TG-73/TG-74).
#
# The decision to trigger a real outage (e.g. restart a Proxmox host or a switch) is the OWNER's risk call.
# This watcher's job is the other half: during that outage, prove — IN REAL TIME — that BOTH agentic systems
# (Territory Grounder AND the predecessor claude-gateway) are actively INGESTING and PROCESSING the alert burst,
# so a stall/drop is caught DURING the outage, not discovered afterward when the comparable data is already lost.
#
# It is READ-ONLY: it queries LibreNMS active alerts (as librenms), TG session_triage, and the predecessor
# gateway.db. It makes NO model-gateway calls, so it never contends with a live triage or an eval run.
#
# Usage:  tools/shadowbench/outage-watch.sh [DURATION_MIN=30] [INTERVAL_SEC=20] [GRACE_INTERVALS=6]
#   Start it JUST BEFORE (or at) the fault so the baseline captures the pre-outage state. It prints a per-interval
#   timeline (active alerts vs TG triages vs predecessor sessions) and alarms on a SUSTAINED stall.
set -u
DUR_MIN="${1:-30}"; INT="${2:-20}"; GRACE="${3:-6}"
KEY="${SSH_KEY:-$HOME/.ssh/one_key}"
S(){ ssh -i "$KEY" -o ConnectTimeout=12 -o StrictHostKeyChecking=no "$@"; }
BASELINE="$(date -u '+%Y-%m-%d %H:%M:%S')"   # UTC moment for all "since start" counts

nms_active(){ S root@dc1nms01 'sudo -u librenms bash -lc "cd /opt/librenms && set -a; . .env 2>/dev/null; set +a; MYSQL_PWD=\$DB_PASSWORD mysql -u\$DB_USERNAME \$DB_DATABASE -N -e \"select count(*) from alerts where state=1;\""' 2>/dev/null | tr -dc '0-9'; }
tg_new(){ S root@dc1tg01 "docker exec -i territory-grounder-postgres-1 psql -U postgres -d grounder -tAc \"select count(*) from session_triage where created_at > '$BASELINE';\"" 2>/dev/null | tr -dc '0-9'; }
tg_hosts(){ S root@dc1tg01 "docker exec -i territory-grounder-postgres-1 psql -U postgres -d grounder -tAc \"select coalesce(string_agg(distinct host,','),'-') from session_triage where created_at > '$BASELINE';\"" 2>/dev/null; }
pred_new(){ sqlite3 -readonly /home/tg/gateway-state/gateway.db "select (select count(*) from sessions where started_at > '$BASELINE') + (select count(*) from session_log where started_at > '$BASELINE');" 2>/dev/null | tr -dc '0-9'; }

BASE_A=$(nms_active); BASE_A=${BASE_A:-0}
echo "OUTAGE WATCH  baseline=$BASELINE UTC  window=${DUR_MIN}min@${INT}s  grace=${GRACE} intervals"
echo "  guarantee: TG + predecessor must BOTH keep producing triages while the alert burst is elevated"
echo "  baseline active LibreNMS alerts (state=1): $BASE_A"
printf '%-10s %-22s %-16s %-18s %s\n' TIME 'LibreNMS-active' 'TG-triages(new)' 'pred-sess(new)' note
N=$(( DUR_MIN*60/INT )); stall=0; peak=0
for i in $(seq 1 "$N"); do
  A=$(nms_active); T=$(tg_new); P=$(pred_new); A=${A:-0}; T=${T:-0}; P=${P:-0}
  d=$(( A - BASE_A )); [ "$A" -gt "$peak" ] && peak=$A
  note=""
  if [ "$i" -gt "$GRACE" ] && [ "$d" -gt 0 ]; then
    if   [ "$T" -eq 0 ] && [ "$P" -eq 0 ]; then stall=$((stall+1)); note="STALL x$stall (BOTH silent under +$d alerts)"
    elif [ "$T" -eq 0 ]; then note="⚠ TG silent (pred processing)"
    elif [ "$P" -eq 0 ]; then note="⚠ predecessor silent (TG processing)"
    else stall=0; note="both processing ✓"; fi
  else stall=0; fi
  printf '%-10s %-22s %-16s %-18s %s\n' "$(date -u +%H:%M:%S)" "$A (Δ$d)" "$T" "$P" "$note"
  [ "$stall" -ge 3 ] && echo "‼ SUSTAINED STALL — a system is NOT processing the burst. Investigate DURING the outage (both systems must capture it for a valid benchmark pair)."
  sleep "$INT"
done
echo "=== SUMMARY: peak active alerts=$peak | TG triaged $(tg_new) new | predecessor $(pred_new) new sessions ==="
echo "    TG hosts triaged since baseline: $(tg_hosts)"
echo "    (post-outage: extract the aligned pair -> tools/shadowbench/run.sh / judge.py for the Tier-N score.)"
