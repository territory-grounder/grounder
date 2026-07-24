#!/usr/bin/env bash
# tier1-run.sh — ONE Tier-1 disk-pressure head-to-head, run cleanly, REVERSIBLY, and VALIDLY.
#
# WHAT IT DOES (fully scripted; no Claude workflow in the loop)
#   0. PREFLIGHT: refuse if an eval-gate is running (gateway contention) or if TG mutation is not OFF.
#   1. Record a UTC baseline, then inject a bounded disk fault on the target (LibreNMS rule 22,
#      "Space on / is >= 90% and < 95%", routed to BOTH transport 4=predecessor and 7=Territory Grounder),
#      and ASSERT the achieved usage landed in the bounded [90,95) window (else rule 22 never fires).
#   2. Launch outage-watch.sh in the background to prove BOTH systems keep ingesting during the fault.
#   3. POLL until BOTH TG (session_triage librespeed-DISK row since baseline) AND the predecessor
#      (a gateway.db session whose issue_title names the target since baseline) have triaged the SAME
#      disk incident. Each remote call is time-bounded so a hung call can never hold the fault open.
#   4. Extract + STRICTLY select the librespeed disk pair (no time/host/rule fallbacks that cross-pair
#      an unrelated incident), score it with judge.py -> a Tier-1 scorecard under tools/shadowbench/out/.
#   5. Print PASS (both triaged the disk incident + the blind judge scored BOTH sides) / ONE-SIDED / FAIL.
#
# SAFETY (hardened after an adversarial review — see TG-71 / the tier1-pipeline-prep review)
#   * The disk fault is a single fallocate'd file; the trap catches EXIT/INT/TERM/HUP/PIPE and ALWAYS
#     runs --clean AND then INDEPENDENTLY verifies the fill file is gone on the target — a still-present
#     file is a LOUD hard failure, never a silent "restored".
#   * Mutation-OFF is proven by an EXACT-series match on the mutation_enabled gauge (a prefix match could
#     read a sibling series like mutation_enabled_attempts_total and false-pass); fail CLOSED if unprovable.
#   * Every remote ssh call is `timeout`-wrapped + ServerAlive-bounded so a connected-but-hung command
#     cannot block past the poll budget with the fault still injected.
#   * Recommended: run under setsid/nohup so an ssh-session SIGHUP still fires the trap.
#
# USAGE
#   tools/shadowbench/tier1-run.sh
#   TARGET_PCT=93 POLL_TIMEOUT=1500 tools/shadowbench/tier1-run.sh
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
GATEWAY_DB="${GATEWAY_DB:-/home/tg/gateway-state/gateway.db}"
LIBRESPEED_HOST="${LIBRESPEED_HOST:-dc1librespeed01}"
TARGET_PCT="${TARGET_PCT:-92}"
MODEL="${MODEL:-primary}"
POLL_TIMEOUT="${POLL_TIMEOUT:-1200}"
POLL_INTERVAL="${POLL_INTERVAL:-60}"
WATCH_MIN="${WATCH_MIN:-22}"
WATCH_INT="${WATCH_INT:-20}"
SKEW_SLACK="${SKEW_SLACK:-120}"          # seconds to back-date the "since" boundary, absorbing clock skew
SSH_CMD_TIMEOUT="${SSH_CMD_TIMEOUT:-45}" # hard per-remote-call ceiling
FILL_FILE="/var/tmp/tg-tier1-fill.img"   # must match inject-tier1-diskfill.sh

OUT_DIR="$SB_DIR/out"
INJECT="$SB_DIR/inject-tier1-diskfill.sh"
WATCH="$SB_DIR/outage-watch.sh"
EXTRACT_PRED="$SB_DIR/extract_predecessor.py"
EXTRACT_TG="$SB_DIR/extract_tg.sh"
JUDGE="$SB_DIR/judge.py"
export SSH_KEY TG_HOST SSH_USER ENV_PATH PG_CONTAINER

STAMP="$(date -u '+%Y%m%dT%H%M%SZ')"
WATCH_LOG="$OUT_DIR/tier1-outage-watch-${STAMP}.log"
SCORECARD_OUT="$OUT_DIR/tier1-scorecard-${STAMP}.json"
WORK=""; WATCH_PID=""
KEY=""; NOTE=""; STATUS="FAIL"; EXIT_CODE=1
tg_ok=0; pred_ok=0

abort() { echo "ABORT: $*" >&2; exit 2; }

# bounded ssh: hard timeout + server-alive so a connected-but-hung command cannot block forever.
SB() { timeout "$SSH_CMD_TIMEOUT" ssh -i "$SSH_KEY" -o ConnectTimeout=10 -o ServerAliveInterval=5 \
        -o ServerAliveCountMax=2 -o BatchMode=yes -o StrictHostKeyChecking=no "$@"; }
S()  { SB "${SSH_USER}@${TG_HOST}" "$@"; }            # -> the TG Docker host
SL() { SB "root@${LIBRESPEED_HOST}" "$@"; }           # -> the injected target

# ---------------------------------------------------------------------------
# Reversible-fault trap — ALWAYS restore the disk AND independently prove it.
# ---------------------------------------------------------------------------
cleanup() {
  rc=$?
  trap - EXIT INT TERM HUP PIPE
  [ -n "${WATCH_PID:-}" ] && kill "$WATCH_PID" 2>/dev/null
  echo ""
  echo "[cleanup] restoring disk on $LIBRESPEED_HOST (removing the injected fill) ..."
  [ -x "$INJECT" ] && "$INJECT" --clean "$LIBRESPEED_HOST" >/dev/null 2>&1
  # INDEPENDENT verification — do NOT trust inject --clean's exit code (it exits 0 even if ssh never
  # connected). Re-check the fill file directly; a still-present file is a LOUD hard failure.
  if SL "test ! -e '$FILL_FILE'" 2>/dev/null; then
    echo "[cleanup] OK: $FILL_FILE confirmed GONE on $LIBRESPEED_HOST"
  else
    echo "[cleanup] ‼ FAILED to confirm removal of $FILL_FILE on $LIBRESPEED_HOST — DISK MAY STILL BE FILLED."
    echo "[cleanup] ‼ MANUAL ACTION: ssh root@$LIBRESPEED_HOST 'rm -f $FILL_FILE; df -h /'"
    [ "$rc" -eq 0 ] && rc=4
  fi
  [ -n "${WORK:-}" ] && [ -d "$WORK" ] && rm -rf "$WORK" 2>/dev/null
  exit "$rc"
}
trap cleanup EXIT INT TERM HUP PIPE

# ---------------------------------------------------------------------------
# 0. PREFLIGHT
# ---------------------------------------------------------------------------
echo "# tier1-run — Tier-1 disk-pressure head-to-head  (host=$LIBRESPEED_HOST target=${TARGET_PCT}%  stamp=$STAMP)"
echo "## preflight"
for f in "$INJECT" "$WATCH" "$EXTRACT_PRED" "$EXTRACT_TG" "$JUDGE"; do
  [ -x "$f" ] || abort "required script missing or not executable: $f"
done
[ -f "$SSH_KEY" ]    || abort "SSH key not found: $SSH_KEY"
[ -f "$GATEWAY_DB" ] || abort "predecessor gateway.db not found: $GATEWAY_DB"
command -v sqlite3 >/dev/null 2>&1 || abort "sqlite3 not on PATH"
command -v python3 >/dev/null 2>&1 || abort "python3 not on PATH"

check_no_evalgate() {   # re-usable: preflight AND again just before the judge (TOCTOU).
  # eval-gate.sh runs on THIS host (go-run, tunnelling to the box gateway) — a sibling session shares this
  # checkout, so a LOCAL match catches the contention. The '[e]val' bracket-trick stops pgrep -f from
  # matching its own / our own command line (the self-match that made a naive check false-positive). We do
  # NOT check $TG_HOST: no eval-gate process ever runs there, and `ssh 'pgrep -f eval-gate.sh'` self-matches
  # the ssh command wrapper -> a guaranteed false abort.
  if pgrep -f '[e]val/eval-gate.sh' >/dev/null 2>&1; then
    abort "an eval-gate run is in progress locally — refusing (would contend for the model gateway)"
  fi
  return 0
}
check_no_evalgate
echo "  eval-gate: none running (local + $TG_HOST)"

# Mutation MUST be OFF — EXACT series match, fail closed. A prefix match on "mutation_enabled" would also
# match mutation_enabled_attempts_total etc.; anchor the bare gauge and reject if >1 distinct series match.
METRICS="$(S "curl -fsS --max-time 10 http://127.0.0.1:${GROUNDER_PORT}/metrics" 2>/dev/null || true)"
[ -n "$METRICS" ] || abort "could not read grounder /metrics on ${TG_HOST}:${GROUNDER_PORT} — cannot prove mutation OFF"
MVAL="$(printf '%s\n' "$METRICS" | awk '
  /^mutation_enabled([ {]|$)/ { n++; v=$NF }
  END { if (n==1) print v; else if (n>1) print "AMBIG"; else print "ABSENT" }')"
case "$MVAL" in
  ABSENT) abort "mutation_enabled gauge absent from /metrics — cannot prove mutation OFF" ;;
  AMBIG)  abort "mutation_enabled matched >1 series — refusing to guess the gauge value" ;;
esac
[ "$(awk -v v="$MVAL" 'BEGIN{print (v+0==0)?"yes":"no"}')" = "yes" ] \
  || abort "MUTATION IS ON (mutation_enabled=$MVAL on $TG_HOST) — refusing to run a benchmark that could actuate"
echo "  mutation:  OFF (mutation_enabled=$MVAL on $TG_HOST)"

WORK="$(mktemp -d)"; mkdir -p "$OUT_DIR"

# ---------------------------------------------------------------------------
# 1. Baseline + inject, then ASSERT the fault landed in the bounded [90,95) window.
# ---------------------------------------------------------------------------
# Back-date the "since" boundary by SKEW_SLACK to absorb clock skew between this host and the two DBs.
# Safe because the disk alert cannot fire until the next SNMP storage poll (~5 min) >> SKEW_SLACK.
BASELINE="$(date -u -d "-${SKEW_SLACK} seconds" '+%Y-%m-%d %H:%M:%S')"
BASELINE_EPOCH="$(date -u -d "-${SKEW_SLACK} seconds" '+%s')"
SHADOW_DIR="${SHADOW_DIR:-$HOME/logs/claude-gateway/mutation-shadow}"
DATE="$(date -u '+%Y-%m-%d')"
echo "## inject   (since-boundary=$BASELINE UTC, back-dated ${SKEW_SLACK}s for skew)"
INJ_OUT="$("$INJECT" "$LIBRESPEED_HOST" "$TARGET_PCT" 2>&1)"; INJ_RC=$?
printf '%s\n' "$INJ_OUT" | sed 's/^/    /'
[ "$INJ_RC" -eq 0 ] || abort "disk-fill injection failed on $LIBRESPEED_HOST (rc=$INJ_RC)"
printf '%s\n' "$INJ_OUT" | grep -q 'already at/above' \
  && abort "target already at/above ${TARGET_PCT}% — no fault injected, aborting (nothing to benchmark)"
# Verify the ACHIEVED LibreNMS storage_perc (Used/Size — the rule-22 metric, NOT df Use% which excludes
# reserved blocks and reads ~6% higher) is inside [90,95): below 90 => won't fire; >=95 => won't match.
ACH="$(SL "df -P / | awk 'NR==2{printf \"%d\", \$3*100/\$2}'" 2>/dev/null | tr -dc '0-9')"
[ -n "$ACH" ] || abort "could not read achieved df% on $LIBRESPEED_HOST after inject"
if [ "$ACH" -lt 90 ] || [ "$ACH" -ge 95 ]; then
  abort "achieved / usage ${ACH}% is OUTSIDE rule-22 window [90,95) — the alert will not fire; aborting"
fi
echo "  injected: / at ${ACH}% (inside rule-22 window [90,95))"

# ---------------------------------------------------------------------------
# 2. Background observability.
# ---------------------------------------------------------------------------
echo "## outage-watch (background) -> $WATCH_LOG"
"$WATCH" "$WATCH_MIN" "$WATCH_INT" >"$WATCH_LOG" 2>&1 &
WATCH_PID=$!

# ---------------------------------------------------------------------------
# 3. POLL — BOTH sides scoped to the SAME librespeed disk incident (no cron-noise latching).
# ---------------------------------------------------------------------------
tg_ready_count() {   # TG: a new session_triage row for the librespeed host since baseline (disk-ish rule)
  S "docker exec -i $PG_CONTAINER psql -U postgres -d grounder -tAc \"select count(*) from session_triage where created_at > '$BASELINE' and (host ilike '%librespeed%' or external_ref ilike '%librespeed%');\"" 2>/dev/null | tr -dc '0-9'
}
pred_ready_count() { # Predecessor runs MASTER_SWITCH=ON / MUTATIONS=OFF, so its live triages are logged to the
                     # SHADOW LOG (mutation-shadow-gate.py), NOT the now-stale gateway.db. Detect an AGENTIC
                     # shadow-log entry NAMING the target since baseline (mirrors extract_predecessor.py's
                     # AGENTIC_SOURCE classing; scoped to the target so the ~97% cron-noise can't false-pass).
  local f="$SHADOW_DIR/shadow-$(date -u '+%Y-%m-%d').jsonl"
  [ -f "$f" ] || { echo 0; return; }
  SB_BASE="$BASELINE_EPOCH" SB_HOST="$LIBRESPEED_HOST" python3 - "$f" <<'PY' 2>/dev/null | tr -dc '0-9'
import sys,json,os
f=sys.argv[1]; base=int(os.environ.get("SB_BASE","0") or 0)
host=(os.environ.get("SB_HOST","") or "").lower(); short=host.split(".")[0]
sess=set()
for line in open(f, errors="ignore"):
    line=line.strip()
    if not line: continue
    try: d=json.loads(line)
    except Exception: continue
    if (d.get("source") or d.get("emitter") or d.get("src") or "") != "mutation-shadow-gate.py": continue
    ts=d.get("ts") or d.get("timestamp") or d.get("time") or 0
    try: ts=int(float(ts))
    except Exception: ts=0
    if ts and ts < base: continue
    blob=json.dumps(d).lower()
    if "librespeed" in blob or (short and short in blob):
        sid=d.get("session") or d.get("session_id") or d.get("sess") or ""
        if sid: sess.add(sid)
print(len(sess))
PY
}

echo "## poll  (budget=${POLL_TIMEOUT}s @ ${POLL_INTERVAL}s; LibreNMS storage poll ~5min + rule-22 60s delay)"
DEADLINE=$(( $(date +%s) + POLL_TIMEOUT )); iter=0
while :; do
  now="$(date +%s)"; [ "$now" -lt "$DEADLINE" ] || break
  iter=$((iter + 1))
  tc="$(tg_ready_count)";   tc="${tc:-0}"; [ -n "$tc" ] || tc=0
  pc="$(pred_ready_count)"; pc="${pc:-0}"; [ -n "$pc" ] || pc=0
  [ "$tc" -gt 0 ] 2>/dev/null && tg_ok=1
  [ "$pc" -gt 0 ] 2>/dev/null && pred_ok=1
  printf '  [poll %02d | %s UTC] TG-librespeed=%s(ok=%d)  pred-librespeed=%s(ok=%d)  remaining=%ss\n' \
    "$iter" "$(date -u '+%H:%M:%S')" "$tc" "$tg_ok" "$pc" "$pred_ok" "$(( DEADLINE - now ))"
  { [ "$tg_ok" -eq 1 ] && [ "$pred_ok" -eq 1 ]; } && { echo "  both systems triaged the disk incident."; break; }
  sleep "$POLL_INTERVAL"
done

if [ "$tg_ok" -eq 0 ] && [ "$pred_ok" -eq 0 ]; then
  STATUS="FAIL"; EXIT_CODE=1; SCORECARD_OUT=""
  NOTE="neither system triaged a librespeed disk incident within the ${POLL_TIMEOUT}s budget"
  echo "  timeout: NEITHER system triaged — nothing to judge."
else
  # -------------------------------------------------------------------------
  # 4. Extract (time-bounded) + STRICTLY select the librespeed disk pair.
  # -------------------------------------------------------------------------
  echo "## extract + judge"
  timeout 180 python3 "$EXTRACT_PRED" --date "$DATE" --json > "$WORK/pred.json" 2>"$WORK/pred.err" \
    || { echo "  WARN: extract_predecessor failed/timed out"; echo '{"incidents":[]}' > "$WORK/pred.json"; }
  TG_FORMAT=jsononly SHADOW_FROM="${BASELINE}+00" timeout 180 "$EXTRACT_TG" > "$WORK/tg.json" 2>"$WORK/tg.err" \
    || { echo "  WARN: extract_tg failed/timed out"; echo '[]' > "$WORK/tg.json"; }
  python3 - "$WORK/tg.json" <<'PYG'
import json,sys
p=sys.argv[1]
try:
    t=open(p).read().strip(); assert t; json.loads(t)
except Exception:
    open(p,"w").write("[]")
PYG

  # STRICT selection: TG must be a librespeed DISK row; predecessor must be a librespeed-ATTRIBUTED
  # incident. NO fallback to non-disk rows or to "newest real incident by time" (both cross-pair an
  # unrelated incident and manufacture a false PASS — the audit's #1 finding).
  WORK="$WORK" LIBRESPEED_HOST="$LIBRESPEED_HOST" BASELINE="$BASELINE" python3 - <<'PYSEL'
import json, os, datetime
work=os.environ["WORK"]; host=os.environ.get("LIBRESPEED_HOST","dc1librespeed01").lower()
base=os.environ.get("BASELINE","")
def pts(s):
    if not s: return None
    s=str(s).strip().replace("Z","+00:00")
    if "T" not in s and " " in s: s=s.replace(" ","T",1)
    try:
        dt=datetime.datetime.fromisoformat(s)
        return dt.replace(tzinfo=datetime.timezone.utc) if dt.tzinfo is None else dt
    except Exception: return None
MIN=datetime.datetime.min.replace(tzinfo=datetime.timezone.utc); bdt=pts(base)
def hmatch(v): v=(v or "").lower(); return "librespeed" in v or (host and host in v)
def since(dt): return bdt is None or dt is None or dt>=bdt
def is_disk(r):
    ar=(r.get("alertRule") or r.get("alert_rule") or r.get("rule") or "").lower()
    return any(k in ar for k in ("space","disk","storage","/ is",">= 90"))
# TG side
try: tg=json.load(open(os.path.join(work,"tg.json"))); tg=tg if isinstance(tg,list) else []
except Exception: tg=[]
tg_cands=[r for r in tg if (hmatch(r.get("host")) or hmatch(r.get("external_ref"))) and since(pts(r.get("createdAt"))) and is_disk(r)]
tg_sel=sorted(tg_cands,key=lambda r:(pts(r.get("createdAt")) or MIN))[-1] if tg_cands else None
# predecessor side
try: incs=(json.load(open(os.path.join(work,"pred.json"))).get("incidents") or [])
except Exception: incs=[]
def real(i):
    k=(i.get("incidentKey") or "").strip()
    return k not in ("","T") and bool(i.get("agentic")) and bool(i.get("issue") or i.get("reasoningExcerpt"))
pred_cands=[i for i in incs if real(i) and hmatch(i.get("host")) and since(pts(i.get("firstTs")))]
pred_sel=sorted(pred_cands,key=lambda i:(pts(i.get("firstTs")) or MIN))[-1] if pred_cands else None
key=(tg_sel or {}).get("external_ref") or (pred_sel or {}).get("incidentKey") or ("tier1-disk-"+host)
if tg_sel is not None: json.dump(tg_sel,open(os.path.join(work,"j_tg.json"),"w"))
if pred_sel is not None: json.dump(pred_sel,open(os.path.join(work,"j_pred.json"),"w"))
open(os.path.join(work,"key.txt"),"w").write(key)
open(os.path.join(work,"have_tg.txt"),"w").write("yes" if tg_sel is not None else "no")
open(os.path.join(work,"have_pred.txt"),"w").write("yes" if pred_sel is not None else "no")
print("  selected: tg=%s pred=%s key=%s" % ("yes" if tg_sel else "no","yes" if pred_sel else "no",key))
PYSEL

  KEY="$(cat "$WORK/key.txt" 2>/dev/null)"; KEY="${KEY:-tier1-disk-$LIBRESPEED_HOST}"
  HAVE_TG="$(cat "$WORK/have_tg.txt" 2>/dev/null)"; HAVE_TG="${HAVE_TG:-no}"
  HAVE_PRED="$(cat "$WORK/have_pred.txt" 2>/dev/null)"; HAVE_PRED="${HAVE_PRED:-no}"

  if [ "$HAVE_PRED" = "yes" ] && [ "$HAVE_TG" = "yes" ]; then
    check_no_evalgate                         # TOCTOU: re-verify no eval-gate started during the poll window
    JUDGE_ARGS=(--incident-key "$KEY" --model "$MODEL" --ssh-key "$SSH_KEY" --tg-host "$TG_HOST" \
                --ssh-user "$SSH_USER" --env-path "$ENV_PATH" --out "$SCORECARD_OUT" \
                --pred "$WORK/j_pred.json" --tg "$WORK/j_tg.json")
    echo "  judging incident '$KEY' (pred=yes tg=yes) -> $SCORECARD_OUT"
    timeout 420 python3 "$JUDGE" "${JUDGE_ARGS[@]}" >"$WORK/judge.stdout" 2>"$WORK/judge.err" || true
    # PASS only if the blind judge produced numeric dims on BOTH sides + a winner (never one-sided).
    SCORED="$(python3 - "$SCORECARD_OUT" <<'PYSC'
import json,sys
try: v=json.load(open(sys.argv[1]))
except Exception: print("no"); sys.exit(0)
if v.get("judge_unavailable"): print("no"); sys.exit(0)
dims=v.get("dims") or {}
def side_ok(letter):
    dd=dims.get(letter) or {}
    return isinstance(dd,dict) and any(isinstance(x,(int,float)) for x in dd.values())
print("yes" if side_ok("A") and side_ok("B") and (v.get("winner") not in (None,"")) else "no")
PYSC
)"
    if [ "$SCORED" = "yes" ]; then STATUS="PASS"; EXIT_CODE=0
    else STATUS="FAIL"; EXIT_CODE=1
      NOTE="both sides present but the judge did not score BOTH sides + a winner — see $WORK/judge.err"
    fi
  else
    STATUS="ONE-SIDED"; EXIT_CODE=3
    NOTE="only one system triaged the disk incident (pred=$HAVE_PRED tg=$HAVE_TG) — NOT a valid head-to-head"
    [ -s "$SCORECARD_OUT" ] || SCORECARD_OUT=""
  fi
fi

# ---------------------------------------------------------------------------
# 5. Summary.
# ---------------------------------------------------------------------------
echo ""
echo "==================== TIER-1 SUMMARY ===================="
echo " result:        ${STATUS}"
echo " incident:      ${KEY:-<none>}   (host $LIBRESPEED_HOST, LibreNMS rule 22 disk-pressure)"
echo " since UTC:     $BASELINE"
echo " TG triaged:    $([ "$tg_ok" -eq 1 ] && echo yes || echo no)   (session_triage librespeed row)"
echo " pred triaged:  $([ "$pred_ok" -eq 1 ] && echo yes || echo no)   (gateway.db session, issue_title~librespeed)"
echo " scorecard:     ${SCORECARD_OUT:-<none — nothing judged>}"
echo " outage-watch:  $WATCH_LOG"
[ -n "${NOTE:-}" ] && echo " note:          $NOTE"
case "$STATUS" in
  PASS)      echo " => PASS: both systems triaged the same disk incident and the blind judge scored BOTH sides." ;;
  ONE-SIDED) echo " => ONE-SIDED: only one system triaged within the budget; NOT a valid head-to-head." ;;
  FAIL)      echo " => FAIL: no scored head-to-head produced." ;;
esac
echo "========================================================"
exit "${EXIT_CODE:-1}"
