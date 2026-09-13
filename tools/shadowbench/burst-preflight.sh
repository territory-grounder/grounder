#!/usr/bin/env bash
# burst-preflight.sh — TG-75 burst-safety preflight for the tier-3/4 outage drills (TG-73/74).
#
# A cascade is a FLOOD. Before any owner-triggered outage, assert that BOTH systems would CAPTURE a
# correlated burst — correlate-or-ingest — rather than silently suppress it into a benchmark with no data:
#   TG side:  worker up (scrape-fresh), suppression decision counter present on /metrics (the counter is
#             always-emitted, so PRESENCE is the wiring proof; tg_stage_* triples shown when traffic has
#             minted them), ingest pre-drop counter readable on the front door (it emits on first drop, so
#             "absent" = zero drops since boot — reported as such, never as a missing instrument), and the
#             alert_cluster table reachable (the TG-385 durable collapse identity a burst funnels into).
#   Pred side: gateway.db readable (sessions + session_log), and the n8n dedup lane (the LibreNMS receiver
#             workflow carries the dedup logic) ACTIVE if discoverable read-only via the n8n API. When the
#             API is not configured/reachable the verdict is UNKNOWN — honestly, never OK.
#
# READ-ONLY everywhere: /metrics + federate GET, psql SELECT, sqlite -readonly, n8n GET. No estate
# hostnames in this file — targets come from env (same conventions as outage-watch.sh / extract_tg.sh):
#   TG_HOST (required)  SSH_USER=root  SSH_KEY=~/.ssh/one_key  PG_CONTAINER=territory-grounder-postgres-1
#   GROUNDER_PORT=8081  PROM_PORT=9090  PRED_DB=$HOME/gateway-state/gateway.db
#   N8N_API_URL + N8N_API_KEY (optional; without them the n8n limb is UNKNOWN)
#   N8N_DEDUP_WORKFLOW="librenms receiver"   (case-insensitive substring of the workflow name)
# Exit: 0 all limbs PASS · 2 any FAIL · 3 no FAIL but >=1 UNKNOWN (not proven — drill discipline decides).
set -euo pipefail

TG_HOST="${TG_HOST:?set TG_HOST (TG docker host; estate names are env-only, never committed)}"
SSH_USER="${SSH_USER:-root}"; SSH_KEY="${SSH_KEY:-$HOME/.ssh/one_key}"
PG_CONTAINER="${PG_CONTAINER:-territory-grounder-postgres-1}"
GROUNDER_PORT="${GROUNDER_PORT:-8081}"; PROM_PORT="${PROM_PORT:-9090}"
PRED_DB="${PRED_DB:-$HOME/gateway-state/gateway.db}"
N8N_API_URL="${N8N_API_URL:-}"; N8N_API_KEY="${N8N_API_KEY:-}"
N8N_DEDUP_WORKFLOW="${N8N_DEDUP_WORKFLOW:-librenms receiver}"

SSH_OPTS=(-i "$SSH_KEY" -o ConnectTimeout=10 -o ServerAliveInterval=5 -o ServerAliveCountMax=2
          -o BatchMode=yes -o StrictHostKeyChecking=no)
# shellcheck disable=SC2029  # client-side expansion into the remote command is the point
STG() { timeout 45 ssh "${SSH_OPTS[@]}" "${SSH_USER}@${TG_HOST}" "$@"; }
fed() { STG "curl -fsS --max-time 8 --get 'http://127.0.0.1:${PROM_PORT}/federate' --data-urlencode 'match[]=$1'" 2>/dev/null || true; }

FAILS=0; UNKNOWNS=0
pass()    { printf '  PASS     %s\n' "$1"; }
failing() { printf '  FAIL     %s\n' "$1"; FAILS=$((FAILS + 1)); }
unknown() { printf '  UNKNOWN  %s\n' "$1"; UNKNOWNS=$((UNKNOWNS + 1)); }

echo "BURST-SAFETY PREFLIGHT  $(date -u '+%Y-%m-%d %H:%M:%S') UTC  (read-only; run BEFORE any tier-3/4 drill)"
echo "— TG side —"

# 1. Worker up, by scrape truth: prometheus reached worker:8444 recently (up==1 with a fresh sample).
UP_LINE="$(fed '{__name__="up",job="worker"}' | grep -E '^up\{' | head -1)" || true
if [ -z "$UP_LINE" ]; then
  failing "worker liveness: no up{job=\"worker\"} series from federate — worker exposition not scraped"
else
  UP_VAL="$(awk '{print $2}' <<<"$UP_LINE")"; UP_TS="$(awk '{print int($3 / 1000)}' <<<"$UP_LINE")"
  AGE=$(( $(date -u +%s) - UP_TS ))
  if [ "${UP_VAL%%.*}" = "1" ] && [ "$AGE" -lt 180 ]; then
    pass "worker up (up{job=\"worker\"}=1, sample ${AGE}s old)"
  else
    failing "worker liveness: up=${UP_VAL} sample ${AGE}s old — worker down or scrape stale"
  fi
fi

# 2. Suppression posture: the decision counter is ALWAYS emitted (even 0), so presence proves the gate is
#    wired to the exposition; its by-outcome split + the tg_stage_* triples ride along when traffic exists.
SUP="$(fed '{__name__=~"tg_suppression_decisions.*",job="worker"}' | grep -E '^tg_suppression' )" || true
if grep -q '^tg_suppression_decisions_total' <<<"${SUP:-}"; then
  pass "suppression decision counter present: $(grep '^tg_suppression_decisions_total' <<<"$SUP" | awk '{print "total="$2}' | head -1)"
  grep '^tg_suppression_decisions_by_outcome_total' <<<"$SUP" | sed -E 's/^[^{]*\{[^}]*outcome="([^"]*)"[^}]*\} ([0-9.]+).*/           outcome \1=\2/' || true
else
  failing "suppression decision counter ABSENT (tg_suppression_decisions_total is always-emitted — its absence means the suppression plane is not observable)"
fi
STAGES="$(fed '{__name__=~"tg_stage_.*",job="worker"}' | grep -cE '^tg_stage_' )" || STAGES=0
if [ "${STAGES:-0}" -gt 0 ]; then
  printf '           stage triples: %s series live (traffic since worker boot)\n' "$STAGES"
else
  printf '           stage triples: none yet (emit on first traffic since worker boot — idle, not dead; suppression counter above is the wiring proof)\n'
fi

# 3. Front-door admission: grounder /metrics readable; predrop counter value or its defined-absent state.
GM="$(STG "curl -fsS --max-time 8 http://127.0.0.1:${GROUNDER_PORT}/metrics" 2>/dev/null)" || GM=""
if grep -qE '^tg_up[{ ]' <<<"${GM:-}"; then
  PD="$(grep -E '^tg_ingest_predrop_total' <<<"$GM" | awk '{s += $2} END {printf "%d", s}')" || PD=""
  if grep -qE '^tg_ingest_predrop_total' <<<"$GM"; then
    pass "front-door /metrics readable; pre-drop counter tg_ingest_predrop_total=${PD} (accepted-but-unminted deliveries visible)"
  else
    pass "front-door /metrics readable; pre-drop counter absent = ZERO drops since grounder boot (series emits on first drop — defined state, not a gap)"
  fi
else
  failing "front-door /metrics on :${GROUNDER_PORT} unreadable — admission posture unobservable"
fi

# 4. Burst-correlate landing zone: the durable cluster-identity table answers SELECT.
AC="$(STG "docker exec -i $PG_CONTAINER psql -U postgres -d grounder -tAc \"select count(*)||'|'||coalesce(max(created_at)::text,'never') from alert_cluster;\"" 2>/dev/null)" || AC=""
if [ -n "$AC" ]; then
  pass "alert_cluster reachable: ${AC%%|*} clusters, latest ${AC#*|}"
else
  failing "alert_cluster table unreachable — a correlated burst has no durable collapse identity to land in"
fi

echo "— predecessor side —"

# 5. gateway.db readable (both session tables the watcher counts).
GW="$(sqlite3 -readonly "$PRED_DB" "select (select count(*) from sessions)||'|'||(select count(*) from session_log)||'|'||coalesce((select max(started_at) from (select started_at from sessions union all select started_at from session_log)),'never');" 2>/dev/null)" || GW=""
if [ -n "$GW" ]; then
  IFS='|' read -r GS GL GMAX <<<"$GW"
  pass "gateway.db readable: sessions=$GS session_log=$GL latest=$GMAX"
else
  failing "gateway.db unreadable at $PRED_DB — predecessor capture cannot be counted"
fi

# 6. n8n dedup lane: the receiver workflow carrying the dedup must be ACTIVE. Read-only discovery; when the
#    API is not configured or answers nothing, the honest verdict is UNKNOWN — never OK.
if [ -n "$N8N_API_URL" ] && [ -n "$N8N_API_KEY" ]; then
  ACT="$(curl -fsSk --max-time 10 -H "X-N8N-API-KEY: ${N8N_API_KEY}" --get "${N8N_API_URL%/}/api/v1/workflows" --data-urlencode "active=true" 2>/dev/null)" || ACT=""
  if [ -z "$ACT" ]; then
    unknown "n8n API at \$N8N_API_URL not answering — dedup-lane posture NOT PROVEN"
  elif grep -io "\"name\":\"[^\"]*${N8N_DEDUP_WORKFLOW}[^\"]*\"" <<<"$ACT" | head -1 | grep -q .; then
    pass "n8n dedup lane ACTIVE: $(grep -io "\"name\":\"[^\"]*${N8N_DEDUP_WORKFLOW}[^\"]*\"" <<<"$ACT" | head -1 | cut -d'"' -f4)"
  else
    ALL="$(curl -fsSk --max-time 10 -H "X-N8N-API-KEY: ${N8N_API_KEY}" "${N8N_API_URL%/}/api/v1/workflows" 2>/dev/null)" || ALL=""
    if grep -qio "\"name\":\"[^\"]*${N8N_DEDUP_WORKFLOW}[^\"]*\"" <<<"${ALL:-}"; then
      failing "n8n dedup workflow matching '${N8N_DEDUP_WORKFLOW}' exists but is INACTIVE — a burst would hit a dead receiver"
    else
      unknown "no n8n workflow matching '${N8N_DEDUP_WORKFLOW}' discoverable — dedup-lane posture NOT PROVEN"
    fi
  fi
else
  unknown "N8N_API_URL/N8N_API_KEY not set — n8n dedup lane not discoverable read-only; posture NOT PROVEN"
fi

echo "—"
if [ "$FAILS" -gt 0 ]; then
  echo "VERDICT: NOT READY — $FAILS failing limb(s). A tier-3/4 drill against this posture risks a burst one side never records."
  exit 2
elif [ "$UNKNOWNS" -gt 0 ]; then
  echo "VERDICT: NOT PROVEN — 0 failures but $UNKNOWNS UNKNOWN limb(s). Do not read this as OK."
  exit 3
fi
echo "VERDICT: READY — both systems observable + capture lanes reachable."
exit 0
