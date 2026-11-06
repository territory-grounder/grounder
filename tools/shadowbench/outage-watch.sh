#!/usr/bin/env bash
# outage-watch.sh — TG-75 v2: dual-system outage watcher for the Benchmark-Ladder cascade tiers (TG-73/74).
#
# The decision to trigger a real outage is the OWNER's risk call. This watcher is the other half of TG-75:
# during that outage, prove IN REAL TIME that BOTH agentic systems (Territory Grounder AND the predecessor
# claude-gateway) are ingesting AND processing the burst, so a stall/drop is caught DURING the outage.
#
# v2 adds, per poll, alongside v1's LibreNMS-active / TG-sessions / predecessor-sessions columns:
#   * collapse ratio  — Δalerts_ingested : Δsessions_minted (ingest_alert_occurrence counts every ACCEPTED
#     delivery, ingest_alert every new canonical ref; session_triage counts minted sessions). The pve03
#     cascade's 1.000 alerts/session fan-out (TG-385) is visible here as ratio ~1; healthy collapse reads >1.
#   * stage funnels   — per-poll deltas of the TG-380 decision-stage triples (tg_stage_{offered,eligible,
#     acted}_total for suppress/correlate/predict/gate) read from the worker exposition via the Prometheus
#     federate endpoint (worker:8444 publishes no host port; federate is the sanctioned host-local read).
#   * degraded-LibreNMS fallback — when the LibreNMS lane fails the column says DEGRADED and TG's own
#     ingest-occurrence flow becomes the activity signal. A blank/zero would read "quiet healthy"; a failed
#     instrument must never impersonate a quiet estate.
#   * zero-session burst alarm — RED when the last BURST_ZERO_POLLS polls carried >= BURST_MIN accepted
#     alert deliveries while TG minted ZERO sessions (sliding window: LibreNMS delivers in poller batches,
#     so a single-poll threshold would blink). v1's sustained-stall alarm is kept unchanged.
#
# READ-ONLY: LibreNMS API GET (or the v1 read-only mysql count), psql SELECT via SSH, /metrics GET,
# sqlite -readonly. No model-gateway calls, no mutation anywhere.
#
# Usage:
#   outage-watch.sh [DURATION_MIN=30] [INTERVAL_SEC=20] [GRACE_INTERVALS=6]      # live watch (v1 interface)
#   outage-watch.sh --replay-window <from> <to> [BUCKET_SEC=60]                  # drill: evaluate a PAST
#     window from the durable tables (ingest_alert_occurrence/ingest_alert/session_triage + gateway.db), so
#     the zero-session alarm is provable RED against a known-quiet historical window with NO injection.
#     Timestamps 'YYYY-MM-DD HH:MM[:SS][+00]', UTC assumed when no offset. LibreNMS actives are not durable
#     in TG, so replay runs the degraded lane by construction (marked in every row).
#
# Config (env; estate hostnames never live in this file):
#   TG_HOST (required)  SSH_USER=root  SSH_KEY=~/.ssh/one_key  PG_CONTAINER=territory-grounder-postgres-1
#   GROUNDER_PORT=8081  PROM_PORT=9090  PRED_DB=$HOME/gateway-state/gateway.db
#   LibreNMS lane: LIBRENMS_URL+LIBRENMS_TOKEN (API GET, primary) else NMS_HOST (v1 mysql count) else degraded.
#   BURST_MIN=10  BURST_ZERO_POLLS=5
# Exit: 4 zero-session RED fired · 3 sustained stall fired · 0 clean (replay verdict uses the same codes).
set -euo pipefail

TG_HOST="${TG_HOST:?set TG_HOST (TG docker host; estate names are env-only, never committed)}"
SSH_USER="${SSH_USER:-root}"; SSH_KEY="${SSH_KEY:-$HOME/.ssh/one_key}"
PG_CONTAINER="${PG_CONTAINER:-territory-grounder-postgres-1}"
GROUNDER_PORT="${GROUNDER_PORT:-8081}"; PROM_PORT="${PROM_PORT:-9090}"
PRED_DB="${PRED_DB:-$HOME/gateway-state/gateway.db}"
NMS_HOST="${NMS_HOST:-}"; LIBRENMS_URL="${LIBRENMS_URL:-}"; LIBRENMS_TOKEN="${LIBRENMS_TOKEN:-}"
BURST_MIN="${BURST_MIN:-10}"; W="${BURST_ZERO_POLLS:-5}"

SSH_OPTS=(-i "$SSH_KEY" -o ConnectTimeout=10 -o ServerAliveInterval=5 -o ServerAliveCountMax=2
          -o BatchMode=yes -o StrictHostKeyChecking=no)
S()   { timeout 45 ssh "${SSH_OPTS[@]}" "$@"; }
# shellcheck disable=SC2029  # client-side expansion into the remote command is the point (v1/campaign.sh pattern)
STG() { S "${SSH_USER}@${TG_HOST}" "$@"; }

# --- probes (each returns "" on failure — the caller must treat "" as instrument-down, NEVER as zero) ---
nms_active() {
  local out=""
  if [ -n "$LIBRENMS_URL" ] && [ -n "$LIBRENMS_TOKEN" ]; then
    out="$(curl -fsSk --max-time 10 -H "X-Auth-Token: ${LIBRENMS_TOKEN}" --get "${LIBRENMS_URL%/}/api/v0/alerts" \
             --data-urlencode "state=1" 2>/dev/null | grep -oE '"count": ?[0-9]+' | head -1 | tr -dc '0-9')" || out=""
  elif [ -n "$NMS_HOST" ]; then
    # shellcheck disable=SC2029,SC2016  # $DB_* expand REMOTELY from /opt/librenms/.env — single quotes intended
    out="$(S "root@${NMS_HOST}" 'set -a; . /opt/librenms/.env; set +a; sudo -u librenms MYSQL_PWD="$DB_PASSWORD" mysql -u"$DB_USERNAME" "$DB_DATABASE" -N -e "select count(*) from alerts where state=1;"' 2>/dev/null | tr -dc '0-9')" || out=""
  fi
  printf '%s' "$out"
}
tg_counts() {  # "occ|newref|sess" cumulative since $BASELINE, one round trip
  STG "docker exec -i $PG_CONTAINER psql -U postgres -d grounder -tAc \"select (select count(*) from ingest_alert_occurrence where received_at > '$BASELINE'), (select count(*) from ingest_alert where received_at > '$BASELINE'), (select count(*) from session_triage where created_at > '$BASELINE');\"" 2>/dev/null | tr -d ' ' || true
}
tg_hosts() {
  STG "docker exec -i $PG_CONTAINER psql -U postgres -d grounder -tAc \"select coalesce(string_agg(distinct host,','),'-') from session_triage where created_at > '$BASELINE';\"" 2>/dev/null || true
}
pred_new() {
  sqlite3 -readonly "$PRED_DB" "select (select count(*) from sessions where started_at > '$BASELINE') + (select count(*) from session_log where started_at > '$BASELINE');" 2>/dev/null | tr -dc '0-9' || true
}
prom_fetch() {  # raw federate text for the worker's stage triples; non-zero RC = probe failed
  STG "curl -fsS --max-time 8 --get 'http://127.0.0.1:${PROM_PORT}/federate' --data-urlencode 'match[]={__name__=~\"tg_stage_.*\",job=\"worker\"}'" 2>/dev/null
}

declare -A PREV_F CUR_F
FUNNEL_OK=0
read_funnel() {  # runs in the PARENT shell (assoc-array writes must survive); fills CUR_F, sets FUNNEL_OK
  local out fam st val
  CUR_F=()
  if out="$(prom_fetch)"; then FUNNEL_OK=1; else FUNNEL_OK=0; return 0; fi
  # An empty 200 body is legitimate: the triples emit on first traffic since worker boot (idle, not dead).
  while IFS='|' read -r fam st val; do
    [ -n "$fam" ] || continue
    printf -v val '%.0f' "$val" 2>/dev/null || val=0
    CUR_F["$fam|$st"]="$val"
  done < <(sed -nE 's/^tg_stage_(offered|eligible|acted)_total\{[^}]*stage="([a-z]+)"[^}]*\} ([0-9.eE+-]+).*/\1|\2|\3/p' <<<"$out")
}
funnel_delta() {  # compact per-poll Δo/Δe/Δa per stage; counter reset (cur<prev) re-baselines to 0
  local st fam cur prev out="" trip
  for st in suppress correlate predict gate; do
    trip=""
    for fam in offered eligible acted; do
      cur="${CUR_F[$fam|$st]:-0}"; prev="${PREV_F[$fam|$st]:-0}"
      [ "$cur" -lt "$prev" ] && prev=0
      trip="${trip}$((cur - prev))/"
    done
    out="${out}${st:0:3}${trip%/} "
  done
  printf '%s' "${out% }"
}
save_funnel() { local k; PREV_F=(); for k in "${!CUR_F[@]}"; do PREV_F[$k]="${CUR_F[$k]}"; done; }

ratio_str() { awk -v o="$1" -v s="$2" 'BEGIN{ if (s>0) printf "%.1f:1", o/s; else if (o>0) printf "INF:0"; else printf "-" }'; }

# --- shared alarm evaluator (live + replay run the SAME code path; the replay drill proves it) ---
OCC_HIST=(); SESS_HIST=(); STALL=0; STALL_ALARMS=0; ZS_ALARMS=0
alarms_step() {  # <idx> <grace> <elevated01> <d_actives> <occ_delta> <sess_delta> <sess_cum> <pred_cum> <pred_valid01>
  local i="$1" grace="$2" elev="$3" d="$4" od="$5" sd="$6" T="$7" P="$8" pv="$9"
  local j n start occ_win=0 sess_win=0
  NOTE=""
  OCC_HIST+=("$od"); SESS_HIST+=("$sd")
  n=${#OCC_HIST[@]}; start=$(( n > W ? n - W : 0 ))
  for ((j = start; j < n; j++)); do occ_win=$((occ_win + OCC_HIST[j])); sess_win=$((sess_win + SESS_HIST[j])); done
  # v1 sustained-stall: activity elevated while BOTH systems have produced nothing since baseline.
  if [ "$i" -gt "$grace" ] && [ "$elev" = 1 ]; then
    if [ "$T" -eq 0 ] && [ "$pv" = 1 ] && [ "$P" -eq 0 ]; then
      STALL=$((STALL + 1)); NOTE="STALL x$STALL (BOTH silent under +$d alerts)"
    elif [ "$T" -eq 0 ] && [ "$pv" = 1 ]; then NOTE="⚠ TG silent (pred processing)"
    elif [ "$T" -eq 0 ]; then NOTE="⚠ TG silent (pred UNKNOWN)"
    elif [ "$pv" = 1 ] && [ "$P" -eq 0 ]; then NOTE="⚠ predecessor silent (TG processing)"
    else STALL=0; NOTE="both processing ✓"; fi
  else STALL=0; fi
  if [ "$STALL" -ge 3 ]; then
    STALL_ALARMS=$((STALL_ALARMS + 1))
    ALARM_LINE="‼ SUSTAINED STALL — a system is NOT processing the burst. Investigate DURING the outage (both systems must capture it for a valid benchmark pair)."
  fi
  # v2 zero-session burst: >= BURST_MIN accepted deliveries across the last W polls, ZERO sessions minted.
  if [ "$n" -ge "$W" ] && [ "$occ_win" -ge "$BURST_MIN" ] && [ "$sess_win" -eq 0 ]; then
    ZS_ALARMS=$((ZS_ALARMS + 1))
    NOTE="${NOTE:+$NOTE }ZERO-SESSION RED (+${occ_win} alerts/${W} polls, 0 sessions)"
    ALARM_LINE="${ALARM_LINE:+$ALARM_LINE
}‼ ZERO-SESSION BURST — TG accepted ${occ_win} alert deliveries over the last ${W} polls and minted NO session. If real, TG is swallowing the burst (suppression/collapse eating the cascade): the comparable data is being lost NOW."
  fi
}
verdict_exit() { [ "$ZS_ALARMS" -gt 0 ] && exit 4; [ "$STALL_ALARMS" -gt 0 ] && exit 3; exit 0; }

hdr() { printf '%-9s %-20s %-14s %-8s %-9s %-9s %-31s %s\n' TIME "$1" 'TG-occ(new)' 'TG-sess' 'pred-sess' 'alrt:sess' 'stage-funnel Δo/Δe/Δa' note; }

# =========================== replay drill ===========================
if [ "${1:-}" = "--replay-window" ]; then
  FROM="${2:?--replay-window needs <from> <to>}"; TO="${3:?--replay-window needs <from> <to>}"; B="${4:-60}"
  TS_RE='^[0-9]{4}-[0-9]{2}-[0-9]{2}[ T][0-9]{2}:[0-9]{2}(:[0-9]{2})?([+-][0-9]{2}(:?[0-9]{2})?|Z)?$'
  [[ "$FROM" =~ $TS_RE && "$TO" =~ $TS_RE ]] || { echo "bad timestamp (want 'YYYY-MM-DD HH:MM[:SS][+00]')" >&2; exit 2; }
  FE="$(date -u -d "$FROM" +%s)"; TE="$(date -u -d "$TO" +%s)"
  [ "$TE" -gt "$FE" ] || { echo "empty window" >&2; exit 2; }
  # A dead DB lane must abort loudly — an unreadable table rendered as zeros would replay as "all quiet".
  STG "docker exec -i $PG_CONTAINER psql -U postgres -d grounder -tAc \"select 1;\"" >/dev/null 2>&1 \
    || { echo "ABORT: TG psql lane unreachable (host/container/db) — refusing to replay zeros as quiet" >&2; exit 2; }
  declare -A ROCC RSESS RPRED
  while IFS='|' read -r b c; do [ -n "$b" ] && ROCC[$b]="$c"; done < <(
    STG "docker exec -i $PG_CONTAINER psql -U postgres -d grounder -tAc \"select floor(extract(epoch from received_at)/$B)::bigint, count(*) from ingest_alert_occurrence where received_at >= '$FROM' and received_at < '$TO' group by 1;\"" | tr -d ' ')
  while IFS='|' read -r b c; do [ -n "$b" ] && RSESS[$b]="$c"; done < <(
    STG "docker exec -i $PG_CONTAINER psql -U postgres -d grounder -tAc \"select floor(extract(epoch from created_at)/$B)::bigint, count(*) from session_triage where created_at >= '$FROM' and created_at < '$TO' group by 1;\"" | tr -d ' ')
  PRED_OK=1
  predrows="$(sqlite3 -readonly "$PRED_DB" "select cast(strftime('%s',started_at) as integer)/$B, count(*) from (select started_at from sessions union all select started_at from session_log) where cast(strftime('%s',started_at) as integer) >= $FE and cast(strftime('%s',started_at) as integer) < $TE group by 1;" 2>/dev/null)" || PRED_OK=0
  while IFS='|' read -r b c; do [ -n "$b" ] && RPRED[$b]="$c"; done <<<"$predrows"
  [ "$PRED_OK" = 1 ] || echo "  ⚠ gateway.db unreadable — predecessor column UNKNOWN this replay (never a known-zero)"
  echo "OUTAGE WATCH — REPLAY  window=$FROM .. $TO UTC  bucket=${B}s  burst>=$BURST_MIN/${W} buckets"
  echo "  durable tables only (no injection). LibreNMS actives are not durable here -> DEGRADED lane by construction:"
  echo "  activity signal = TG ingest_alert_occurrence flow. Alarm logic = the SAME alarms_step live mode runs."
  hdr 'LibreNMS-active'
  T=0; P=0; OCC=0; NEW=0; i=0
  for ((b = FE / B; b * B < TE; b++)); do
    i=$((i + 1)); od="${ROCC[$b]:-0}"; sd="${RSESS[$b]:-0}"; pd="${RPRED[$b]:-0}"
    OCC=$((OCC + od)); T=$((T + sd)); P=$((P + pd))
    ALARM_LINE=""
    n=${#OCC_HIST[@]}; start=$(( n >= W-1 ? n - (W-1) : 0 )); ew=$od
    for ((j = start; j < n; j++)); do ew=$((ew + OCC_HIST[j])); done   # would-be window sum incl. this bucket
    elev=0; [ "$ew" -ge "$BURST_MIN" ] && elev=1
    alarms_step "$i" "${GRACE_INTERVALS:-0}" "$elev" "$ew" "$od" "$sd" "$T" "$P" "$PRED_OK"
    printf '%-9s %-20s %-14s %-8s %-9s %-9s %-31s %s\n' "$(date -u -d "@$((b * B))" +%H:%M:%S)" \
      "DEGRADED(replay)" "$OCC(+$od)" "$T" "$([ "$PRED_OK" = 1 ] && printf '%s' "$P" || printf '?')" "$(ratio_str "$OCC" "$T")" "n/a (not durable)" "$NOTE"
    [ -n "$ALARM_LINE" ] && printf '%s\n' "$ALARM_LINE"
  done
  echo "=== REPLAY VERDICT: $([ "$ZS_ALARMS" -gt 0 ] && echo "RED — zero-session burst alarm fired ${ZS_ALARMS}x (stall alarms: $STALL_ALARMS)" || echo "GREEN — no zero-session alarm (stall alarms: $STALL_ALARMS)") | ${OCC} deliveries, ${T} TG sessions, ${P} pred sessions ==="
  verdict_exit
fi

# =========================== live watch (v1 interface) ===========================
DUR_MIN="${1:-30}"; INT="${2:-20}"; GRACE="${3:-6}"
BASELINE="$(date -u '+%Y-%m-%d %H:%M:%S')"   # UTC moment for all "since start" counts
BASE_A="$(nms_active)"; LANE=ok; [ -z "$BASE_A" ] && { LANE=degraded; BASE_A=0; }
echo "OUTAGE WATCH v2  baseline=$BASELINE UTC  window=${DUR_MIN}min@${INT}s  grace=$GRACE  burst>=${BURST_MIN}/${W} polls"
echo "  guarantee: TG + predecessor must BOTH keep producing triages while the alert burst is elevated"
echo "  baseline active LibreNMS alerts (state=1): $BASE_A$([ "$LANE" = degraded ] && echo '  [LANE DEGRADED at start]')"
hdr 'LibreNMS-active'
read_funnel; save_funnel
N=$((DUR_MIN * 60 / INT)); peak=0; POCC=0; PSESS=0
for i in $(seq 1 "$N"); do
  A="$(nms_active)"; C="$(tg_counts)"; P="$(pred_new)"
  IFS='|' read -r OCC NEW T <<< "${C:-||}"
  read_funnel
  ALARM_LINE=""; NOTE=""
  if [ -z "$OCC" ] || [ -z "$T" ]; then
    # TG DB unreadable: an instrument failure must not feed the alarms as fake zeros.
    printf '%-9s %-20s %-14s %-8s %-9s %-9s %-31s %s\n' "$(date -u +%H:%M:%S)" "${A:-DEGRADED}" "?" "?" "${P:-?}" "-" "-" "⚠ TG psql probe failed — no data, alarms suspended this poll"
    sleep "$INT"; continue
  fi
  od=$((OCC - POCC)); sd=$((T - PSESS)); POCC=$OCC; PSESS=$T
  pv=1; [ -z "$P" ] && { pv=0; P=0; }
  if [ -n "$A" ]; then
    d=$((A - BASE_A)); [ "$A" -gt "$peak" ] && peak=$A
    elev=0; [ "$d" -gt 0 ] && elev=1
    ACOL="$A (Δ$d)"
  else
    # LibreNMS lane down: fall back to TG's own ingest flow as the activity signal, and SAY SO.
    n=${#OCC_HIST[@]}; start=$(( n >= W-1 ? n - (W-1) : 0 )); ew=$od
    for ((j = start; j < n; j++)); do ew=$((ew + OCC_HIST[j])); done
    d=$ew; elev=0; [ "$ew" -ge "$BURST_MIN" ] && elev=1
    ACOL="DEGRADED(occΔ+$ew)"
  fi
  alarms_step "$i" "$GRACE" "$elev" "$d" "$od" "$sd" "$T" "$P" "$pv"
  [ "$pv" = 0 ] && NOTE="${NOTE:+$NOTE }⚠ pred-db unreadable"
  FCOL="unavail"; [ "$FUNNEL_OK" = 1 ] && { FCOL="$(funnel_delta)"; save_funnel; }
  printf '%-9s %-20s %-14s %-8s %-9s %-9s %-31s %s\n' "$(date -u +%H:%M:%S)" "$ACOL" "$OCC($NEW)" "$T" "$P" "$(ratio_str "$OCC" "$T")" "$FCOL" "$NOTE"
  [ -n "$ALARM_LINE" ] && printf '%s\n' "$ALARM_LINE"
  sleep "$INT"
done
C="$(tg_counts)"; IFS='|' read -r OCC NEW T <<< "${C:-0|0|0}"
echo "=== SUMMARY: peak active alerts=$peak | TG ingested $OCC deliveries ($NEW new refs), minted $T sessions | predecessor $(pred_new) new sessions | alarms: zero-session=$ZS_ALARMS stall=$STALL_ALARMS ==="
echo "    TG hosts triaged since baseline: $(tg_hosts)"
echo "    (post-outage: extract the aligned pair -> tools/shadowbench/run.sh / judge.py for the Tier-N score.)"
verdict_exit
