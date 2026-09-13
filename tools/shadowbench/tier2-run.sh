#!/usr/bin/env bash
# tier2-run.sh — ONE Tier-2 AMBIGUOUS MULTI-SIGNAL head-to-head on ONE guinea-pig guest (TG-72).
#
# WHAT IT DOES (fully scripted; no Claude workflow in the loop; template: tier1-run.sh)
#   0. PREFLIGHT (modernized — see the block comment at the preflight section): injector pool agreement,
#      breaker closed, guest healthy, BOTH LibreNMS rules for the two fault classes enabled.
#   1. Record BOTH faults in the injected_fault ledger (durable-record-BEFORE-effect), then inject TWO
#      concurrent faults on the ONE target guest:
#        service-down  — systemctl stop of the pool-declared unit   (ground truth: start/restart-service)
#        log-fill      — fallocate-grow the pool-declared app log into the disk-rule window
#                        (ground truth: grounded stand-down — no reclaim verb is registered)
#      TWO faults with DISTINCT operator-declared correct answers (core/diagcorpus/expectations.json) is
#      what makes this Tier 2: root-cause SELECTION under ambiguity + the single-cause discipline.
#   2. Sanctioned LibreNMS detection (campaign.sh's path): `php artisan device:poll` x2 across the rule
#      delay, check-services.php, then alerts.php dispatch — NEVER a write to the alerts table.
#   3. POLL until BOTH systems triaged the target since baseline; then RESTORE BOTH faults immediately
#      (verified independently, ledger rows closed only on a positive verify) — the fault window ends the
#      moment the data is banked, not after the judging.
#   4. STRICT pair select (host + since-baseline + the two injected classes only, via _driver.fault_class
#      — the SAME classifier that forms campaign pairs), blind-judge, and emit a Tier-2 scorecard
#      out/tier2-scorecard-<stamp>.json carrying the per-side single_cause_ok mechanical check.
#
# WHY THE TWO FAULTS ARE INJECTED DIRECTLY (not through cmd/faultinjector): the engine's INVARIANT 2
# refuses a second fault on a host that owes a restore — deliberately, and correctly, for the ESTATE
# (tools/faultinjector/plan.go). Tier 2 needs the one state the engine will never manufacture: two
# concurrent declared faults on one guest. So this script performs both effects itself, tier1-injector
# style, while keeping the part of the engine's discipline that is about SAFETY rather than scheduling:
#   * durable-record-BEFORE-effect: both ledger rows (restore_state='pending', restore_due_at, fault_ref =
#     the exact undo handle) are INSERTed before any remote effect. A crash between record and effect
#     leaves a harmless obligation the engine's reconciler discharges idempotently; the reverse order is
#     how guests get stranded (plan.go PROVENANCE).
#   * the recorded obligations make a RUNNING engine treat this host as busy (busy-ness derives only from
#     the ledger — INVARIANT 1), so the campaign rotation cannot stack a third fault here mid-window.
#   * the two rows are real A1-denominator ground truth: diagcorpus and the axis scorer join sessions to
#     them by (host, window, fault_type), so the tier-2 window is scoreable by the pre-registered,
#     judge-free primary endpoint too.
#
# HALF-INJECTION REFUSAL: if only one of the two faults lands there is no ambiguity, hence no Tier-2
# experiment — the run REFUSES TO SCORE (exit 6, distinct from every other outcome), restores whatever
# landed, and emits a scorecard that documents the refusal. A half-injection proves nothing and must
# never bank as a tier-2 pair.
#
# EXIT CODES
#   0 PASS (both faults landed, both systems triaged, judge scored both sides; dry-run: flow completed)
#   1 FAIL (nothing scoreable: neither system triaged, or the judge could not score both sides)
#   2 ABORT (preflight refusal — nothing was injected)
#   3 ONE-SIDED (only one SYSTEM triaged within budget — not a valid head-to-head)
#   4 restore verification failed (overrides any other code — a possibly-degraded guinea-pig outranks a result)
#   6 HALF-INJECTION (only one FAULT landed — refused to score)
#
# USAGE
#   TIER2_HOST=<guest> TG_HOST=<box> NMS_HOST=<nms> TIER2_POOL_FILE=<pool> tools/shadowbench/tier2-run.sh
#   tools/shadowbench/tier2-run.sh --dry-run                    # fixture flow, no estate contact at all
#   TIER2_DRY_FIXTURE=half tools/shadowbench/tier2-run.sh --dry-run   # prove the refuse-to-score path (exit 6)
#
# Estate identities (guest, TG box, NMS host, pool file) come ONLY from the environment — no estate
# hostname is committed in this file (env-config pattern; the STONITH rule). Remote commands go over
# bounded ssh exactly like tier1-run.sh/campaign.sh; nothing here shells out via `sh -c` locally, and the
# LibreNMS invocations replicate campaign.sh's sanctioned remote command lines verbatim.
set -u

# ---------------------------------------------------------------------------
# Flags.
# ---------------------------------------------------------------------------
DRY=0
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run|--dry) DRY=1; shift ;;
    *) echo "ABORT: unknown argument: $1 (see header USAGE)" >&2; exit 2 ;;
  esac
done

# ---------------------------------------------------------------------------
# Configuration. Estate identities are REQUIRED from env in a real run; everything else has neutral
# defaults (paths, container names, rule ids, timing) that carry no estate hostname.
# ---------------------------------------------------------------------------
SB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SB_DIR/../.." && pwd)"
FIX_DIR="$SB_DIR/fixtures/tier2"
OUT_DIR="$SB_DIR/out"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/one_key}"
SSH_USER="${SSH_USER:-root}"
ENV_PATH="${ENV_PATH:-/srv/tg/deploy/.env}"
PG_CONTAINER="${PG_CONTAINER:-territory-grounder-postgres-1}"
GROUNDER_PORT="${GROUNDER_PORT:-8081}"
SHADOW_DIR="${SHADOW_DIR:-$HOME/logs/claude-gateway/mutation-shadow}"
DISK_RULE_ID="${DISK_RULE_ID:-22}"        # "Space on / is >= 90% and < 95%" — bounded [90,95)
SERVICE_RULE_ID="${SERVICE_RULE_ID:-9}"   # "Service up/down"
TIER2_DISK_PCT="${TIER2_DISK_PCT:-91}"    # storage_perc target, inside the disk rule's bounded window
RESTORE_AFTER_MIN="${RESTORE_AFTER_MIN:-45}"  # ledger restore_due_at: the reconciler's safety-net horizon
POLL_TIMEOUT="${POLL_TIMEOUT:-1200}"
POLL_INTERVAL="${POLL_INTERVAL:-60}"
SKEW_SLACK="${SKEW_SLACK:-120}"
SSH_CMD_TIMEOUT="${SSH_CMD_TIMEOUT:-45}"
WATCH_MIN="${WATCH_MIN:-22}"
WATCH_INT="${WATCH_INT:-20}"
MODEL="${MODEL:-primary}"
TIER2_DRY_FIXTURE="${TIER2_DRY_FIXTURE:-both}"

abort() { echo "ABORT: $*" >&2; exit 2; }

if [ "$DRY" = 1 ]; then
  # Dry-run never dials anything: estate identities come from the fixtures, remote reads from estate.json.
  TIER2_HOST="gp-alpha01"
  TG_HOST="${TG_HOST:-dry-unused}"; NMS_HOST="${NMS_HOST:-dry-unused}"
  TIER2_POOL_FILE="$FIX_DIR/pool.txt"; ENV_PATH="$FIX_DIR/deploy.env"
  EST_FIX="$FIX_DIR/estate.json"
  case "$TIER2_DRY_FIXTURE" in
    both) INJ_FIX="$FIX_DIR/inject-both.json" ;;
    half) INJ_FIX="$FIX_DIR/inject-half.json" ;;
    *) abort "TIER2_DRY_FIXTURE must be 'both' or 'half' (got '$TIER2_DRY_FIXTURE')" ;;
  esac
else
  [ -n "${TIER2_HOST:-}" ]      || abort "TIER2_HOST is required (the guinea-pig guest name, as in the pool file)"
  [ -n "${TG_HOST:-}" ]         || abort "TG_HOST is required (the TG docker host)"
  [ -n "${NMS_HOST:-}" ]        || abort "NMS_HOST is required (the LibreNMS host)"
  [ -n "${TIER2_POOL_FILE:-}" ] || abort "TIER2_POOL_FILE is required (the operator's injector pool file)"
fi

WATCH="$SB_DIR/outage-watch.sh"
EXTRACT_PRED="$SB_DIR/extract_predecessor.py"
EXTRACT_TG="$SB_DIR/extract_tg.sh"
JUDGE="$SB_DIR/judge.py"
OPSCHEMA="$REPO_ROOT/core/actuate/opschema/opschema.json"
EXPECTATIONS="$REPO_ROOT/core/diagcorpus/expectations.json"
export SSH_KEY TG_HOST SSH_USER ENV_PATH PG_CONTAINER

STAMP="$(date -u '+%Y%m%dT%H%M%SZ')"
WATCH_LOG="$OUT_DIR/tier2-outage-watch-${STAMP}.log"
SCORECARD_OUT="$OUT_DIR/tier2-scorecard-${STAMP}.json"
WORK=""; WATCH_PID=""
STATUS="FAIL"; EXIT_CODE=1; KEY=""; NOTE=""
tg_ok=0; pred_ok=0; tg_full=0
SVC_ARMED=0; LOG_ARMED=0            # a fault whose effect stage was ENTERED; the trap restores armed faults
SVC_LEDGER_ID=""; LOG_LEDGER_ID=""
RESTORED_DONE=0; RESTORE_FAILED=0
POSTURE_START=""; POSTURE_END=""; BREAKER_STATE=""

# Bounded ssh (tier1 pattern): hard timeout + server-alive so a connected-but-hung call cannot hold the
# faults open past the poll budget. SC2029 is intentional throughout — variables expanding CLIENT-side
# into the remote command line is exactly how these harness scripts parameterize remote reads; every value
# interpolated is preflight-validated against a strict charset (see validate_token below).
# shellcheck disable=SC2029
SB() { timeout "$SSH_CMD_TIMEOUT" ssh -i "$SSH_KEY" -o ConnectTimeout=10 -o ServerAliveInterval=5 \
        -o ServerAliveCountMax=2 -o BatchMode=yes -o StrictHostKeyChecking=no "$@"; }
S()   { SB "${SSH_USER}@${TG_HOST}" "$@"; }     # -> the TG docker host (grounder DB + metrics)
SL()  { SB "root@${TIER2_HOST}" "$@"; }         # -> the injected guinea-pig
SNMS() { SB "root@${NMS_HOST}" "$@"; }          # -> the LibreNMS host

psql_tg() {  # one bounded psql read/write against the grounder DB (tier1's docker-exec path)
  S "docker exec -i $PG_CONTAINER psql -U postgres -d grounder -tAc \"$1\"" 2>/dev/null
}

# validate_token NAME VALUE REGEX — every value later interpolated into remote SQL/shell lines must pass
# its charset gate here, once, in preflight. This is the harness-side analogue of the fixed-argv rule: an
# injected quote/space/semicolon must be structurally impossible, not merely unlikely.
validate_token() {
  printf '%s' "$2" | grep -qE "$3" || abort "$1 '$2' fails its safety charset ($3)"
}

# ---------------------------------------------------------------------------
# Reversible-fault trap — the safety net for any exit before the inline restore has run. The inline
# restore (restore_all) is the normal path so the fault window closes as soon as the data is banked;
# this trap covers a death before that point. A still-degraded guinea-pig is a LOUD failure, never a
# silent "restored".
# ---------------------------------------------------------------------------
# shellcheck disable=SC2317  # invoked via the trap only
cleanup() {
  rc=$?
  trap - EXIT INT TERM HUP PIPE
  [ -n "${WATCH_PID:-}" ] && kill "$WATCH_PID" 2>/dev/null
  if [ "$RESTORED_DONE" -eq 0 ] && [ "$DRY" = 0 ] && { [ "$SVC_ARMED" -eq 1 ] || [ "$LOG_ARMED" -eq 1 ]; }; then
    echo ""
    echo "[trap] run died with fault(s) armed — restoring now"
    restore_all
  fi
  [ "$RESTORE_FAILED" -eq 1 ] && { [ "$rc" -eq 0 ] || [ "$rc" -eq 3 ] || [ "$rc" -eq 6 ]; } && rc=4
  [ -n "${WORK:-}" ] && [ -d "$WORK" ] && rm -rf "$WORK" 2>/dev/null
  exit "$rc"
}
trap cleanup EXIT INT TERM HUP PIPE

# ---------------------------------------------------------------------------
# Restore BOTH faults + INDEPENDENT verification + ledger close. The undo per class is the engine's own
# (tools/faultinjector/effects.go UndoArgv): `systemctl start <unit>` verified by is-active, and
# `truncate -s 0 <log>` (never rm — the inode a running service holds open must survive) verified by
# `test ! -s`. A ledger row is marked 'restored' ONLY on a positive verify — the ledger describes the
# estate, not our intentions toward it (store.go MarkRestored) — otherwise 'failed', which quarantines
# the host for the engine and flags the operator.
# ---------------------------------------------------------------------------
ledger_mark() { # $1=id $2=restored|failed $3=reason
  [ -n "$1" ] || return 0
  if [ "$2" = restored ]; then
    psql_tg "UPDATE injected_fault SET restore_state='restored', restored_at=now() WHERE id=$1" >/dev/null
  else
    psql_tg "UPDATE injected_fault SET restore_state='failed', note = note || ' | restore failed: $3' WHERE id=$1" >/dev/null
  fi
}

SVC_RESTORE="skipped"; LOG_RESTORE="skipped"
restore_all() {
  RESTORED_DONE=1
  [ "$DRY" = 1 ] && { SVC_RESTORE="dry-run (nothing armed)"; LOG_RESTORE="dry-run (nothing armed)"; return 0; }
  if [ "$SVC_ARMED" -eq 1 ]; then
    echo "[restore] systemctl start $POOL_UNIT on $TIER2_HOST"
    SL "systemctl start '$POOL_UNIT'" >/dev/null 2>&1
    sleep 2
    st="$(SL "systemctl is-active '$POOL_UNIT'" 2>/dev/null | tr -dc 'a-z-')"
    if [ "$st" = "active" ]; then
      SVC_RESTORE="verified (is-active=active)"; ledger_mark "$SVC_LEDGER_ID" restored ""
    else
      SVC_RESTORE="FAILED (is-active=$st)"; RESTORE_FAILED=1
      ledger_mark "$SVC_LEDGER_ID" failed "is-active=$st"
      echo "[restore] !! $POOL_UNIT NOT active on $TIER2_HOST — MANUAL: ssh root@$TIER2_HOST 'systemctl start $POOL_UNIT'"
    fi
  fi
  if [ "$LOG_ARMED" -eq 1 ]; then
    echo "[restore] truncate -s 0 $POOL_LOGPATH on $TIER2_HOST"
    SL "truncate -s 0 -- '$POOL_LOGPATH'" >/dev/null 2>&1
    if SL "test ! -s '$POOL_LOGPATH'" 2>/dev/null; then
      LOG_RESTORE="verified (empty-or-absent)"; ledger_mark "$LOG_LEDGER_ID" restored ""
    else
      LOG_RESTORE="FAILED (log still has bytes)"; RESTORE_FAILED=1
      ledger_mark "$LOG_LEDGER_ID" failed "log still non-empty"
      echo "[restore] !! $POOL_LOGPATH still non-empty on $TIER2_HOST — DISK MAY STILL BE FILLED. MANUAL: ssh root@$TIER2_HOST 'truncate -s 0 $POOL_LOGPATH'"
    fi
  fi
  # Re-evaluate so the alerts clear promptly (sanctioned path — an empty rule result transitions the
  # active alerts to RECOVERED; never a write to the alerts table).
  SNMS "sudo -u librenms bash -lc 'cd /opt/librenms && php artisan device:poll $TIER2_HOST </dev/null >/dev/null 2>&1; php check-services.php -h $TIER2_HOST >/dev/null 2>&1; php alerts.php >/dev/null 2>&1'" >/dev/null 2>&1 || true
  return 0
}

# ---------------------------------------------------------------------------
# 0. PREFLIGHT — MODERNIZED vs tier1-run.sh (TG-72).
#
# tier1-run.sh hard-refuses unless tg_may_actuate == 0. As a REFUSAL that check is STALE: the owner set
# the live posture to Semi-auto (Phase-2 flip 2026-07-20; BOARD.md § Live posture, re-verified from the
# running system each session), so on today's estate the gauge legitimately reads 1 and a mutation-OFF
# gate would refuse every valid run — while protecting nothing, because ladder scoring never consumed an
# executed effect: it scores PROPOSALS (docs/BENCHMARK-LADDER.md — "Ladder scoring itself stays on
# proposals"), and the TG-84 confirmatory campaign has been accruing pairs on this same estate with
# auto-heal live since the flip. What replaces the stale gate is the set of checks that DO protect this
# benchmark's validity and the estate:
#   1. INJECTOR POOL AGREEMENT — the target is in the operator's pool file WITH a declared unit + log
#      path, AND in TG_PROXMOX_ALLOWED_GUESTS read from the deploy env: a pool/allowlist mismatch
#      manufactures guaranteed misses against the system under test (engine AssertPool, live: searxng01).
#   2. BREAKER CLOSED — the injector engine's own gate (store.go BreakerOpen): when TG's mutation breaker
#      is open the estate is already unhappy and we add no load; UNREADABLE ⇒ treated as OPEN (fail closed).
#   3. GUEST HEALTHY — reachable; hostname matches the pool entry (the bash analogue of AssertGuestName's
#      stale-vmid guard); the declared unit ACTIVE (a stop must be a real state TRANSITION or no alert can
#      fire); root storage_perc below the rule window (the fill must CAUSE the alerting state, not find it).
#   4. BOTH LibreNMS RULES ENABLED (+ a service check on the target) — a disabled rule makes its fault
#      undetectable and the run 0/0-vacuous, which would read as a benchmark miss (the 0/0-miss trap).
#
# tg_may_actuate is still READ — but RECORDED, not gated on: the scorecard carries the posture at start
# and end, and any TG actuation against the target during the window is captured from action_execution as
# an EVENT in the scorecard. An auto-heal mid-window is a fact about the incident both systems triaged —
# the analyst weighs it; refusing to run would mean the ladder can never climb on the estate as it
# actually operates. The eval-gate contention check stays: that one is about the shared model gateway,
# not about mutation, and it is as real today as it was for tier 1.
# ---------------------------------------------------------------------------
echo "# tier2-run — Tier-2 ambiguous multi-signal head-to-head  (host=${TIER2_HOST} stamp=$STAMP dry=$DRY fixture=$([ "$DRY" = 1 ] && echo "$TIER2_DRY_FIXTURE" || echo -))"
echo "## preflight"
for f in "$JUDGE" "$EXTRACT_PRED" "$EXTRACT_TG" "$WATCH"; do
  [ -x "$f" ] || abort "required script missing or not executable: $f"
done
[ -f "$OPSCHEMA" ]      || abort "op-class registry not found: $OPSCHEMA"
[ -f "$EXPECTATIONS" ]  || abort "expectations file not found: $EXPECTATIONS"
[ -f "$TIER2_POOL_FILE" ] || abort "pool file not found: $TIER2_POOL_FILE"
command -v python3 >/dev/null 2>&1 || abort "python3 not on PATH"

check_no_evalgate() {  # gateway contention (unchanged from tier1); dry-run makes no gateway calls
  [ "$DRY" = 1 ] && return 0
  if pgrep -f '[e]val/eval-gate.sh' >/dev/null 2>&1; then
    abort "an eval-gate run is in progress locally — refusing (would contend for the model gateway)"
  fi
}
check_no_evalgate

# --- 1. injector pool agreement -------------------------------------------------------------------
POOL_LINE="$(awk -v h="$TIER2_HOST" '!/^[[:space:]]*(#|$)/ && $2==h {print; exit}' "$TIER2_POOL_FILE")"
[ -n "$POOL_LINE" ] || abort "target '$TIER2_HOST' is not in the pool file $TIER2_POOL_FILE — guinea-pig pool ONLY"
read -r POOL_VMID _ POOL_NODE POOL_CONTAINER POOL_UNIT POOL_LOGPATH _ <<<"$POOL_LINE"
[ "$POOL_CONTAINER" = "-" ] && POOL_CONTAINER=""
[ "${POOL_UNIT:-}" = "-" ] && POOL_UNIT=""
[ "${POOL_LOGPATH:-}" = "-" ] && POOL_LOGPATH=""
[ -n "${POOL_UNIT:-}" ]    || abort "pool entry for $TIER2_HOST declares NO unit — not eligible for service-down (never guessed)"
[ -n "${POOL_LOGPATH:-}" ] || abort "pool entry for $TIER2_HOST declares NO log path — not eligible for log-fill (never guessed)"
# Charset gates for everything later interpolated into remote SQL/shell lines (see validate_token).
validate_token "TIER2_HOST" "$TIER2_HOST" '^[A-Za-z0-9][A-Za-z0-9.-]*$'
validate_token "pool node" "$POOL_NODE" '^[A-Za-z0-9][A-Za-z0-9.-]*$'
validate_token "pool unit" "$POOL_UNIT" '^[A-Za-z0-9@._-]+$'
validate_token "pool vmid" "$POOL_VMID" '^[0-9]+$'
validate_token "RESTORE_AFTER_MIN" "$RESTORE_AFTER_MIN" '^[0-9]+$'
validate_token "TIER2_DISK_PCT" "$TIER2_DISK_PCT" '^9[0-4]$'   # inside the disk rule's bounded [90,95) window
# ValidLogPath's refusals, in bash (tools/faultinjector/plan.go): absolute, no traversal, no metachars or
# whitespace, and NEVER inside an evidence store — a restore that truncates journald/the guard trail/the
# audit log would destroy the record TG's own safety controls depend on.
case "$POOL_LOGPATH" in
  /*) : ;;
  *) abort "log path '$POOL_LOGPATH' is not absolute" ;;
esac
case "$POOL_LOGPATH" in
  *..*) abort "log path '$POOL_LOGPATH' contains a traversal segment" ;;
esac
validate_token "pool log path" "$POOL_LOGPATH" '^/[A-Za-z0-9._/-]+$'
for forb in /var/log/journal/ /run/log/journal/ /var/log/tg-actuator-guard /var/log/audit/ /var/log/wtmp /var/log/btmp /var/log/lastlog; do
  case "$POOL_LOGPATH" in
    "${forb%/}"|"$forb"*) abort "log path '$POOL_LOGPATH' is inside evidence store $forb — refused (ValidLogPath)" ;;
  esac
done
# Allowlist agreement, read from the DEPLOY env (the running system's own config, not a doc stamp).
if [ "$DRY" = 1 ]; then
  ALLOWED="$(grep -E '^TG_PROXMOX_ALLOWED_GUESTS=' "$ENV_PATH" | cut -d= -f2-)"
else
  # TG-350/TG-413 split the allowlist: the ACTUATE plane (which HEALS the fault) reads
  # TG_ACTUATE_PROXMOX_ALLOWED_GUESTS; the triage-plane TG_PROXMOX_ALLOWED_GUESTS is empty post-split.
  # Assert pool agreement against the plane that can heal; fall back to the old key for pre-split estates.
  ALLOWED="$(S "grep -E '^TG_ACTUATE_PROXMOX_ALLOWED_GUESTS=' '$ENV_PATH'" 2>/dev/null | cut -d= -f2-)"
  [ -n "$ALLOWED" ] || ALLOWED="$(S "grep -E '^TG_PROXMOX_ALLOWED_GUESTS=' '$ENV_PATH'" 2>/dev/null | cut -d= -f2-)"
fi
[ -n "$ALLOWED" ] || abort "could not read TG_ACTUATE_PROXMOX_ALLOWED_GUESTS (nor TG_PROXMOX_ALLOWED_GUESTS) from $ENV_PATH — cannot assert pool agreement"
case ",$ALLOWED," in
  *",$TIER2_HOST,"*) : ;;
  *) abort "pool/allowlist mismatch: $TIER2_HOST is in the pool but NOT in the actuate allowlist (TG_ACTUATE_PROXMOX_ALLOWED_GUESTS) — TG structurally cannot heal it; every fault there is a manufactured miss (AssertPool)" ;;
esac
echo "  pool:       $TIER2_HOST (vmid=$POOL_VMID node=$POOL_NODE) unit=$POOL_UNIT log=$POOL_LOGPATH — declared + allowlisted"

# --- 2. breaker closed ----------------------------------------------------------------------------
if [ "$DRY" = 1 ]; then
  BREAKER_STATE="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["breaker_state"])' "$EST_FIX")"
else
  BREAKER_STATE="$(psql_tg "SELECT state FROM mutation_breaker_state WHERE name='mutation'" | tr -dc 'a-z-')"
fi
[ -n "$BREAKER_STATE" ] || abort "cannot read the mutation breaker — treating as OPEN (fail closed, store.go BreakerOpen)"
[ "$BREAKER_STATE" != "open" ] || abort "TG mutation breaker is OPEN — the estate is already unhappy; not adding load"
echo "  breaker:    $BREAKER_STATE"

# --- posture: READ AND RECORD, never gate (the modernization) -------------------------------------
read_posture() {  # exact-series match on tg_may_actuate (tier1's awk, verbatim) — recorded, not gated on
  if [ "$DRY" = 1 ]; then
    python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["tg_may_actuate"])' "$EST_FIX"
    return 0
  fi
  local m
  m="$(S "curl -fsS --max-time 10 http://127.0.0.1:${GROUNDER_PORT}/metrics" 2>/dev/null || true)"
  printf '%s\n' "$m" | awk '
    /^tg_may_actuate([ {]|$)/ { n++; v=$NF }
    END { if (n==1) print v; else if (n>1) print "AMBIG"; else print "ABSENT" }'
}
POSTURE_START="$(read_posture)"
echo "  posture:    tg_may_actuate=$POSTURE_START (recorded, not gated on — scoring stays on proposals; an auto-heal in the window becomes a scorecard EVENT)"

# --- 3. guest healthy -----------------------------------------------------------------------------
if [ "$DRY" = 1 ]; then
  GUEST_HN="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["guest_hostname"])' "$EST_FIX")"
  UNIT_STATE="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["unit_active"])' "$EST_FIX")"
  STORAGE_PERC="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["storage_perc"])' "$EST_FIX")"
  LOG_PRESIZE="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["log_presize_bytes"])' "$EST_FIX")"
else
  GUEST_HN="$(SL "hostname" 2>/dev/null | tr -d '[:space:]')"
  [ -n "$GUEST_HN" ] || abort "guest $TIER2_HOST unreachable — never powered on or faulted blind"
  UNIT_STATE="$(SL "systemctl is-active '$POOL_UNIT'" 2>/dev/null | tr -dc 'a-z-')"
  STORAGE_PERC="$(SL "df -P / | awk 'NR==2{printf \"%d\", \$3*100/\$2}'" 2>/dev/null | tr -dc '0-9')"
  LOG_PRESIZE="$(SL "stat -c %s '$POOL_LOGPATH' 2>/dev/null || echo 0" 2>/dev/null | tr -dc '0-9')"
fi
# Name-assert (AssertGuestName's bash analogue): the guest's own hostname must match the pool entry —
# a stale pool row is indistinguishable from a correct one until the wrong machine is already broken.
case "$GUEST_HN" in
  "$TIER2_HOST"|"${TIER2_HOST%%.*}") : ;;
  *) abort "SAFETY: guest at '$TIER2_HOST' reports hostname '$GUEST_HN' — the pool entry looks stale; refusing to act" ;;
esac
[ "$UNIT_STATE" = "active" ] || abort "unit $POOL_UNIT is '$UNIT_STATE' (not active) on $TIER2_HOST — a stop would be no state transition, so no alert can fire (and the baseline is already broken)"
[ -n "$STORAGE_PERC" ] || abort "could not read root storage_perc on $TIER2_HOST"
[ "$STORAGE_PERC" -lt 90 ] || abort "root already at ${STORAGE_PERC}% (>=90) — the disk signal would pre-exist the fault; nothing valid to inject"
echo "  guest:      hostname=$GUEST_HN unit=$POOL_UNIT($UNIT_STATE) storage_perc=${STORAGE_PERC}% log_presize=${LOG_PRESIZE:-0}B"

# --- 4. BOTH LibreNMS rules enabled ---------------------------------------------------------------
if [ "$DRY" = 1 ]; then
  DISK_RULE_DIS="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["disk_rule_disabled"])' "$EST_FIX")"
  SVC_RULE_DIS="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["service_rule_disabled"])' "$EST_FIX")"
  SVC_CHECKS="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["service_checks"])' "$EST_FIX")"
else
  DISK_RULE_DIS="$(SNMS "mysql -N librenms -e 'select disabled from alert_rules where id=$DISK_RULE_ID;'" 2>/dev/null | tr -dc '0-9')"
  SVC_RULE_DIS="$(SNMS "mysql -N librenms -e 'select disabled from alert_rules where id=$SERVICE_RULE_ID;'" 2>/dev/null | tr -dc '0-9')"
  SVC_CHECKS="$(SNMS "mysql -N librenms -e \"select count(*) from services s join devices d on d.device_id=s.device_id where d.hostname='$TIER2_HOST';\"" 2>/dev/null | tr -dc '0-9')"
fi
[ "$DISK_RULE_DIS" = "0" ] || abort "disk rule id=$DISK_RULE_ID is disabled/absent (got '$DISK_RULE_DIS') — the log-fill would be undetectable (0/0-vacuous)"
[ "$SVC_RULE_DIS" = "0" ]  || abort "service rule id=$SERVICE_RULE_ID is disabled/absent (got '$SVC_RULE_DIS') — the service-down would be undetectable"
if [ -z "$SVC_CHECKS" ] || [ "$SVC_CHECKS" -lt 1 ]; then
  abort "$TIER2_HOST carries no LibreNMS service check — the service rule can never fire for it"
fi
echo "  rules:      disk#$DISK_RULE_ID enabled, service#$SERVICE_RULE_ID enabled, service_checks=$SVC_CHECKS"

WORK="$(mktemp -d)"; mkdir -p "$OUT_DIR"
export WORK SB_DIR
export SCORECARD="$WORK/unused-scorecard.jsonl"   # _driver imports require it; never written here

# --- ground truth: the two injected classes must have DISTINCT correct answers (root-cause selection).
# service-down is read from the SHARED operator ground truth (core/diagcorpus/expectations.json), where it
# is declared healable (accept: start/restart-service). log-fill's answer is carried HERE, harness-local,
# NOT injected into expectations.json — that file is the pre-registered judge-free primary endpoint's
# ruleset (PRE-REGISTRATION.md §1), and adding a class to it moves those faults from Excluded into Scored,
# shifting a published diagnosis-corpus rate. That is a deliberate ground-truth change owed its own reviewed
# MR with a measured before/after, not a side effect of a harness. log-fill's correct answer is a reasoned
# STAND-DOWN for exactly disk-fill's reason (no registered op-class reclaims log-grown space; the honest
# remedy verb is deliberately unregistered — tools/faultinjector/plan.go ClassLogFill.Provokes() is nil).
# When that reclaim verb ships and log-fill earns its own expectations.json entry, this block should read it
# from there instead.
SB_EXPECT="$EXPECTATIONS" python3 - <<'PY' > "$WORK/gt.json" || abort "ground-truth check failed (see above)"
import json, os, sys
doc = json.load(open(os.environ["SB_EXPECT"]))
byclass = {e.get("fault_type"): e for e in doc.get("expectations", [])}
svc = byclass.get("service-down")
if svc is None:
    print("service-down is NOT declared in expectations.json — tier 2 needs its healable ground truth",
          file=sys.stderr); sys.exit(1)
if svc.get("unhealable") or svc.get("stand_down_is_correct") or not (svc.get("accept") or []):
    print("service-down must be a HEALABLE class (non-empty accept) to anchor the 'root cause selected + "
          f"acted' side of the tier-2 pair; got {svc!r}", file=sys.stderr); sys.exit(1)
gt = {
    "service-down": {"accept": sorted(svc.get("accept") or []), "stand_down_is_correct": False,
                     "source": "expectations.json (shared operator ground truth)"},
    # HARNESS-LOCAL, mirrors disk-fill's declared stand_down_is_correct; see the block comment above.
    "log-fill": {"accept": [], "stand_down_is_correct": True,
                 "source": "harness-local (not injected into the pre-registered expectations.json)"},
}
a, b = gt["service-down"], gt["log-fill"]
if (a["accept"], a["stand_down_is_correct"]) == (b["accept"], b["stand_down_is_correct"]):
    print("the two classes resolve to the SAME correct answer — no root-cause selection to measure",
          file=sys.stderr); sys.exit(1)
json.dump(gt, sys.stdout)
PY
echo "  ground truth: service-down (heal, from expectations.json) vs log-fill (stand-down, harness-local) — distinct"

# ---------------------------------------------------------------------------
# 1. Ledger record (durable-record-BEFORE-effect), then inject BOTH faults.
# ---------------------------------------------------------------------------
if [ "$DRY" = 1 ]; then
  BASELINE="2000-01-01 00:00:00"; BASELINE_EPOCH=946684800
else
  BASELINE="$(date -u -d "-${SKEW_SLACK} seconds" '+%Y-%m-%d %H:%M:%S')"
  BASELINE_EPOCH="$(date -u -d "-${SKEW_SLACK} seconds" '+%s')"
fi
DATE="$(date -u '+%Y-%m-%d')"
LEDGER_NOTE="tier2 TG-72 stamp=$STAMP concurrent-pair"
echo "## inject   (since-boundary=$BASELINE UTC)"

svc_landed=0; log_landed=0; svc_detail=""; log_detail=""; LOG_ALLOC=0; LOG_ACHIEVED=""
if [ "$DRY" = 1 ]; then
  cp "$INJ_FIX" "$WORK/inject.json"
  svc_landed="$(python3 -c 'import json,sys; print(1 if json.load(open(sys.argv[1]))["service_down"]["landed"] else 0)' "$WORK/inject.json")"
  log_landed="$(python3 -c 'import json,sys; print(1 if json.load(open(sys.argv[1]))["log_fill"]["landed"] else 0)' "$WORK/inject.json")"
  echo "  [dry-run] fixture $INJ_FIX: service_down landed=$svc_landed log_fill landed=$log_landed"
else
  ledger_insert() {  # $1 fault_type  $2 fault_ref  -> id on stdout (values charset-gated in preflight)
    # RETURNING id emits the id on line 1, then psql's "INSERT 0 1" command tag on line 2. Take the FIRST
    # line before stripping to digits — else `tr -dc 0-9` concatenates the tag's digits (id 1426 -> 142601),
    # and the restore-mark UPDATE (WHERE id=<captured>) then misses, leaving the obligation 'pending' (a
    # spurious reconciler restore fires at the dead-man deadline). See TG-72.
    psql_tg "INSERT INTO injected_fault (host, fault_type, note, restore_state, restore_due_at, fault_ref, node) VALUES ('$TIER2_HOST','$1','$LEDGER_NOTE','pending', now() + interval '$RESTORE_AFTER_MIN minutes','$2','$POOL_NODE') RETURNING id" | head -1 | tr -dc '0-9'
  }
  SVC_LEDGER_ID="$(ledger_insert service-down "$POOL_UNIT")"
  LOG_LEDGER_ID="$(ledger_insert log-fill "$POOL_LOGPATH")"
  # An unrecorded fault is how a guest gets stranded — refuse to inject without both obligations durable.
  [ -n "$SVC_LEDGER_ID" ] || abort "could not record the service-down obligation — NOT injecting"
  [ -n "$LOG_LEDGER_ID" ] || abort "could not record the log-fill obligation — NOT injecting"
  echo "  ledger:     service-down id=$SVC_LEDGER_ID, log-fill id=$LOG_LEDGER_ID (restore due in ${RESTORE_AFTER_MIN}m; reconciler safety-net armed)"

  # -- service-down: stop the declared unit; landed only on a verified state transition.
  SVC_ARMED=1
  SL "systemctl stop '$POOL_UNIT'" >/dev/null 2>&1
  sleep 2
  st="$(SL "systemctl is-active '$POOL_UNIT'" 2>/dev/null | tr -dc 'a-z-')"
  if [ -n "$st" ] && [ "$st" != "active" ]; then
    svc_landed=1; svc_detail="is-active=$st"
    echo "  service-down: $POOL_UNIT stopped ($st)"
  else
    svc_detail="unit still '$st' after stop (or unreadable)"
    echo "  service-down: FAILED to land — $svc_detail"
  fi

  # -- log-fill: the engine's own arithmetic (inject.go planDiskFill): target = size*pct/100 over the
  #    FULL size (the rule's storage_perc metric, NOT df Use%), refuse at/above target (pre-effect) and
  #    refuse to leave < 128MiB — a guest with no headroom cannot run the diagnostics under test.
  read -r FS_SIZE FS_USED <<<"$(SL "df -B1 --output=size,used / | awk 'NR==2{print \$1, \$2}'" 2>/dev/null)"
  if [ -z "${FS_SIZE:-}" ] || [ -z "${FS_USED:-}" ]; then
    log_detail="could not read df — nothing allocated (pre-effect)"
    ledger_mark "$LOG_LEDGER_ID" restored ""   # provably nothing broken — close early (ErrPreEffect rule)
  else
    TARGET_USED=$(( FS_SIZE * TIER2_DISK_PCT / 100 ))
    LOG_ALLOC=$(( TARGET_USED - FS_USED ))
    FREE_AFTER=$(( FS_SIZE - TARGET_USED ))
    if [ "$LOG_ALLOC" -le 0 ]; then
      log_detail="already at/above ${TIER2_DISK_PCT}% — pre-effect refusal, nothing allocated"
      ledger_mark "$LOG_LEDGER_ID" restored ""
    elif [ "$FREE_AFTER" -lt 134217728 ]; then
      log_detail="fill would leave <128MiB free — refusing to wedge the guest (pre-effect)"
      ledger_mark "$LOG_LEDGER_ID" restored ""
    else
      LOG_ARMED=1
      SL "mkdir -p -- '$(dirname "$POOL_LOGPATH")' && fallocate -l '$LOG_ALLOC' '$POOL_LOGPATH'" >/dev/null 2>&1
      LOG_ACHIEVED="$(SL "df -P / | awk 'NR==2{printf \"%d\", \$3*100/\$2}'" 2>/dev/null | tr -dc '0-9')"
      if [ -n "$LOG_ACHIEVED" ] && [ "$LOG_ACHIEVED" -ge 90 ] && [ "$LOG_ACHIEVED" -lt 95 ]; then
        log_landed=1; log_detail="grew $POOL_LOGPATH by ${LOG_ALLOC}B; storage_perc=${LOG_ACHIEVED}% inside [90,95)"
        echo "  log-fill:   $log_detail"
      else
        log_detail="achieved '${LOG_ACHIEVED:-?}%' OUTSIDE the rule window [90,95) — not a detectable fault"
        echo "  log-fill:   FAILED to land — $log_detail"
      fi
    fi
  fi
  [ "$log_landed" = 1 ] || echo "  log-fill:   $log_detail"
  SB_SVC_LANDED="$svc_landed" SB_SVC_DETAIL="$svc_detail" SB_LOG_LANDED="$log_landed" \
  SB_LOG_DETAIL="$log_detail" SB_LOG_ALLOC="$LOG_ALLOC" SB_LOG_ACHIEVED="${LOG_ACHIEVED:-}" python3 - <<'PY' > "$WORK/inject.json"
import json, os
print(json.dumps({
  "service_down": {"landed": os.environ["SB_SVC_LANDED"] == "1", "detail": os.environ["SB_SVC_DETAIL"]},
  "log_fill": {"landed": os.environ["SB_LOG_LANDED"] == "1", "detail": os.environ["SB_LOG_DETAIL"],
               "alloc_bytes": int(os.environ["SB_LOG_ALLOC"] or 0),
               "achieved_pct": int(os.environ["SB_LOG_ACHIEVED"] or 0) or None}}))
PY
fi

emit_scorecard() {  # $1 STATUS  $2 EXIT_CODE — assembles the tier-2 scorecard from whatever WORK holds
  SB_STATUS="$1" SB_EXIT="$2" SB_OUT="$SCORECARD_OUT" SB_STAMP="$STAMP" SB_HOST="$TIER2_HOST" \
  SB_UNIT="$POOL_UNIT" SB_LOGPATH="$POOL_LOGPATH" SB_BASELINE="$BASELINE" SB_MODE="$([ "$DRY" = 1 ] && echo dry-run || echo real)" \
  SB_FIXTURE="$([ "$DRY" = 1 ] && echo "$TIER2_DRY_FIXTURE" || echo "")" SB_BREAKER="$BREAKER_STATE" \
  SB_POSTURE_START="$POSTURE_START" SB_POSTURE_END="$POSTURE_END" SB_KEY="$KEY" SB_NOTE="$NOTE" \
  SB_SVC_RESTORE="$SVC_RESTORE" SB_LOG_RESTORE="$LOG_RESTORE" \
  SB_SVC_LEDGER="$SVC_LEDGER_ID" SB_LOG_LEDGER="$LOG_LEDGER_ID" \
  SB_OPSCHEMA="$OPSCHEMA" SB_TG_OK="$tg_ok" SB_PRED_OK="$pred_ok" python3 - <<'PY'
import json, os, re

W = os.environ["WORK"]
def load(name, default):
    try:
        with open(os.path.join(W, name), encoding="utf-8") as fh:
            return json.load(fh)
    except Exception:
        return default

def norm(s):  # the SAME normalization _driver.fault_class applies, so patterns match both spellings
    return re.sub(r"[^a-z0-9]+", " ", (s or "").lower()).strip()

# --- single_cause_ok: the tier-2 MECHANICAL check — exactly ONE op-class per proposal. The vocabulary
# is the CLOSED embedded registry (core/actuate/opschema/opschema.json): per class, the slug (hyphen and
# space spellings collapse under norm()) plus the argv head ("systemctl restart", ...), matched on word
# boundaries so `restart-service` can never also count as start-service. Surfaces are the COMMITMENT
# surfaces only — TG's structured `op` field + its conclusion, the predecessor's rationale (conclusion) —
# never the reasoning trajectory, where explored-and-rejected alternatives legitimately live. count==0
# is a stand-down (no proposal to bundle), count>=2 is the bundling the discipline forbids; both fail
# the strict ==1 boolean, and the count+list are recorded so a reader can tell which.
reg = json.load(open(os.environ["SB_OPSCHEMA"]))
pats = {}
for oc in reg.get("op_classes", []):
    slug = oc.get("op_class", "")
    p = {norm(slug)}
    argv = oc.get("argv_template") or []
    if len(argv) >= 2:
        p.add(norm(" ".join(argv[:2])))
    pats[slug] = {re.compile(r"\b" + re.escape(x) + r"\b") for x in p if x}

def scan(text):
    t = norm(text)
    return {slug for slug, rxs in pats.items() if any(rx.search(t) for rx in rxs)}

def single_cause(rec, side):
    if rec is None:
        return None
    found = set()
    if side == "tg":
        op = (rec.get("op") or "").strip()
        if op:
            hit = scan(op)
            found |= hit if hit else {"unregistered:" + op}
        found |= scan(rec.get("conclusionExcerpt"))
        surface = "op field + conclusionExcerpt"
    else:
        found |= scan(rec.get("rationale"))
        surface = "rationale (conclusion)"
    return {"ok": len(found) == 1, "count": len(found), "op_classes": sorted(found), "surface": surface}

j_tg = load("j_tg.json", None)
j_pred = load("j_pred.json", None)
card = {
    "tier": "2",
    "stamp": os.environ["SB_STAMP"],
    "mode": os.environ["SB_MODE"],
    "fixture": os.environ["SB_FIXTURE"] or None,
    "host": os.environ["SB_HOST"],
    "baseline_utc": os.environ["SB_BASELINE"],
    "status": os.environ["SB_STATUS"],
    "exit_code": int(os.environ["SB_EXIT"]),
    "refused_to_score": os.environ["SB_STATUS"] == "HALF-INJECTION",
    "note": os.environ["SB_NOTE"] or None,
    "posture": {
        "breaker_state": os.environ["SB_BREAKER"],
        "tg_may_actuate_start": os.environ["SB_POSTURE_START"],
        "tg_may_actuate_end": os.environ["SB_POSTURE_END"] or None,
        "rationale": "scoring is on PROPOSALS (docs/BENCHMARK-LADDER.md); live posture is owner-set "
                     "Semi-auto, so mutation-ON does not invalidate the run — a TG actuation in the "
                     "window is recorded in autoheal_events, never a refusal (TG-72)",
    },
    "faults": {
        "service_down": dict(load("inject.json", {}).get("service_down", {}),
                             unit=os.environ["SB_UNIT"],
                             ledger_id=int(os.environ["SB_SVC_LEDGER"] or 0) or None,
                             restore=os.environ["SB_SVC_RESTORE"]),
        "log_fill": dict(load("inject.json", {}).get("log_fill", {}),
                         path=os.environ["SB_LOGPATH"],
                         ledger_id=int(os.environ["SB_LOG_LEDGER"] or 0) or None,
                         restore=os.environ["SB_LOG_RESTORE"]),
    },
    "ground_truth": load("gt.json", {}),
    "detection": dict(load("detect.json", {}),
                      tg_triaged=os.environ["SB_TG_OK"] == "1",
                      pred_triaged=os.environ["SB_PRED_OK"] == "1"),
    "autoheal_events": load("events.json", []),
    "selection": {"incident_key": os.environ["SB_KEY"] or None,
                  "tg_ref": (j_tg or {}).get("external_ref"),
                  "pred_key": (j_pred or {}).get("incidentKey")},
    "single_cause": {"tg": single_cause(j_tg, "tg"), "pred": single_cause(j_pred, "pred")},
    "judge": load("judge.json", None),
}
with open(os.environ["SB_OUT"], "w", encoding="utf-8") as fh:
    json.dump(card, fh, indent=2, ensure_ascii=False)
    fh.write("\n")
sc = card["single_cause"]
print("  scorecard: %s" % os.environ["SB_OUT"])
print("  single_cause_ok: tg=%s pred=%s" % (
    "n/a" if sc["tg"] is None else sc["tg"]["ok"],
    "n/a" if sc["pred"] is None else sc["pred"]["ok"]))
PY
}

# --- HALF-INJECTION: refuse to score (exit 6, distinct) — no ambiguity means no tier-2 experiment.
if [ "$svc_landed" -ne 1 ] || [ "$log_landed" -ne 1 ]; then
  STATUS="HALF-INJECTION"; EXIT_CODE=6
  NOTE="only $(( svc_landed + log_landed )) of 2 faults landed (service=$svc_landed log=$log_landed) — a half-injection proves nothing; refusing to score"
  echo "  !! $NOTE"
  restore_all
  POSTURE_END="$(read_posture)"
  emit_scorecard "$STATUS" "$EXIT_CODE"
  echo ""
  echo "==================== TIER-2 SUMMARY ===================="
  echo " result:     HALF-INJECTION (exit 6 — REFUSED TO SCORE)"
  echo " note:       $NOTE"
  echo " restores:   service=$SVC_RESTORE  log=$LOG_RESTORE"
  echo " scorecard:  $SCORECARD_OUT (documents the refusal; carries no score)"
  echo "========================================================"
  exit "$EXIT_CODE"
fi

# ---------------------------------------------------------------------------
# 2+3. Observability + sanctioned detection + poll for BOTH systems.
# ---------------------------------------------------------------------------
if [ "$DRY" = 0 ]; then
  echo "## outage-watch (background) -> $WATCH_LOG"
  "$WATCH" "$WATCH_MIN" "$WATCH_INT" >"$WATCH_LOG" 2>&1 &
  WATCH_PID=$!

  # The sanctioned path (campaign.sh): device:poll runs the AlertRules evaluator (legacy poller.php -h
  # does NOT), twice across the rule delay; check-services.php refreshes the service check; alerts.php
  # dispatches to both transports. The alerts table itself is NEVER written.
  echo "## force-detect (device:poll x2 + check-services + alerts.php — sanctioned, no alert-table writes)"
  SNMS "sudo -u librenms bash -lc 'cd /opt/librenms && php artisan device:poll $TIER2_HOST </dev/null >/dev/null 2>&1'" >/dev/null 2>&1 || true
  SNMS "sudo -u librenms bash -lc 'cd /opt/librenms && php check-services.php -h $TIER2_HOST >/dev/null 2>&1'" >/dev/null 2>&1 || true
  sleep 65
  SNMS "sudo -u librenms bash -lc 'cd /opt/librenms && php artisan device:poll $TIER2_HOST </dev/null >/dev/null 2>&1'" >/dev/null 2>&1 || true
  SNMS "sudo -u librenms bash -lc 'cd /opt/librenms && php check-services.php -h $TIER2_HOST >/dev/null 2>&1'" >/dev/null 2>&1 || true
  SNMS "sudo -u librenms bash -lc 'cd /opt/librenms && php alerts.php >/dev/null 2>&1'" >/dev/null 2>&1 || true

  HKW="$(echo "$TIER2_HOST" | sed -E 's/^[a-z]+[0-9]+//; s/[0-9]+$//')"
  tg_count() {  # $1 = extra alert_rule filter; counts host-scoped triage rows since baseline
    psql_tg "select count(*) from session_triage where created_at > '$BASELINE' and (host ilike '%${TIER2_HOST}%' or host ilike '%${HKW}%' or external_ref ilike '%${HKW}%') and ($1)" | tr -dc '0-9'
  }
  SVC_FILTER="alert_rule ilike '%service%' or alert_rule ilike '%http%' or alert_rule ilike '%${POOL_UNIT}%'"
  DISK_FILTER="alert_rule ilike '%space%' or alert_rule ilike '%disk%' or alert_rule ilike '%storage%'"
  pred_count() {  # agentic shadow-log sessions naming the target since baseline (campaign.sh's probe)
    local f
    f="$SHADOW_DIR/shadow-$(date -u '+%Y-%m-%d').jsonl"
    [ -f "$f" ] || { echo 0; return; }
    SB_BASE="$BASELINE_EPOCH" SB_HOST="$TIER2_HOST" python3 - "$f" <<'PY' 2>/dev/null | tr -dc '0-9'
import sys, json, os, re
f = sys.argv[1]; base = int(os.environ.get("SB_BASE", "0") or 0)
host = (os.environ.get("SB_HOST", "") or "").lower()
kw = re.sub(r"[0-9]+$", "", re.sub(r"^[a-z]+[0-9]+", "", host)) or host
sess = set()
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
        if sid:
            sess.add(str(sid))
print(len(sess))
PY
  }

  echo "## poll  (budget=${POLL_TIMEOUT}s @ ${POLL_INTERVAL}s; SNMP storage/service polls ~5min + rule delay)"
  DEADLINE=$(( $(date +%s) + POLL_TIMEOUT )); iter=0
  while :; do
    now="$(date +%s)"; [ "$now" -lt "$DEADLINE" ] || break
    iter=$((iter + 1))
    tsvc="$(tg_count "$SVC_FILTER")"; tsvc="${tsvc:-0}"
    tdsk="$(tg_count "$DISK_FILTER")"; tdsk="${tdsk:-0}"
    pc="$(pred_count)"; pc="${pc:-0}"
    [ $(( ${tsvc:-0} + ${tdsk:-0} )) -gt 0 ] 2>/dev/null && tg_ok=1
    { [ "${tsvc:-0}" -gt 0 ] && [ "${tdsk:-0}" -gt 0 ]; } 2>/dev/null && tg_full=1
    [ "${pc:-0}" -gt 0 ] 2>/dev/null && pred_ok=1
    printf '  [poll %02d | %s UTC] TG svc=%s disk=%s (ok=%d full=%d)  pred=%s (ok=%d)  remaining=%ss\n' \
      "$iter" "$(date -u '+%H:%M:%S')" "$tsvc" "$tdsk" "$tg_ok" "$tg_full" "$pc" "$pred_ok" "$(( DEADLINE - now ))"
    # TG-72: exit on TG's FULL multi-signal (BOTH the service AND the disk), not any signal. The LibreNMS
    # service-check (rule 9) fires ~300s after inject — far slower than the disk poll — so the old "any signal"
    # exit banked a partial (service:0) multi-signal the moment the fast disk triaged, forcing a ONE-SIDED
    # result (first live run 2026-08-15). Waiting for tsvc>0 lets the slow service-check land before banking;
    # the deadline still bounds it, and tg_ok remains the honest "TG triaged something" flag for the scorecard.
    { [ "$tg_full" -eq 1 ] && [ "$pred_ok" -eq 1 ]; } && { echo "  both systems triaged the FULL multi-signal incident."; break; }
    sleep "$POLL_INTERVAL"
  done
else
  tg_ok=1; pred_ok=1   # fixtures below carry the sessions; the poll is a live-estate concern
fi

# ---------------------------------------------------------------------------
# RESTORE BOTH NOW — the data is banked (or provably absent); the fault window must not stay open
# through extraction and judging. The trap remains the net for anything after this point.
# ---------------------------------------------------------------------------
[ "$DRY" = 0 ] && echo "## restore (immediately after the poll — the window closes when the data is banked)"
restore_all
[ -n "${WATCH_PID:-}" ] && { kill "$WATCH_PID" 2>/dev/null; WATCH_PID=""; }
POSTURE_END="$(read_posture)"

# Auto-heal events over the fault window: any TG execution against the target is a recorded EVENT in the
# scorecard (the modernized posture rule) — visible to the analyst, never a refusal.
if [ "$DRY" = 1 ]; then
  python3 -c 'import json,sys; json.dump(json.load(open(sys.argv[1]))["autoheal_events"], open(sys.argv[2], "w"))' "$EST_FIX" "$WORK/events.json"
else
  psql_tg "select coalesce(json_agg(row_to_json(e)),'[]') from (select action_id, external_ref, verdict::text as verdict, unverifiable, target_host, executed_at from action_execution where (target_host ilike '%${TIER2_HOST}%' or target_host ilike '%${HKW}%') and executed_at > '$BASELINE' order by executed_at) e" > "$WORK/events.json" 2>/dev/null
  python3 - "$WORK/events.json" <<'PY'
import json, sys
p = sys.argv[1]
try:
    t = open(p).read().strip(); assert t; json.loads(t)
except Exception:
    open(p, "w").write("[]")
PY
fi

# ---------------------------------------------------------------------------
# 4. Extract + STRICT tier-2 select + judge + scorecard.
# ---------------------------------------------------------------------------
if [ "$tg_ok" -eq 0 ] && [ "$pred_ok" -eq 0 ]; then
  STATUS="FAIL"; EXIT_CODE=1
  NOTE="neither system triaged the multi-signal incident within the ${POLL_TIMEOUT}s budget"
  echo "  timeout: NEITHER system triaged — nothing to judge."
  emit_scorecard "$STATUS" "$EXIT_CODE"
else
  echo "## extract + select + judge"
  if [ "$DRY" = 1 ]; then
    cp "$FIX_DIR/tg-sessions.json" "$WORK/tg.json"
    cp "$FIX_DIR/pred-incidents.json" "$WORK/pred.json"
  else
    timeout 180 python3 "$EXTRACT_PRED" --date "$DATE" --json > "$WORK/pred.json" 2>"$WORK/pred.err" \
      || { echo "  WARN: extract_predecessor failed/timed out"; echo '{"incidents":[]}' > "$WORK/pred.json"; }
    TG_FORMAT=jsononly SHADOW_FROM="${BASELINE}+00" timeout 180 "$EXTRACT_TG" > "$WORK/tg.json" 2>"$WORK/tg.err" \
      || { echo "  WARN: extract_tg failed/timed out"; echo '[]' > "$WORK/tg.json"; }
  fi

  # STRICT tier-2 selection. Same host + since-baseline + ONLY the two injected classes, classified by
  # _driver.fault_class — the SAME classifier that forms campaign pairs, so "service" and "disk" mean
  # here exactly what they mean in the confirmatory harness. No time/host/rule fallback may cross-pair
  # an unrelated incident (the tier-1 audit's #1 finding); newest-in-window wins deterministically.
  SB_HOST_SEL="$TIER2_HOST" SB_BASELINE_SEL="$BASELINE" python3 - <<'PY'
import json, os, sys
sys.path.insert(0, os.environ["SB_DIR"])
import _driver as d  # fault_class + _parse_ts + _host_match: the campaign aligner's own primitives

W = os.environ["WORK"]; host = os.environ["SB_HOST_SEL"].lower()
base = d._parse_ts(os.environ["SB_BASELINE_SEL"])
TIER2_CLASSES = {"service", "disk"}   # service-down alerts class "service"; log-fill raises the disk rule

def since(ts):
    return base is None or ts is None or ts >= base

try:
    tg = json.load(open(os.path.join(W, "tg.json")))
    tg = tg if isinstance(tg, list) else []
except Exception:
    tg = []
tg_c = [r for r in tg
        if d._host_match(host, r.get("host")) and since(d._parse_ts(r.get("createdAt")))
        and d.fault_class(r.get("alertRule")) in TIER2_CLASSES]
tg_sel = max(tg_c, key=lambda r: d._parse_ts(r.get("createdAt")) or base, default=None)

try:
    incs = json.load(open(os.path.join(W, "pred.json"))).get("incidents") or []
except Exception:
    incs = []
pred_c = [i for i in incs
          if d.is_real_pred(i) and d._host_match(host, d.pred_subject_host(i))
          and since(d._parse_ts(i.get("firstTs")))
          and d.fault_class(" ".join(str(i.get(k) or "") for k in ("issue", "incidentKey", "alertCategory")))
          in TIER2_CLASSES]
pred_sel = max(pred_c, key=lambda i: d._parse_ts(i.get("firstTs")) or base, default=None)

detect = {
    "tg": {c: sum(1 for r in tg_c if d.fault_class(r.get("alertRule")) == c) for c in sorted(TIER2_CLASSES)},
    "tg_refs": sorted({r.get("external_ref") or "" for r in tg_c} - {""}),
    "pred": {c: sum(1 for i in pred_c
                    if d.fault_class(" ".join(str(i.get(k) or "") for k in ("issue", "incidentKey", "alertCategory"))) == c)
             for c in sorted(TIER2_CLASSES)},
    "pred_keys": sorted({i.get("incidentKey") or "" for i in pred_c} - {""}),
}
json.dump(detect, open(os.path.join(W, "detect.json"), "w"))
if tg_sel is not None:
    json.dump(tg_sel, open(os.path.join(W, "j_tg.json"), "w"))
if pred_sel is not None:
    json.dump(pred_sel, open(os.path.join(W, "j_pred.json"), "w"))
key = (tg_sel or {}).get("external_ref") or (pred_sel or {}).get("incidentKey") or ("tier2-" + host)
open(os.path.join(W, "key.txt"), "w").write(key)
open(os.path.join(W, "have.txt"), "w").write(
    ("yes" if tg_sel is not None else "no") + " " + ("yes" if pred_sel is not None else "no"))
print("  selected: tg=%s pred=%s key=%s  (tg per-class %s; pred per-class %s)" % (
    "yes" if tg_sel else "no", "yes" if pred_sel else "no", key, detect["tg"], detect["pred"]))
PY

  KEY="$(cat "$WORK/key.txt" 2>/dev/null)"
  read -r HAVE_TG HAVE_PRED < "$WORK/have.txt"

  if [ "$HAVE_TG" = "yes" ] && [ "$HAVE_PRED" = "yes" ]; then
    check_no_evalgate     # TOCTOU: an eval-gate may have started during the poll window
    JUDGE_ARGS=(--incident-key "$KEY" --model "$MODEL" --ssh-key "$SSH_KEY" --tg-host "$TG_HOST"
                --ssh-user "$SSH_USER" --env-path "$ENV_PATH" --out "$WORK/judge.json"
                --pred "$WORK/j_pred.json" --tg "$WORK/j_tg.json")
    [ "$DRY" = 1 ] && JUDGE_ARGS+=(--dry-run)
    echo "  judging incident '$KEY' (pred=yes tg=yes)$([ "$DRY" = 1 ] && echo ' [dry-run: prompt built, model NOT called]')"
    timeout 420 python3 "$JUDGE" "${JUDGE_ARGS[@]}" >"$WORK/judge.stdout" 2>"$WORK/judge.err" || true
    SCORED="$(python3 - "$WORK/judge.json" <<'PY'
import json, sys
try:
    v = json.load(open(sys.argv[1]))
except Exception:
    print("no"); sys.exit(0)
if v.get("dry_run"):
    print("dry"); sys.exit(0)
if v.get("judge_unavailable"):
    print("no"); sys.exit(0)
dims = v.get("dims") or {}
def side_ok(letter):
    dd = dims.get(letter) or {}
    return isinstance(dd, dict) and any(isinstance(x, (int, float)) for x in dd.values())
print("yes" if side_ok("A") and side_ok("B") and (v.get("winner") not in (None, "")) else "no")
PY
)"
    case "$SCORED" in
      yes) STATUS="PASS"; EXIT_CODE=0 ;;
      dry) STATUS="DRY-PASS"; EXIT_CODE=0; NOTE="dry-run: full flow proven against fixtures; judge prompt built, model not called" ;;
      *)   STATUS="FAIL"; EXIT_CODE=1
           NOTE="both sides present but the judge did not score BOTH sides + a winner — see judge.err" ;;
    esac
  else
    STATUS="ONE-SIDED"; EXIT_CODE=3
    NOTE="only one system triaged the tier-2 incident (tg=$HAVE_TG pred=$HAVE_PRED) — NOT a valid head-to-head"
  fi
  emit_scorecard "$STATUS" "$EXIT_CODE"
fi

# ---------------------------------------------------------------------------
# 5. Summary.
# ---------------------------------------------------------------------------
echo ""
echo "==================== TIER-2 SUMMARY ===================="
echo " result:      ${STATUS}"
echo " incident:    ${KEY:-<none>}   (host $TIER2_HOST: service-down[$POOL_UNIT] + log-fill[$POOL_LOGPATH])"
echo " since UTC:   $BASELINE"
echo " TG triaged:  $([ "$tg_ok" -eq 1 ] && echo yes || echo no)    pred triaged: $([ "$pred_ok" -eq 1 ] && echo yes || echo no)"
echo " restores:    service=$SVC_RESTORE  log=$LOG_RESTORE"
echo " posture:     tg_may_actuate start=$POSTURE_START end=${POSTURE_END:-?}  breaker=$BREAKER_STATE"
echo " scorecard:   $SCORECARD_OUT"
[ "$DRY" = 0 ] && echo " outage-watch: $WATCH_LOG"
[ -n "${NOTE:-}" ] && echo " note:        $NOTE"
case "$STATUS" in
  PASS)      echo " => PASS: both faults landed, both systems triaged, and the blind judge scored BOTH sides." ;;
  DRY-PASS)  echo " => DRY-PASS: the full flow ran against fixtures; nothing was injected." ;;
  ONE-SIDED) echo " => ONE-SIDED: only one system triaged within budget; NOT a valid head-to-head." ;;
  *)         echo " => FAIL: no scored head-to-head produced." ;;
esac
echo "========================================================"
[ "$RESTORE_FAILED" -eq 1 ] && EXIT_CODE=4
exit "${EXIT_CODE:-1}"
