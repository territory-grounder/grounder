#!/usr/bin/env bash
# campaign.sh — TG-84 paired data-accumulation campaign orchestrator.
#
# GOAL
#   Inject a series of REAL, REVERSIBLE faults on the guinea-pigs so BOTH agentic systems triage the
#   SAME incidents, accumulating a statistically meaningful PAIRED dataset for a head-to-head comparison:
#     * TG (Territory Grounder)   — transport 7 (territory-grounder)  -> session_triage
#     * predecessor (claude-gateway) — transport 4 (Chatops-n8n)      -> mutation-shadow log
#   Both run shadow / MUTATIONS=OFF; this harness only TRIAGES — it NEVER approves/actuates a proposal.
#
# WHAT ONE INCIDENT DOES
#   1. pick (host, fault-type) from a host+type-alternating rotation (so consecutive same-(rule,host)
#      injections are far apart and LibreNMS cannot dedup/correlate them into one alert);
#   2. inject a REAL reversible fault (reuse the tier1 injectors for disk/memory; stop+restart a
#      non-critical service for the service type);
#   3. FORCE LibreNMS to detect + fire the routing rule (device:poll x2 across the rule's 60s delay —
#      device:poll runs the AlertRules evaluator, unlike legacy poller.php -h which only refreshes data;
#      then alerts.php to dispatch to transports 4 AND 7) — the sanctioned path, never poking `alerts`;
#   4. WAIT (bounded) until BOTH the predecessor shadow log AND TG session_triage show a triage for
#      this host since this incident's baseline;
#   5. record the PAIRED incident (host/fault/timestamp + both sides' triage refs) to the manifest;
#   6. CLEAN UP the fault reliably (trap + explicit clean + INDEPENDENT verify the guinea-pig is
#      restored: disk file gone, memory freed, service active) BEFORE the next incident;
#   7. PACE (>= 5 min between injections).
#   A per-incident failure still CLEANS UP and continues, logging the miss to the manifest.
#
# ROUTING (verified 2026-07-20 against the LibreNMS DB — see tools/shadowbench/README.md)
#   rule 22  "Space on / is >= 90% and < 95%"     -> alert_operation 2 -> transports 4 AND 7   (disk)
#   rule 21  "Linux High Memory Usage, >= 90%"     -> alert_operation 2 -> transports 4 AND 7   (memory)
#   rule 25  "TG-84 guinea-pig High Memory >=85%"  -> alert_operation 4 -> transports 4 AND 7   (memory, easier)
#   rule 9   "Service up/down" (http check on librespeed01) -> alert_operation 1 -> incl. 4 AND 7 (service)
#
# SAFETY (hard)
#   * Guinea-pigs ONLY (dc1librespeed01, dc1myspeed01). Reversible faults ONLY. Never powers a host.
#   * A trap on EXIT/INT/TERM/HUP/PIPE cleans the CURRENTLY-ACTIVE fault + independently proves restoration;
#     a still-degraded guinea-pig is a LOUD failure, never a silent "restored".
#   * Mutation MUST be OFF on TG (exact-series match on the gauge, fail closed) — refuses to run otherwise.
#   * Refuses if an eval-gate is running locally (model-gateway contention).
#   * READ-ONLY on both systems except the injected fault: never approves/votes/actuates a TG proposal,
#     never edits TG/predecessor config.
#   * Every remote call is `timeout`-wrapped + ServerAlive-bounded so a hung call cannot hold a fault open.
#
# USAGE
#   tools/shadowbench/campaign.sh --count 1                 # ONE incident (routing validation)
#   tools/shadowbench/campaign.sh --count 6 --resume        # 6 more, continuing the rotation phase
#   tools/shadowbench/campaign.sh --dry-run --count 8       # print the plan + preflight, inject NOTHING
#   tools/shadowbench/campaign.sh --clean                   # sweep: restore ALL guinea-pigs, verify, exit
#   FAULT_PLAN=disk,memory,disk tools/shadowbench/campaign.sh --count 3   # explicit fault sequence
#   PIN_HOST=dc1myspeed01 tools/shadowbench/campaign.sh --count 1     # pin the (quiet) host for a clean pair
#   POLL_TIMEOUT=480 PACE_SECS=300 tools/shadowbench/campaign.sh --count 8
#
# The default rotation uses DISK + MEMORY faults only (reversible, and they do NOT collide with a service
# drive). The 'service' fault (nginx crash/restore on librespeed01) is opt-in via FAULT_PLAN=service, and the
# --clean sweep skips it unless SWEEP_SERVICE=1 — both to avoid colliding with an owner's nginx graduation run.
set -u

# ---------------------------------------------------------------------------
# Configuration (all overridable; defaults match the estate)
# ---------------------------------------------------------------------------
SB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/one_key}"
TG_HOST="${TG_HOST:-dc1tg01}"
SSH_USER="${SSH_USER:-root}"
ENV_PATH="${ENV_PATH:-/srv/tg/deploy/.env}"
PG_CONTAINER="${PG_CONTAINER:-territory-grounder-postgres-1}"
GROUNDER_PORT="${GROUNDER_PORT:-8081}"
NMS_HOST="${NMS_HOST:-dc1nms01}"
SHADOW_DIR="${SHADOW_DIR:-$HOME/logs/claude-gateway/mutation-shadow}"

# Guinea-pigs (permission-free fault injection). librespeed01 also carries a LibreNMS http service check
# (service_id 41) so it is the only host that supports the 'service' fault type.
LIBRESPEED_HOST="${LIBRESPEED_HOST:-dc1librespeed01}"
MYSPEED_HOST="${MYSPEED_HOST:-dc1myspeed01}"

DISK_PCT="${DISK_PCT:-92}"           # rule 22 bounded window [90,95)
MEM_PCT="${MEM_PCT:-91}"             # rule 21 unbounded >=90 (rule 25 fires >=85)
MEM_HOLD="${MEM_HOLD:-900}"          # hog auto-releases after this many secs (OOM safety net); we clean sooner
SVC_NAME="${SVC_NAME:-nginx}"        # non-critical service to crash+restore for the 'service' fault

POLL_TIMEOUT="${POLL_TIMEOUT:-600}"  # bounded per-incident wait for BOTH sides to triage (~10 min hard cap)
POLL_INTERVAL="${POLL_INTERVAL:-30}"
PACE_SECS="${PACE_SECS:-300}"        # >= 5 min between injections (dedup/correlation + poller relief)
SKEW_SLACK="${SKEW_SLACK:-120}"      # back-date the "since" boundary to absorb clock skew
SSH_CMD_TIMEOUT="${SSH_CMD_TIMEOUT:-45}"
FORCE_POLL="${FORCE_POLL:-1}"        # force LibreNMS detection (poller.php x2 + alerts.php) vs waiting for cron

OUT_DIR="${OUT_DIR:-$SB_DIR/out}"
MANIFEST="${MANIFEST:-$OUT_DIR/campaign-manifest.jsonl}"
FAULT_PLAN="${FAULT_PLAN:-}"         # explicit "disk,memory,..." overrides the built-in fault rotation
PIN_HOST="${PIN_HOST:-}"             # if set, pin EVERY incident to this one guinea-pig (overrides host rotation)

INJECT_DISK="$SB_DIR/inject-tier1-diskfill.sh"
INJECT_MEM="$SB_DIR/inject-tier1-mempressure.sh"

# Fault artifact paths — MUST match the injectors so cleanup/verify is deterministic.
FILL_FILE="/var/tmp/tg-tier1-fill.img"
MEM_PIDFILE="/var/tmp/tg-tier1-mem.pid"
MEM_HOGPY="/var/tmp/tg-tier1-mem-hog.py"

export SSH_KEY TG_HOST SSH_USER ENV_PATH PG_CONTAINER

STAMP="$(date -u '+%Y%m%dT%H%M%SZ')"
CUR_FAULT=""; CUR_HOST=""            # the currently-armed fault, cleaned by the trap on any exit
RUN_LOG_PREFIX="[campaign $STAMP]"

# Counters for the end-of-run summary.
N_PAIRED=0; N_ONESIDED=0; N_MISS=0; N_CLEANFAIL=0; N_DONE=0

abort() { echo "ABORT: $*" >&2; exit 2; }
log()   { echo "$RUN_LOG_PREFIX $*"; }

# ---------------------------------------------------------------------------
# Bounded ssh helpers (hard timeout + server-alive so a hung call cannot block).
# ---------------------------------------------------------------------------
SB()   { timeout "$SSH_CMD_TIMEOUT" ssh -i "$SSH_KEY" -o ConnectTimeout=10 -o ServerAliveInterval=5 \
           -o ServerAliveCountMax=2 -o BatchMode=yes -o StrictHostKeyChecking=no "$@"; }
STG()  { SB "${SSH_USER}@${TG_HOST}" "$@"; }                 # -> TG Docker host
SL()   { local h="$1"; shift; SB "root@${h}" "$@"; }         # -> a guinea-pig target
SNMS() { SB "root@${NMS_HOST}" "$@"; }                       # -> LibreNMS host

# host -> short keyword for blob/ilike matching (dc1librespeed01 -> librespeed).
hkw() { echo "$1" | sed -E 's/^[a-z]+[0-9]+//; s/[0-9]+$//'; }

# ---------------------------------------------------------------------------
# Per-fault cleanup + INDEPENDENT verification. Each prints "OK <detail>" or "FAIL <detail>"
# and returns 0 only when the guinea-pig is proven restored.
# ---------------------------------------------------------------------------
clean_disk() {
  local h="$1"
  [ -x "$INJECT_DISK" ] && "$INJECT_DISK" --clean "$h" >/dev/null 2>&1
  # Independent verify — do NOT trust --clean's exit code (it exits 0 even if ssh never connected).
  if SL "$h" "test ! -e '$FILL_FILE'" 2>/dev/null; then
    local use; use="$(SL "$h" "df -P / | awk 'NR==2{printf \"%d\", \$3*100/\$2}'" 2>/dev/null | tr -dc '0-9')"
    echo "OK fill-gone storage_perc=${use:-?}%"; return 0
  fi
  echo "FAIL $FILL_FILE STILL PRESENT on $h — DISK MAY STILL BE FILLED (manual: ssh root@$h 'rm -f $FILL_FILE')"
  return 1
}
clean_mem() {
  local h="$1"
  [ -x "$INJECT_MEM" ] && "$INJECT_MEM" --clean "$h" >/dev/null 2>&1
  # Independent verify: the pidfile is gone AND real-used memory dropped back below the alert threshold.
  if SL "$h" "test ! -e '$MEM_PIDFILE'" 2>/dev/null; then
    local ru; ru="$(SL "$h" "free -b | awk '/Mem:/{printf \"%d\", (\$2-\$7)*100/\$2}'" 2>/dev/null | tr -dc '0-9')"
    if [ -n "$ru" ] && [ "$ru" -lt 90 ] 2>/dev/null; then echo "OK hog-gone real-used=${ru}%"; return 0; fi
    echo "FAIL hog-gone but real-used=${ru:-?}% still >=90% on $h"; return 1
  fi
  echo "FAIL $MEM_PIDFILE STILL PRESENT on $h — MEM HOG MAY STILL RUN (manual: ssh root@$h 'kill \$(cat $MEM_PIDFILE)')"
  return 1
}
clean_service() {
  local h="$1"
  SL "$h" "systemctl start '$SVC_NAME'" >/dev/null 2>&1
  sleep 2
  local st; st="$(SL "$h" "systemctl is-active '$SVC_NAME' 2>/dev/null" 2>/dev/null | tr -dc 'a-z')"
  if [ "$st" = "active" ]; then echo "OK $SVC_NAME active"; return 0; fi
  echo "FAIL $SVC_NAME is '$st' (not active) on $h — SERVICE MAY BE DOWN (manual: ssh root@$h 'systemctl start $SVC_NAME')"
  return 1
}
clean_fault() {  # dispatch by type; echoes the verify line, returns its rc
  case "$1" in
    disk)    clean_disk "$2" ;;
    memory)  clean_mem "$2" ;;
    service) clean_service "$2" ;;
    *)       echo "OK nothing (unknown fault '$1')"; return 0 ;;
  esac
}

# ---------------------------------------------------------------------------
# Global trap — ALWAYS restore the currently-armed fault, then INDEPENDENTLY prove it.
# ---------------------------------------------------------------------------
on_exit() {
  rc=$?
  trap - EXIT INT TERM HUP PIPE
  if [ -n "$CUR_FAULT" ] && [ -n "$CUR_HOST" ]; then
    echo ""
    echo "[trap] restoring active fault: $CUR_FAULT on $CUR_HOST ..."
    v="$(clean_fault "$CUR_FAULT" "$CUR_HOST")"; vrc=$?
    echo "[trap] $v"
    [ "$vrc" -ne 0 ] && { echo "[trap] LOUD: a guinea-pig may still be degraded — SEE ABOVE"; [ "$rc" -eq 0 ] && rc=4; }
  fi
  exit "$rc"
}
trap on_exit EXIT INT TERM HUP PIPE

# ---------------------------------------------------------------------------
# --clean : sweep ALL guinea-pigs, restore + verify every fault artifact, exit.
# ---------------------------------------------------------------------------
if [ "${1:-}" = "--clean" ]; then
  echo "# campaign --clean : sweeping guinea-pigs for any leftover fault artifacts"
  rc=0
  for h in "$LIBRESPEED_HOST" "$MYSPEED_HOST"; do
    SL "$h" "hostname" >/dev/null 2>&1 || { echo "  $h: UNREACHABLE (skipped)"; continue; }
    # disk+memory only: the campaign never injects 'service', and sweeping it would `systemctl start nginx`
    # on librespeed01 — colliding with an owner's nginx graduation drive. Set SWEEP_SERVICE=1 to include it.
    _sweep_types="disk memory"; [ "${SWEEP_SERVICE:-0}" = 1 ] && _sweep_types="disk memory service"
    for ft in $_sweep_types; do
      [ "$ft" = service ] && [ "$h" != "$LIBRESPEED_HOST" ] && continue
      v="$(clean_fault "$ft" "$h")"; vrc=$?
      printf '  %-22s %-8s %s\n' "$h" "$ft" "$v"
      [ "$vrc" -ne 0 ] && rc=1
    done
  done
  CUR_FAULT=""; CUR_HOST=""
  echo "# sweep $([ $rc -eq 0 ] && echo OK || echo 'INCOMPLETE — see FAIL lines above')"
  exit "$rc"
fi

# ---------------------------------------------------------------------------
# Parse flags: --count N, --resume, --dry-run
# ---------------------------------------------------------------------------
COUNT=1; RESUME=0; DRY=0
while [ $# -gt 0 ]; do
  case "$1" in
    --count) COUNT="${2:-1}"; shift 2 ;;
    --count=*) COUNT="${1#*=}"; shift ;;
    --resume) RESUME=1; shift ;;
    --dry-run|--dry) DRY=1; shift ;;
    *) abort "unknown argument: $1 (see header USAGE)" ;;
  esac
done
[ "$COUNT" -ge 1 ] 2>/dev/null || abort "--count must be a positive integer (got '$COUNT')"

# ---------------------------------------------------------------------------
# Build the incident plan (host, fault) x COUNT.
# ---------------------------------------------------------------------------
mkdir -p "$OUT_DIR"
EXISTING=0
[ -f "$MANIFEST" ] && EXISTING="$(grep -c . "$MANIFEST" 2>/dev/null || echo 0)"
PHASE=0
[ "$RESUME" = 1 ] && PHASE="$EXISTING"   # continue the rotation phase so host/fault stay balanced

# Built-in rotation: alternate HOST and TYPE across DISK+MEMORY only (the reversible, non-colliding faults
# this campaign is built on). The 'service' fault (nginx crash/restore) is NOT in the default rotation — it
# collides with an owner's nginx graduation drive on librespeed01 and is opt-in only via FAULT_PLAN=service.
# myspeed01 leads the rotation because it is the quieter host (no concurrent service drive), so odd-count
# runs land the cleanest, least-contaminated pairs first.
ROT_HOST=("$MYSPEED_HOST" "$LIBRESPEED_HOST" "$MYSPEED_HOST" "$LIBRESPEED_HOST" \
          "$MYSPEED_HOST" "$LIBRESPEED_HOST" "$MYSPEED_HOST" "$LIBRESPEED_HOST")
ROT_TYPE=("disk"          "memory"           "memory"        "disk" \
          "disk"          "memory"           "memory"        "disk")
ROT_LEN="${#ROT_TYPE[@]}"

PLAN_HOST=(); PLAN_TYPE=()
if [ -n "$FAULT_PLAN" ]; then
  IFS=',' read -r -a _ftypes <<< "$FAULT_PLAN"
  i=0
  while [ "${#PLAN_TYPE[@]}" -lt "$COUNT" ]; do
    ft="${_ftypes[$(( i % ${#_ftypes[@]} ))]}"
    ft="$(echo "$ft" | tr -d '[:space:]')"
    # service only on the host that has a service check; else use librespeed
    if [ "$ft" = service ]; then h="$LIBRESPEED_HOST"
    else h="${ROT_HOST[$(( (PHASE + i) % ROT_LEN ))]}"; fi
    PLAN_TYPE+=("$ft"); PLAN_HOST+=("$h"); i=$((i+1))
  done
else
  for ((i=0; i<COUNT; i++)); do
    idx=$(( (PHASE + i) % ROT_LEN ))
    PLAN_HOST+=("${ROT_HOST[$idx]}"); PLAN_TYPE+=("${ROT_TYPE[$idx]}")
  done
fi

# PIN_HOST: pin EVERY incident to one guinea-pig (used for the routing-validation run and to prefer the
# quiet myspeed01 for clean, un-contaminated pairs). Fault sequence is unchanged; only the host is forced.
if [ -n "$PIN_HOST" ]; then
  for ((i=0; i<${#PLAN_HOST[@]}; i++)); do PLAN_HOST[$i]="$PIN_HOST"; done
fi

echo "# ============================================================================"
echo "# TG-84 data-accumulation campaign   stamp=$STAMP"
echo "#   manifest:     $MANIFEST  (existing lines: $EXISTING)"
echo "#   plan:         $COUNT incident(s)  resume=$RESUME phase=$PHASE  pace=${PACE_SECS}s  poll-budget=${POLL_TIMEOUT}s"
echo "#   force-detect: $([ "$FORCE_POLL" = 1 ] && echo 'yes (device:poll x2 + alerts.php)' || echo 'no (wait for cron cadence)')"
for ((i=0; i<COUNT; i++)); do printf '#     %2d. %-22s %s\n' "$((i+1))" "${PLAN_HOST[$i]}" "${PLAN_TYPE[$i]}"; done
echo "# ============================================================================"

# ---------------------------------------------------------------------------
# Preflight (once).
# ---------------------------------------------------------------------------
echo "## preflight"
[ -x "$INJECT_DISK" ] || abort "missing/!executable: $INJECT_DISK"
[ -x "$INJECT_MEM" ]  || abort "missing/!executable: $INJECT_MEM"
[ -f "$SSH_KEY" ]     || abort "SSH key not found: $SSH_KEY"
command -v python3 >/dev/null 2>&1 || abort "python3 not on PATH"
[ -d "$SHADOW_DIR" ]  || abort "predecessor shadow-log dir not found: $SHADOW_DIR"

# eval-gate contention (bracket-trick so pgrep does not self-match).
if pgrep -f '[e]val/eval-gate.sh' >/dev/null 2>&1; then
  abort "an eval-gate run is in progress locally — refusing (would contend for the model gateway)"
fi
echo "  eval-gate:  none running locally"

# Mutation MUST be OFF — EXACT series match, fail closed.
METRICS="$(STG "curl -fsS --max-time 10 http://127.0.0.1:${GROUNDER_PORT}/metrics" 2>/dev/null || true)"
[ -n "$METRICS" ] || abort "could not read grounder /metrics on ${TG_HOST}:${GROUNDER_PORT} — cannot prove mutation OFF"
MVAL="$(printf '%s\n' "$METRICS" | awk '
  /^mutation_enabled([ {]|$)/ { n++; v=$NF }
  END { if (n==1) print v; else if (n>1) print "AMBIG"; else print "ABSENT" }')"
case "$MVAL" in
  ABSENT) abort "mutation_enabled gauge absent from /metrics — cannot prove mutation OFF" ;;
  AMBIG)  abort "mutation_enabled matched >1 series — refusing to guess the gauge value" ;;
esac
[ "$(awk -v v="$MVAL" 'BEGIN{print (v+0==0)?"yes":"no"}')" = "yes" ] \
  || abort "MUTATION IS ON (mutation_enabled=$MVAL on $TG_HOST) — refusing (this campaign must only TRIAGE)"
echo "  mutation:   OFF (mutation_enabled=$MVAL on $TG_HOST)"

# Reachability of every host in the plan (guinea-pigs must be up; we NEVER power one on).
declare -A SEEN_HOST=()
for h in "${PLAN_HOST[@]}"; do
  [ -n "${SEEN_HOST[$h]:-}" ] && continue; SEEN_HOST[$h]=1
  if SL "$h" "hostname" >/dev/null 2>&1; then echo "  reachable:  $h"
  else echo "  UNREACHABLE: $h — its incidents will be skipped (never powered on)"; fi
done

# Alert-rule SCOPE advisory (memory only). The os-agnostic memory rule (#25, fires >=85%)
# is the ONLY routing rule scoped by alert_device_map — a guinea-pig missing from that map
# is silently skipped by AlertRules and can NEVER pair (the 0/0-miss trap; see README). The
# disk rule (#22) and service rule (#9) are global (no map), and #21 needs os=linux which the
# LXC guests are not — so only #25's scope matters. Read-only advisory; does not abort.
MEM_RULE_ID="${MEM_RULE_ID:-25}"
declare -A SEEN_SCOPE=()
for i in "${!PLAN_HOST[@]}"; do
  [ "${PLAN_TYPE[$i]}" = memory ] || continue
  h="${PLAN_HOST[$i]}"
  [ -n "${SEEN_SCOPE[$h]:-}" ] && continue; SEEN_SCOPE[$h]=1
  n="$(SNMS "mysql -N librenms -e \"select count(*) from alert_device_map m join devices d on d.device_id=m.device_id where m.rule_id=${MEM_RULE_ID} and d.hostname='${h}';\"" 2>/dev/null | tr -dc '0-9')"
  if [ "${n:-0}" -ge 1 ]; then echo "  mem-scope:  $h in memory rule #${MEM_RULE_ID} device-map (can fire)"
  else echo "  OUT-OF-SCOPE: $h is NOT in memory rule #${MEM_RULE_ID} device-map — its memory incidents will MISS 0/0. Fix on ${NMS_HOST}: INSERT INTO alert_device_map (rule_id,device_id) SELECT ${MEM_RULE_ID},device_id FROM devices WHERE hostname='${h}';"; fi
done

if [ "$DRY" = 1 ]; then
  echo "## dry-run: preflight complete, plan printed above, NOTHING injected."
  CUR_FAULT=""; CUR_HOST=""
  exit 0
fi

# ---------------------------------------------------------------------------
# Detection: has each side triaged THIS host since THIS incident's baseline?
# ---------------------------------------------------------------------------
tg_probe() {  # prints "<count>\t<comma-sep external_refs>"; scoped host + since-baseline + fault-rule hint
  local h="$1" base="$2" fault="$3" kw; kw="$(hkw "$h")"
  local rulef
  case "$fault" in
    disk)    rulef="and (alert_rule ilike '%space%' or alert_rule ilike '%disk%' or alert_rule ilike '%storage%')" ;;
    memory)  rulef="and (alert_rule ilike '%memory%' or alert_rule ilike '%mem %' or alert_rule ilike '%swap%')" ;;
    service) rulef="and (alert_rule ilike '%service%' or alert_rule ilike '%down%' or alert_rule ilike '%nginx%' or alert_rule ilike '%http%')" ;;
    *)       rulef="" ;;
  esac
  local sql="select count(*), coalesce(string_agg(distinct external_ref, ','),'') from session_triage \
where created_at > '$base' and (host ilike '%${h}%' or host ilike '%${kw}%' or external_ref ilike '%${kw}%') $rulef;"
  STG "docker exec -i $PG_CONTAINER psql -U postgres -d grounder -tAF $'\t' -c \"$sql\"" 2>/dev/null | head -1
}
pred_probe() {  # prints "<count>\t<sessions>\t<issues>"; agentic shadow-log entries naming the host since baseline
  local h="$1" base_epoch="$2"; local f="$SHADOW_DIR/shadow-$(date -u '+%Y-%m-%d').jsonl"
  [ -f "$f" ] || { printf '0\t\t\n'; return; }
  SB_BASE="$base_epoch" SB_HOST="$h" python3 - "$f" <<'PY' 2>/dev/null
import sys, json, os
f = sys.argv[1]
base = int(os.environ.get("SB_BASE", "0") or 0)
host = (os.environ.get("SB_HOST", "") or "").lower()
short = host
import re
m = re.sub(r'^[a-z]+[0-9]+', '', host); m = re.sub(r'[0-9]+$', '', m)
kw = m or host
sess, iss = set(), set()
for line in open(f, errors="ignore"):
    line = line.strip()
    if not line:
        continue
    try:
        d = json.loads(line)
    except Exception:
        continue
    if (d.get("source") or d.get("emitter") or d.get("src") or "") != "mutation-shadow-gate.py":
        continue
    ts = d.get("ts") or d.get("timestamp") or d.get("time") or 0
    try:
        ts = int(float(ts))
    except Exception:
        ts = 0
    if ts and ts < base:
        continue
    blob = json.dumps(d).lower()
    if host in blob or (kw and kw in blob):
        sid = d.get("session") or d.get("session_id") or d.get("sess") or ""
        isu = d.get("issue") or d.get("issue_title") or ""
        if sid:
            sess.add(str(sid))
        if isu:
            iss.add(str(isu))
print("%d\t%s\t%s" % (len(sess), ",".join(sorted(sess)), ",".join(sorted(iss))))
PY
}

# ---------------------------------------------------------------------------
# Force LibreNMS detection: poll the device twice across the rule's 60s delay, then dispatch.
# ---------------------------------------------------------------------------
force_detect() {
  local h="$1" fault="$2"
  [ "$FORCE_POLL" = 1 ] || { echo "  (force-detect off — waiting for cron cadence)"; return 0; }
  # device:poll (NOT the legacy poller.php -h) runs the AlertRules evaluator — the
  # per-device rule sweep that creates alert_log/alerts rows. poller.php -h only
  # refreshes the polled data; it never evaluates rules, so the alert never fires
  # and neither side is handed an incident to triage (the 0/0-miss harness bug).
  echo "  force-detect: device:poll $h (pass 1, runs AlertRules) ..."
  SNMS "sudo -u librenms bash -lc 'cd /opt/librenms && php artisan device:poll $h </dev/null >/dev/null 2>&1'" >/dev/null 2>&1 || true
  [ "$fault" = service ] && SNMS "sudo -u librenms bash -lc 'cd /opt/librenms && php check-services.php -h $h >/dev/null 2>&1'" >/dev/null 2>&1 || true
  sleep 65   # satisfy any rule's 60s delay before the second evaluation
  echo "  force-detect: device:poll $h (pass 2) + alerts.php dispatch ..."
  SNMS "sudo -u librenms bash -lc 'cd /opt/librenms && php artisan device:poll $h </dev/null >/dev/null 2>&1'" >/dev/null 2>&1 || true
  [ "$fault" = service ] && SNMS "sudo -u librenms bash -lc 'cd /opt/librenms && php check-services.php -h $h >/dev/null 2>&1'" >/dev/null 2>&1 || true
  SNMS "sudo -u librenms bash -lc 'cd /opt/librenms && php alerts.php >/dev/null 2>&1'" >/dev/null 2>&1 || true
}
force_recover() {  # after cleanup, re-evaluate so the alert clears promptly (enables a clean re-fire later)
  local h="$1"
  [ "$FORCE_POLL" = 1 ] || return 0
  # device:poll (not poller.php -h) re-runs AlertRules: an empty result set now
  # transitions the active alert to RECOVERED so the next incident can re-fire.
  SNMS "sudo -u librenms bash -lc 'cd /opt/librenms && php artisan device:poll $h </dev/null >/dev/null 2>&1; php alerts.php >/dev/null 2>&1'" >/dev/null 2>&1 || true
}

# ---------------------------------------------------------------------------
# Inject a fault; echo a status token on stdout: OK / ALREADY / FAIL <reason>.
# ---------------------------------------------------------------------------
inject_fault() {
  local fault="$1" h="$2" out rc
  case "$fault" in
    disk)
      out="$("$INJECT_DISK" "$h" "$DISK_PCT" 2>&1)"; rc=$?
      printf '%s\n' "$out" | sed 's/^/      /' >&2
      [ "$rc" -eq 0 ] || { echo "FAIL disk-inject rc=$rc"; return 1; }
      printf '%s\n' "$out" | grep -q 'already at/above' && { echo "ALREADY"; return 0; }
      # assert the achieved storage_perc landed inside rule-22's [90,95) window
      local ach; ach="$(SL "$h" "df -P / | awk 'NR==2{printf \"%d\", \$3*100/\$2}'" 2>/dev/null | tr -dc '0-9')"
      [ -n "$ach" ] || { echo "FAIL could-not-read-df-after-inject"; return 1; }
      if [ "$ach" -lt 90 ] || [ "$ach" -ge 95 ]; then echo "FAIL achieved-${ach}%-outside-[90,95)"; return 1; fi
      echo "OK storage_perc=${ach}%"; return 0 ;;
    memory)
      out="$("$INJECT_MEM" "$h" "$MEM_PCT" "$MEM_HOLD" 2>&1)"; rc=$?
      printf '%s\n' "$out" | sed 's/^/      /' >&2
      [ "$rc" -eq 0 ] || { echo "FAIL mem-inject rc=$rc"; return 1; }
      printf '%s\n' "$out" | grep -q 'already at/above' && { echo "ALREADY"; return 0; }
      local ru; ru="$(SL "$h" "free -b | awk '/Mem:/{printf \"%d\", (\$2-\$7)*100/\$2}'" 2>/dev/null | tr -dc '0-9')"
      [ -n "$ru" ] || { echo "FAIL could-not-read-free-after-inject"; return 1; }
      if [ "$ru" -lt 88 ]; then echo "FAIL achieved-real-used-${ru}%-below-threshold"; return 1; fi
      echo "OK real-used=${ru}%"; return 0 ;;
    service)
      SL "$h" "systemctl stop '$SVC_NAME'" >/dev/null 2>&1
      sleep 2
      local st; st="$(SL "$h" "systemctl is-active '$SVC_NAME' 2>/dev/null" 2>/dev/null | tr -dc 'a-z')"
      if [ "$st" = "active" ]; then echo "FAIL $SVC_NAME still active after stop"; return 1; fi
      echo "OK $SVC_NAME=$st"; return 0 ;;
    *) echo "FAIL unknown-fault-$fault"; return 1 ;;
  esac
}

# ---------------------------------------------------------------------------
# Append one incident record to the manifest (JSON built in python to avoid quoting hazards).
# ---------------------------------------------------------------------------
append_manifest() {
  MF_ID="$1" MF_HOST="$2" MF_FAULT="$3" MF_BASE="$4" MF_STATUS="$5" \
  MF_INJECT="$6" MF_TG_OK="$7" MF_PRED_OK="$8" MF_TG_REFS="$9" MF_PRED_SESS="${10}" \
  MF_PRED_ISS="${11}" MF_CLEAN="${12}" MF_NOTE="${13}" MF_MANIFEST="$MANIFEST" \
  python3 - <<'PY'
import os, json, datetime
def lst(s):
    s = (s or "").strip()
    return [x for x in s.split(",") if x] if s else []
rec = {
    "ts": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "incident_id": os.environ["MF_ID"],
    "fault_type": os.environ["MF_FAULT"],
    "host": os.environ["MF_HOST"],
    "baseline_utc": os.environ["MF_BASE"],
    "status": os.environ["MF_STATUS"],
    "inject_result": os.environ["MF_INJECT"],
    "tg_triaged": os.environ["MF_TG_OK"] == "1",
    "pred_triaged": os.environ["MF_PRED_OK"] == "1",
    "tg_refs": lst(os.environ["MF_TG_REFS"]),
    "pred_sessions": lst(os.environ["MF_PRED_SESS"]),
    "pred_issues": lst(os.environ["MF_PRED_ISS"]),
    "cleanup_verified": os.environ["MF_CLEAN"] == "1",
    "note": os.environ["MF_NOTE"],
}
with open(os.environ["MF_MANIFEST"], "a") as fh:
    fh.write(json.dumps(rec) + "\n")
PY
}

# ---------------------------------------------------------------------------
# One incident, end-to-end.
# ---------------------------------------------------------------------------
run_incident() {
  local seq="$1" h="$2" fault="$3"
  local id="cmp-${STAMP}-$(printf '%03d' "$seq")"
  local base base_epoch note="" status="MISS" inj tg_ok=0 pred_ok=0
  local tg_refs="" pred_sess="" pred_iss="" clean_ok=0 clean_line=""
  base="$(date -u -d "-${SKEW_SLACK} seconds" '+%Y-%m-%d %H:%M:%S')"
  base_epoch="$(date -u -d "-${SKEW_SLACK} seconds" '+%s')"

  echo ""
  echo "## incident $id  ($seq)  host=$h  fault=$fault  since=${base} UTC"

  # Reachability (never power on; skip if down).
  if ! SL "$h" "hostname" >/dev/null 2>&1; then
    note="host unreachable — skipped (never powered on)"
    echo "  SKIP: $note"
    append_manifest "$id" "$h" "$fault" "$base" "SKIP" "unreachable" 0 0 "" "" "" 1 "$note"
    N_MISS=$((N_MISS+1)); N_DONE=$((N_DONE+1)); return 0
  fi
  # 'service' fault only where a service check exists.
  if [ "$fault" = service ] && [ "$h" != "$LIBRESPEED_HOST" ]; then
    note="service fault only supported on $LIBRESPEED_HOST (has the http service check) — skipped"
    echo "  SKIP: $note"
    append_manifest "$id" "$h" "$fault" "$base" "SKIP" "unsupported" 0 0 "" "" "" 1 "$note"
    N_MISS=$((N_MISS+1)); N_DONE=$((N_DONE+1)); return 0
  fi

  # Arm the trap BEFORE injecting so any interruption restores this fault.
  CUR_FAULT="$fault"; CUR_HOST="$h"
  echo "  inject: $fault on $h"
  inj="$(inject_fault "$fault" "$h")"; local irc=$?
  echo "  inject-result: $inj"
  if [ "$irc" -ne 0 ]; then
    note="injection failed: $inj"
    clean_line="$(clean_fault "$fault" "$h")"; [ $? -eq 0 ] && clean_ok=1
    echo "  cleanup: $clean_line"
    CUR_FAULT=""; CUR_HOST=""
    append_manifest "$id" "$h" "$fault" "$base" "MISS" "$inj" 0 0 "" "" "" "$clean_ok" "$note"
    N_MISS=$((N_MISS+1)); [ "$clean_ok" = 0 ] && N_CLEANFAIL=$((N_CLEANFAIL+1)); N_DONE=$((N_DONE+1)); return 0
  fi
  if [ "$inj" = "ALREADY" ]; then
    note="target already at/above threshold — no NEW fault injected; nothing to pair"
    clean_line="$(clean_fault "$fault" "$h")"; [ $? -eq 0 ] && clean_ok=1
    echo "  cleanup: $clean_line"
    CUR_FAULT=""; CUR_HOST=""
    append_manifest "$id" "$h" "$fault" "$base" "MISS" "$inj" 0 0 "" "" "" "$clean_ok" "$note"
    N_MISS=$((N_MISS+1)); N_DONE=$((N_DONE+1)); return 0
  fi

  # Force LibreNMS to detect + fire the routing rule (both transports).
  force_detect "$h" "$fault"

  # Poll (bounded) until BOTH sides triage this host since baseline.
  echo "  poll (budget=${POLL_TIMEOUT}s @ ${POLL_INTERVAL}s):"
  local deadline=$(( $(date +%s) + POLL_TIMEOUT )) it=0
  while :; do
    local now; now="$(date +%s)"; [ "$now" -lt "$deadline" ] || break
    it=$((it+1))
    local tgp predp tc pc
    tgp="$(tg_probe "$h" "$base" "$fault")"; tc="$(printf '%s' "$tgp" | cut -f1 | tr -dc '0-9')"; tc="${tc:-0}"
    predp="$(pred_probe "$h" "$base_epoch")"; pc="$(printf '%s' "$predp" | cut -f1 | tr -dc '0-9')"; pc="${pc:-0}"
    [ "$tc" -gt 0 ] 2>/dev/null && { tg_ok=1; tg_refs="$(printf '%s' "$tgp" | cut -f2)"; }
    [ "$pc" -gt 0 ] 2>/dev/null && { pred_ok=1; pred_sess="$(printf '%s' "$predp" | cut -f2)"; pred_iss="$(printf '%s' "$predp" | cut -f3)"; }
    printf '    [poll %02d | %s UTC] TG=%s(ok=%d)  pred=%s(ok=%d)  remaining=%ss\n' \
      "$it" "$(date -u '+%H:%M:%S')" "$tc" "$tg_ok" "$pc" "$pred_ok" "$(( deadline - now ))"
    { [ "$tg_ok" -eq 1 ] && [ "$pred_ok" -eq 1 ]; } && { echo "    both systems triaged."; break; }
    sleep "$POLL_INTERVAL"
  done

  if [ "$tg_ok" -eq 1 ] && [ "$pred_ok" -eq 1 ]; then status="PAIRED"
  elif [ "$tg_ok" -eq 1 ] || [ "$pred_ok" -eq 1 ]; then status="ONE-SIDED"
  else status="MISS"; fi
  [ "$status" != "PAIRED" ] && note="tg_ok=$tg_ok pred_ok=$pred_ok within ${POLL_TIMEOUT}s budget"

  # CLEAN UP + INDEPENDENT verify, ALWAYS.
  clean_line="$(clean_fault "$fault" "$h")"; [ $? -eq 0 ] && clean_ok=1
  echo "  cleanup: $clean_line"
  force_recover "$h"
  CUR_FAULT=""; CUR_HOST=""
  if [ "$clean_ok" = 0 ]; then
    note="${note:+$note; }CLEAN-FAIL: $clean_line"
    N_CLEANFAIL=$((N_CLEANFAIL+1))
  fi

  append_manifest "$id" "$h" "$fault" "$base" "$status" "$inj" "$tg_ok" "$pred_ok" \
    "$tg_refs" "$pred_sess" "$pred_iss" "$clean_ok" "$note"

  echo "  => $status  (tg=$tg_ok pred=$pred_ok clean=$clean_ok)  refs: tg=[${tg_refs}] pred=[${pred_sess}]"
  case "$status" in
    PAIRED)    N_PAIRED=$((N_PAIRED+1)) ;;
    ONE-SIDED) N_ONESIDED=$((N_ONESIDED+1)) ;;
    *)         N_MISS=$((N_MISS+1)) ;;
  esac
  N_DONE=$((N_DONE+1))
  return 0
}

# ---------------------------------------------------------------------------
# Drive the plan.
# ---------------------------------------------------------------------------
for ((i=0; i<COUNT; i++)); do
  run_incident "$(( EXISTING + i + 1 ))" "${PLAN_HOST[$i]}" "${PLAN_TYPE[$i]}"
  if [ "$i" -lt "$(( COUNT - 1 ))" ]; then
    echo "  pace: sleeping ${PACE_SECS}s before the next injection ..."
    sleep "$PACE_SECS"
  fi
done

# ---------------------------------------------------------------------------
# Summary.
# ---------------------------------------------------------------------------
TOTAL_PAIRED="$(grep -c '"status": "PAIRED"' "$MANIFEST" 2>/dev/null || echo 0)"
echo ""
echo "==================== CAMPAIGN SUMMARY ($STAMP) ===================="
echo " this run:     done=$N_DONE  PAIRED=$N_PAIRED  ONE-SIDED=$N_ONESIDED  MISS=$N_MISS  clean-fails=$N_CLEANFAIL"
echo " manifest:     $MANIFEST"
echo " total PAIRED in manifest (all runs): $TOTAL_PAIRED"
[ "$N_CLEANFAIL" -gt 0 ] && echo " ‼ WARNING: $N_CLEANFAIL cleanup(s) could not be verified — run: $0 --clean"
echo " next batch:   $0 --count N --resume"
echo "=================================================================="
CUR_FAULT=""; CUR_HOST=""
exit 0
