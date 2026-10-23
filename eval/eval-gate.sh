#!/usr/bin/env bash
# eval-gate.sh — the ON-BOX half of TG's binding eval gate (TG-43 / audit R4; drift fix TG-64).
#
# TG's CI has no Postgres/Temporal/model gateway, so the LLM-judge eval cannot run in a stock CI job. The
# model lives on dc1tg01 (loopback LiteLLM). This script tunnels the box gateway and runs the corpus
# through the REAL Runner + judge, then invokes the DETERMINISTIC gate (tools/evalgate) and EXITS NON-ZERO
# on FAIL. It has three modes:
#
#   eval/eval-gate.sh            # CHANGE GATE (default): candidate vs a FRESH origin/main base arm, same
#   eval/eval-gate.sh change     #   window, arms alternated to cancel drift — the pre-merge gate (TG-64).
#                                #   FAST by default (TG-117): a corpus SUBSET x1 run per arm, ~10-15 min.
#                                #   TG_EVAL_FULL=1 (or `make eval-gate-full`) = the 3-run x full-corpus rigor.
#   eval/eval-gate.sh trend      # TREND-WATCH: a clean main measurement vs the COMMITTED baseline, and
#                                #   self-refresh that baseline on a clean, non-regressing run (the nightly).
#                                #   ALWAYS full rigor (full corpus, multi-run) — never quick-passed.
#   eval/eval-gate.sh holdout    # the §1.3 overfitting check: regression vs the sealed holdout, >20pt fails.
#
# WHY the change gate (TG-64): comparing a freshly-measured candidate to the COMMITTED baseline-scorecard.json
# (a point-in-time measurement at an old SHA) conflates the candidate's OWN change with (a) main-drift since
# that SHA and (b) MODEL + LIVE-ESTATE drift (kimi/deepseek + live LibreNMS move hour-to-hour). A stale-
# baseline gate false-FAILs essentially every branch off newer main. The change gate measures BOTH arms in the
# SAME window and gates candidate-vs-fresh-base, so drift cancels. The committed baseline is used ONLY by the
# trend-watch (long-horizon tracking), where a stale point-in-time anchor is legitimate.
#
# WHY fast-by-default (TG-117): the full 3-run x 20-session corpus x a slow reasoning model is ~1.5-2h — far
# too slow to sit on the critical path of every agent-behavior merge. The change gate therefore defaults to a
# bounded quick-pass (a corpus SUBSET run ONCE per arm, still drift-cancelled) that costs ~10-15 min and still
# FAILS non-zero on a real regression. It keeps every real-gate property: the fresh origin/main base arm +
# drift cancellation (TG-64), the refuse-to-pool-a-degraded/contended-arm integrity abort (TG-65), the
# deterministic per-dimension gate (tools/evalgate), and the negative-controls check. The full rigor is not
# deleted — it moves behind TG_EVAL_FULL=1 / `make eval-gate-full` and is what the nightly trend-watch runs.
# Merges are reversible, so the (rare) miss the smaller sample allows is cheap and the nightly still catches it.
#
# GATE SELECTOR (when to run this at all): only PROMPT / SKILL / AGENT-BEHAVIOR changes need the eval — a
# purely DETERMINISTIC change (Go/infra/docs/CI with no agent-behavior delta) is fully covered by `make all`
# (vet · lint · spec · test · build) and should SKIP the eval entirely. Behavioral changes run this fast gate;
# dial up to TG_EVAL_FULL=1 for a high-risk agent-behavior change before merge.
#
# Env (all optional; secrets are read from the box .env by reference, never literals):
#   TG_SSH_KEY   (default ~/.ssh/one_key)   TG_BOX (default root@dc1tg01)   TG_EVAL_PORT (default: a per-invocation free port, TG-493)
#   TG_EVAL_RUNS (per arm; default 1 for the fast change gate, else 3)          TG_GATE_OUT (default eval/out)
#   TG_EVAL_LIMIT (corpus subset; default 8 for the fast change gate, else the full corpus) — exported to go test
#   TG_EVAL_FULL (=1 -> full 3-run x full-corpus rigor for the change gate; `make eval-gate-full` sets it)
#   TG_BASELINE  (default eval/baseline-scorecard.json)   TG_EVAL_MAX_RETRY (default 2, per-arm 429 reruns)
#   TG_BASE_REF  (default origin/main — the branch's merge target; the fresh base arm is checked out here)
set -euo pipefail

MODE="${1:-change}"
[ "$MODE" = "gate" ] && MODE="change"   # back-compat alias

# ── THE GATEWAY LOCK ─────────────────────────────────────────────────────────────────────────────────────
#
# Every mode of this script measures against the SAME box gateway, and so does the fault-injection campaign
# (tools/shadowbench/run-campaign.sh, which has taken /tmp/tg-gateway.lock since it was written). This
# script took no lock at all, so any two of {a hand-run change gate, the nightly trend-watch, a campaign
# cycle} could measure through one contended gateway simultaneously.
#
# THAT IS NOT HYPOTHETICAL. Observed 2026-08-01 on a hand-run change gate: the first candidate arm came back
# `7/8 session(s) errored (degraded/contended arm)`. The integrity check caught it and rerouted to a rerun —
# which is the arm-integrity property working exactly as designed — but it cost a full arm (~4 min) and it
# only recovers because MAX_RETRY happens to be 2. Two contending runs can exhaust that and turn a healthy
# candidate into a gate failure, which is the worst outcome available: a gate that fails for a reason that
# has nothing to do with the change.
#
# So the lock is taken HERE, around the whole run, by every mode. flock is advisory and process-scoped, so
# it composes with the campaign's use of the same path rather than fighting it.
#
# WHAT THIS DOES NOT COVER, stated because a lock that is trusted further than it reaches is worse than no
# lock. This is a LOCAL file lock, so it serializes callers on THIS host only — which is the contention
# that was actually observed (a campaign cycle and a hand-run gate on the same workstation, both tunnelling
# to the same box gateway) and the same scope the campaign script has always assumed. It does NOT serialize
# a CI runner against a workstation, because they do not share /tmp. Genuine cross-host serialization needs
# the lock to live on the BOX, held for the run's duration over ssh; that is a larger change with its own
# failure modes (a dropped ssh session leaving a stale lock), and it is the prerequisite for making this
# gate a blocking CI job rather than a hand-run command. Tracked on TG-237.
#
# It BLOCKS rather than fails: a change gate that refuses to run because the nightly holds the lock has
# simply not answered, and "come back later" is a worse answer than "wait 15 minutes". The timeout exists so
# a stale holder cannot wedge a merge forever, and exceeding it is LOUD — never a silent skip, because a
# quality gate that quietly does not run is the failure this repo has already had (see the nightly job's own
# FAIL-LOUD comment in .gitlab-ci.yml).
GATE_LOCK="${TG_EVAL_LOCK:-/tmp/tg-gateway.lock}"
GATE_LOCK_WAIT="${TG_EVAL_LOCK_WAIT:-3600}"
# TG-503: an ANCESTOR may ALREADY hold $GATE_LOCK. The nightly cron wraps this job in
# `flock -n /tmp/tg-gateway.lock -c '... make eval-drift ...'`, so the cron shell holds the lock while
# make -> eval-gate.sh runs beneath it. Re-taking that same lock in the block below then blocks on our OWN
# parent and times out after ${GATE_LOCK_WAIT}s: the 2026-08-08..16 nightly stall (13 consecutive exit-75
# aborts, an 8-day-blind trend baseline), which appeared when e8aefeef gave this script its own gateway lock
# that the pre-existing cron wrapper now double-takes. An ancestor holding the gateway lock means we ALREADY
# run serialized through it, so honour it exactly like TG_EVAL_LOCKED=1 and fall through to the inner run
# rather than deadlocking. Safe against fail-open: we skip the re-lock ONLY when a genuine ANCESTOR holds
# this exact file (nothing but flock ever opens the gateway lockfile), so an unserialized arm never runs.
gate_lock_held_by_ancestor() {
  local lk="$1" ino anc ancestors="" typ pid rgn _a _b _c _rest
  ino=$(stat -c %i "$lk" 2>/dev/null) || return 1
  [ -n "$ino" ] || return 1
  anc=$PPID                                   # collect our ancestor PIDs — the cron's flock wrapper is one
  while [ -n "$anc" ] && [ "$anc" -gt 1 ] 2>/dev/null; do
    ancestors="$ancestors $anc"
    anc=$(awk '/^PPid:/{print $2}' "/proc/$anc/status" 2>/dev/null)
  done
  # /proc/locks: a FLOCK holder (not a "->" blocked waiter) on our lockfile's inode. If that PID is an
  # ancestor, we already run under its lock. fuser(1) is not installed here, so read the kernel table directly.
  while read -r _a typ _b _c pid rgn _rest; do
    [ "$typ" = "FLOCK" ] || continue
    [ "${rgn##*:}" = "$ino" ] || continue
    case " $ancestors " in *" $pid "*) return 0 ;; esac
  done < /proc/locks
  return 1
}
if [ "${TG_EVAL_LOCKED:-}" != "1" ] && gate_lock_held_by_ancestor "$GATE_LOCK"; then
  echo "== gateway lock ($GATE_LOCK) already held by an ancestor process; running serialized under it (TG-503) =="
  TG_EVAL_LOCKED=1
fi
if [ "${TG_EVAL_LOCKED:-}" != "1" ]; then
  if ! command -v flock >/dev/null 2>&1; then
    echo "eval-gate: flock(1) not available — refusing to measure through a possibly-contended gateway." >&2
    echo "  Install util-linux, or set TG_EVAL_LOCKED=1 to declare this run deliberately unserialized." >&2
    exit 1
  fi
  echo "== taking the gateway lock ($GATE_LOCK, up to ${GATE_LOCK_WAIT}s) =="
  # NOT `exec`: the timeout has to be reported, and an exec'd flock leaves nobody to report it. flock exits
  # 1 on timeout, which is indistinguishable from a genuine gate FAIL to any caller reading only the code —
  # and "the gate said no" versus "the gate never ran" are the two answers this repo has most often
  # confused. Exit 75 (EX_TEMPFAIL) instead, so a CI job or a human can tell them apart.
  # Capture the status DIRECTLY. `if cmd; then ...; fi` followed by `rc=$?` reads the status of the IF
  # STATEMENT, which is 0 whenever the condition is false and there is no else — so the first version of
  # this block turned a lock TIMEOUT into `exit 0`, i.e. a gate that never ran reporting PASS. That is a
  # fail-open in a quality gate, and strictly worse than the silent `exit 1` it was meant to improve on.
  # The marker that tells the two apart. flock exits 1 on timeout, and SO DOES the inner run's own
  # arm-integrity ABORT (run_arm, "refusing to pool a contended/429 arm") and several other inner
  # failures — so the exit code ALONE cannot distinguish them, and reading it as a timeout reports
  # "THE GATE DID NOT RUN" for a gate that ran and refused, with a retryable EX_TEMPFAIL attached.
  # Observed 2026-08-06: a candidate arm degraded twice on a saturated model gateway, aborted correctly,
  # and was announced as a lock contention — sending the reader to `fuser` instead of to the brain.
  # The inner run stamps this file immediately after flock grants the lock, so its existence means
  # "the inner run started", with no race: nothing writes it before the lock is held.
  RAN_MARK="$(mktemp -t tg-eval-gate-ran.XXXXXX)"
  rm -f "$RAN_MARK"                       # created BY the inner run; absence is the signal
  trap 'rm -f "$RAN_MARK"' EXIT
  set +e
  env TG_EVAL_LOCKED=1 TG_EVAL_RAN_MARK="$RAN_MARK" \
    flock --wait "$GATE_LOCK_WAIT" "$GATE_LOCK" "$0" "$@"
  rc=$?
  set -e
  if [ "$rc" = 0 ]; then
    exit 0
  fi
  if [ ! -e "$GATE_LOCK" ] || [ "$rc" != 1 ] || [ -e "$RAN_MARK" ]; then
    exit "$rc"   # a real failure from the inner run — pass it through untouched
  fi
  # Only here is it a genuine timeout: flock returned 1 AND the inner run never stamped its marker.
  echo "" >&2
  echo "eval-gate: COULD NOT ACQUIRE THE GATEWAY LOCK within ${GATE_LOCK_WAIT}s ($GATE_LOCK)." >&2
  echo "  Something else is measuring through the box gateway right now — the nightly trend-watch, a" >&2
  echo "  shadowbench campaign cycle, or another gate run. THE GATE DID NOT RUN; this is NOT a gate" >&2
  echo "  failure and must not be read as one." >&2
  echo "  Wait and re-run, raise TG_EVAL_LOCK_WAIT, or check for a stale holder: fuser -v $GATE_LOCK" >&2
  exit 75  # EX_TEMPFAIL — retryable, deliberately distinct from the gate's own non-zero verdict
fi

# THE INNER RUN STARTS HERE — flock has granted the lock (or TG_EVAL_LOCKED was set by hand). Stamp the
# marker first, before anything that can fail, so every inner outcome is distinguishable from "never ran".
[ -n "${TG_EVAL_RAN_MARK:-}" ] && : > "$TG_EVAL_RAN_MARK"
# Self-test hook: exit immediately with a chosen code, having stamped the marker. Lets the wrapper's
# ran-vs-timeout decision be exercised without a gateway, a box, or a 6-minute arm.
if [ -n "${TG_EVAL_SELFTEST_INNER_RC:-}" ]; then
  echo "selftest: inner run reached, exiting ${TG_EVAL_SELFTEST_INNER_RC}"
  exit "$TG_EVAL_SELFTEST_INNER_RC"
fi

KEY="${TG_SSH_KEY:-$HOME/.ssh/one_key}"
BOX="${TG_BOX:-root@dc1tg01}"
# TG-493: the local forward port is claimed by the SSH BIND ITSELF (open_tunnel below), not pre-picked — the OS
# guarantees exactly one process binds a given local port, so there is no pick-then-bind TOCTOU where two
# concurrent sessions compute the same port before either opens its forwarder (the residual race a pgrep-presence
# pick left). A fixed 4010 under the old TG-117 adopt let a second session share the first's tunnel and the
# first's cleanup then murdered it mid-measurement; a per-invocation bind-claim retires that. TG_EVAL_PORT pins.
LPORT=""   # set by open_tunnel once a bind succeeds
# Fast-by-default CHANGE gate (TG-117): the pre-merge change gate defaults to a bounded quick-pass — a corpus
# SUBSET (TG_EVAL_LIMIT) run ONCE per arm — so a merge costs ~10-15 min, not ~1.5-2h, while keeping every
# real-gate property (fresh base arm + drift-cancel TG-64, integrity abort TG-65, per-dimension gate, negative
# controls). TG_EVAL_FULL=1 (or `make eval-gate-full`) restores the full 3-run x full-corpus rigor; overriding
# TG_EVAL_RUNS / TG_EVAL_LIMIT picks any point in between. trend + holdout ALWAYS run full rigor.
FULL="${TG_EVAL_FULL:-0}"
if [ "$MODE" = "change" ] && [ "$FULL" != "1" ]; then
  RUNS="${TG_EVAL_RUNS:-1}"
  TG_EVAL_LIMIT="${TG_EVAL_LIMIT:-8}"; export TG_EVAL_LIMIT   # export so the go-test child truncates the corpus
else
  RUNS="${TG_EVAL_RUNS:-3}"
fi
MAX_RETRY="${TG_EVAL_MAX_RETRY:-2}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${TG_GATE_OUT:-$HERE/eval/out}"
BASELINE="${TG_BASELINE:-$HERE/eval/baseline-scorecard.json}"
mkdir -p "$OUT"

# Integrity expectation: the FULL corpus size unless a TG_EVAL_LIMIT smoke pass restricts it. A degraded/429
# arm produces a short/errored scorecard; the gate must never pool it (TG-64).
CORPUS_N="$(grep -c '"external_ref"' "$HERE/eval/corpus.json" || echo 0)"
if [ -n "${TG_EVAL_LIMIT:-}" ]; then
  EXPECT_N_ARG="--expect-n ${TG_EVAL_LIMIT}"
else
  EXPECT_N_ARG="--expect-n ${CORPUS_N}"
fi

# Cleanup: kill the tunnel, drop the base worktree, remove the temp root. All best-effort, all on exit.
TUN_PID=""
BASE_WT=""
TMPROOT=""
cleanup() {
  # TG-493: we always open our OWN forwarder on a per-invocation port and always kill it here — no adoption,
  # so an exiting run can never murder a forwarder another concurrent session is still measuring through.
  if [ -n "${TUN_PID:-}" ]; then kill "$TUN_PID" 2>/dev/null || true; fi
  [ -n "${BASE_WT:-}" ] && git -C "$HERE" worktree remove --force "$BASE_WT" 2>/dev/null || true
  [ -n "${TMPROOT:-}" ] && rm -rf "$TMPROOT" 2>/dev/null || true
  # Shred the parity key material (best-effort; the dir is tmpfs on most estates anyway).
  if [ -n "${SECDIR:-}" ] && [ -d "$SECDIR" ]; then
    find "$SECDIR" -type f -exec shred -u {} \; 2>/dev/null || true
    rm -rf "$SECDIR" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# Resolve the gateway master key + LibreNMS token from the box (never printed, never committed).
# CRITICAL — read the key the way a SHELL EXPANDS it, not a raw `sed` of the file. The spec/024 OpenBao drop
# writes `export LITELLM_MASTER_KEY='<key>'` SINGLE-QUOTED (and the .env value may be quoted too); a naive
# `sed -n s/^…=//p` keeps the surrounding quotes → a token 2 chars too long that is NOT litellm's in-memory
# master, so litellm falls to a virtual-key key-DB lookup and (no DB) returns 400 "No connected db" — while
# `/models` still 200s, masking it — and every gate arm degraded to "0 sessions judged". There is NO key drift:
# sourced/dequoted, the drop == OpenBao == the live litellm master (all one 24-char key). Read the live litellm
# process env (/proc/1/environ — already shell-dequoted) FIRST; if we must fall back, SOURCE the drop / .env so
# the shell strips the quotes, never sed the raw line.
MK=$(ssh -i "$KEY" -o StrictHostKeyChecking=no "$BOX" '
  cid=$(docker ps -q -f name=litellm | head -1)
  v=$([ -n "$cid" ] && docker exec "$cid" sh -c "tr \"\0\" \"\n\" < /proc/1/environ" 2>/dev/null | sed -n "s/^LITELLM_MASTER_KEY=//p" | head -1)
  drop="${TG_LITELLM_SECRET_DROP:-/dev/shm/tg-litellm-secrets}/env"
  [ -n "$v" ] || v=$([ -f "$drop" ] && sh -c ". \"$drop\" >/dev/null 2>&1; printf %s \"\${LITELLM_MASTER_KEY:-}\"")
  printf %s "$v"')
# The LibreNMS token feeds controlObservability (TG-362), which EXCLUDES negative controls whose host the
# alert source does not actually monitor — a control on an unmonitored host cannot discriminate, so counting
# it is scoring the agent against a story the estate never had.
#
# The resolution itself lives in eval/lib-librenms-token.sh, shared with eval/tier-ab.sh. It was inline here
# once, and tier-ab.sh carried a stale COPY of its broken first form for weeks after this one was fixed —
# so the whole rationale, including both ways it was got wrong, now lives with the single implementation.
# shellcheck source=eval/lib-librenms-token.sh
. "$HERE/eval/lib-librenms-token.sh"
LT=$(tg_resolve_librenms_token "$KEY" "$BOX")
[ -n "$MK" ] || { echo "could not read LITELLM_MASTER_KEY from $BOX (tried litellm PID1 environ, OpenBao drop, .env)"; exit 1; }

# Toolset PARITY provisioning (Phase B5, 2026-07-30): the eval harness registers the worker's hostdiag +
# syslog-ng tools iff their deployment env is set — and nothing outside the worker container set it, so the
# parity registration was inert in every gate invocation (found by adversarial review). Fetch the declared
# deployments from the box's .env, pull any `file:` key material they reference into a 0700 tempdir (the
# runner already holds root SSH to the box, so no new trust boundary — the material never persists past the
# run), rewrite the refs to the local copies, and export. The harness itself fails CLOSED if the corpus has
# expected-propose labels and TG_HOSTDIAG_DEPLOYMENTS is still empty.
box_env() { ssh -i "$KEY" -o StrictHostKeyChecking=no "$BOX" "grep '^$1=' /srv/tg/deploy/.env | cut -d= -f2-" 2>/dev/null || true; }
HD_DEP=$(box_env TG_HOSTDIAG_DEPLOYMENTS)
SG_DEP=$(box_env TG_SYSLOGNG_DEPLOYMENTS)
NAT_RULES=$(box_env TG_CREDENTIAL_NATIVE_RULES)
HD_KH=$(box_env TG_HOSTDIAG_KNOWN_HOSTS)
SECDIR=""
if [ -n "$HD_DEP" ] || [ -n "$SG_DEP" ]; then
  SECDIR=$(mktemp -d); chmod 700 "$SECDIR"
  for ref in $(printf '%s\n%s\n%s\n' "$HD_DEP" "$SG_DEP" "$NAT_RULES" | grep -oE 'file:[^|,; "]+' | sort -u); do
    p=${ref#file:}; loc="$SECDIR/$(basename "$p")"
    # The refs carry CONTAINER paths (the compose stack binds ./secrets:/secrets:ro under /srv/tg); read
    # the host-side source when the container path does not exist on the box host.
    if ssh -i "$KEY" -o StrictHostKeyChecking=no "$BOX" "cat '$p' 2>/dev/null || cat '/srv/tg/deploy$p' 2>/dev/null || cat '/srv/tg$p'" > "$loc" 2>/dev/null && [ -s "$loc" ]; then
      chmod 600 "$loc"
      HD_DEP=${HD_DEP//"file:$p"/"file:$loc"}
      SG_DEP=${SG_DEP//"file:$p"/"file:$loc"}
      NAT_RULES=${NAT_RULES//"file:$p"/"file:$loc"}
    else
      echo "warn: could not fetch $p from $BOX — tools referencing it will fail closed per call" >&2
      rm -f "$loc"
    fi
  done
  if [ -n "$HD_KH" ]; then
    if ssh -i "$KEY" -o StrictHostKeyChecking=no "$BOX" "cat '$HD_KH'" > "$SECDIR/known_hosts" 2>/dev/null && [ -s "$SECDIR/known_hosts" ]; then
      export TG_HOSTDIAG_KNOWN_HOSTS="$SECDIR/known_hosts"
    fi
  fi
  [ -n "$HD_DEP" ] && export TG_HOSTDIAG_DEPLOYMENTS="$HD_DEP" && echo "parity: hostdiag deployments provisioned from the box"
  [ -n "$SG_DEP" ] && export TG_SYSLOGNG_DEPLOYMENTS="$SG_DEP" && echo "parity: syslog-ng deployments provisioned from the box"
  [ -n "$NAT_RULES" ] && export TG_CREDENTIAL_NATIVE_RULES="$NAT_RULES"
fi

# Open the tunnel: a local port -> box 127.0.0.1:4000 (the litellm gateway). BOTH ends of the -L stay 127.0.0.1
# explicitly, never the ambiguous "localhost":
#   - LOCAL bind: "localhost" makes ssh ALSO try to bind [::1] ("bind [::1]:PORT: Cannot assign requested
#     address" on hosts without IPv6 loopback) and lets the eval client resolve localhost→::1 first onto a dead addr.
#   - REMOTE target: the box's /etc/hosts resolves `localhost`→::1 FIRST, and litellm publishes on 127.0.0.1:4000
#     ONLY (docker `127.0.0.1:4000->4000/tcp`, nothing on [::1]:4000). Forwarding to `localhost:4000` lands the
#     sshd channel on the dead [::1]:4000, the forwarder wedges (LISTENs but every forward RSTs → curl 000), and
#     the run degrades to the "0 sessions judged" abort. Pin the remote to 127.0.0.1:4000. Keepalive holds the
#     long tunnel open across idle gaps so a mid-run arm never loses it.
#
# TG-493 (sequel to the TG-117 reuse guard): the LOCAL BIND IS THE ATOMIC CLAIM. TG-117 pinned 4010 and ADOPTED a
# pre-existing forwarder so a stale one did not fail the next run — but that let a second concurrent session share
# the first's tunnel, and the first's cleanup() then murdered it mid-measurement (connect-refused -> TG-64 abort
# -> a gate that wedged looking like contention). A pgrep-presence PRE-pick only narrowed the window: two sessions
# still computed the same port before either bound. So we never pre-pick and never adopt — open_tunnel walks
# candidate ports and lets ssh's own -L bind decide: the OS admits exactly ONE binder per local port, and
# ExitOnForwardFailure=yes makes ssh EXIT when it cannot bind, so a collision is observable (the child is gone)
# and we try the next port. We keep the REAL ssh pid ($!), so cleanup only ever kills the forwarder WE opened,
# never one matched by argv pattern that belongs to another run.
probe_tunnel() {  # 0 = a live gateway answers through 127.0.0.1:${1:-$LPORT}
  local port="${1:-$LPORT}" code
  code=$(curl -sS -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/health/liveliness" 2>/dev/null || echo 000)
  [ "$code" != "000" ]  # any HTTP response (even 401/404) proves the tunnel forwards; only 000 is "dead"
}
# De-serialization (owner-requested, companion to TG-522): the REMOTE forward target of the tunnel is
# parameterizable so a second concurrent eval LANE can land on a DISTINCT litellm gateway instead of contending
# on the box's one loopback proxy (the sole thing /tmp/tg-gateway.lock serialized). DEFAULT is the box's own
# 127.0.0.1:4000 -- byte-identical to the prior hardcoded behaviour, so a normal single-lane run is unchanged. A
# lane overrides TG_EVAL_GATEWAY_TARGET=<host:port> in its ENV only (never committed: estate addresses stay out
# of the repo). The SSH host ($BOX) is unchanged; only the -L remote endpoint moves, and the box's LAN reach
# (post-TG-160) carries the forward to the second gateway. Keep the remote a literal IP:port, not a name, for
# the same ::1-first reason the box default is pinned to 127.0.0.1 (see the tunnel comment above).
: "${TG_EVAL_GATEWAY_TARGET:=127.0.0.1:4000}"
open_tunnel() {  # claims a port via the ssh -L bind itself; sets LPORT + TUN_PID (the real ssh pid), fails closed
  local pinned="${TG_EVAL_PORT:-}" base=$(( 4010 + $$ % 200 )) ports p pid ok i
  if [ -n "$pinned" ]; then ports="$pinned"; else ports=$(seq "$base" "$(( base + 60 ))"); fi
  for p in $ports; do
    ssh -i "$KEY" -o StrictHostKeyChecking=no -o ExitOnForwardFailure=yes \
        -o ServerAliveInterval=30 -o ServerAliveCountMax=6 \
        -N -L "127.0.0.1:${p}:${TG_EVAL_GATEWAY_TARGET}" "$BOX" &
    pid=$!; ok=""
    for i in 1 2 3 4 5; do
      sleep 1
      kill -0 "$pid" 2>/dev/null || break        # ssh exited: bind lost (ExitOnForwardFailure) or auth failed
      probe_tunnel "$p" && { ok=1; break; }        # gateway answers through the bound port -> ours
    done
    if [ -n "$ok" ]; then
      LPORT="$p"; TUN_PID="$pid"
      echo "== gateway tunnel on 127.0.0.1:${LPORT} -> ${TG_EVAL_GATEWAY_TARGET} via ${BOX} (pid ${TUN_PID}, bind-claimed) =="
      return 0
    fi
    kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true   # release the losing attempt, reap, next
    if [ -n "$pinned" ]; then
      echo "FATAL: TG_EVAL_PORT=${pinned} would not bind+forward (another run holds it, or the gateway is down) — pick another port or unset TG_EVAL_PORT for auto-claim" >&2
      return 1
    fi
  done
  echo "FATAL: no local port in ${base}..$(( base + 60 )) could bind a gateway forwarder to ${TG_EVAL_GATEWAY_TARGET} via ${BOX}" >&2
  return 1
}
open_tunnel || exit 1

cd "$HERE"
export TG_EVAL_GATEWAY="http://127.0.0.1:${LPORT}"  # 127.0.0.1, never "localhost" (avoid the ::1-first resolve)
export LITELLM_MASTER_KEY="$MK"
export LIBRENMS_TOKEN="$LT"
export TG_LIBRENMS_URL="${TG_LIBRENMS_URL:-https://dc1nms01.example.net}"
export TG_LIBRENMS_INSECURE="${TG_LIBRENMS_INSECURE:-true}"

# run_arm LABEL DIR TEST_REGEX OUT_SCORECARD [OUT_CONTROLS] — measure one arm in DIR, verify integrity, and
# RERUN a degraded (429/short) measurement up to MAX_RETRY times before ABORTING (a contended arm must never
# silently enter the pooled verdict). The integrity probe always runs from $HERE — only the candidate checkout
# has the --verify-integrity flag (the base checkout is older origin/main code).
run_arm() {
  local label="$1" dir="$2" regex="$3" out_sc="$4" out_ctrl="${5:-}"
  local attempt=1 trc
  while :; do
    echo "== [$label] measure (attempt ${attempt}/${MAX_RETRY}) in ${dir} =="
    set +e
    ( cd "$dir" && go test ./eval/ -run "$regex" -count=1 -timeout 40m )
    trc=$?
    set -e
    if [ "$trc" -eq 0 ] && ( cd "$HERE" && go run ./tools/evalgate --verify-integrity "$dir/eval/scorecard.json" $EXPECT_N_ARG ); then
      break
    fi
    if [ "$attempt" -ge "$MAX_RETRY" ]; then
      echo "ABORT: [$label] still degraded after ${attempt} attempt(s) — refusing to pool a contended/429 arm (TG-64)." >&2
      exit 1
    fi
    attempt=$((attempt + 1))
    echo "== [$label] degraded — backing off and rerunning =="
    sleep 15
  done
  cp -f "$dir/eval/scorecard.json" "$out_sc"
  if [ -n "$out_ctrl" ] && [ -f "$dir/eval/controls-scorecard.json" ]; then
    cp -f "$dir/eval/controls-scorecard.json" "$out_ctrl"
  fi
}

# --------------------------------------------------------------------------------------------------------
if [ "$MODE" = "holdout" ]; then
  echo "== holdout overfitting check: 1 regression run + 1 sealed-holdout run =="
  go test ./eval/ -run 'TestEvalCorpusOnBox' -count=1 -timeout 40m
  cp -f eval/scorecard.json "$OUT/regression.json"
  go test ./eval/ -run 'TestEvalHoldoutOnBox' -count=1 -timeout 40m
  cp -f eval/holdout-scorecard.json "$OUT/holdout.json"
  go run ./tools/evalgate --baseline "$BASELINE" --candidate "$OUT/regression.json" --holdout "$OUT/holdout.json"
  exit $?
fi

# --------------------------------------------------------------------------------------------------------
# RUBRIC MODE (TG-359): the evidence a core/judge/rubric.json edit actually needs.
#
# The ordinary change gate CANNOT gate a rubric edit. It measures the candidate against a fresh
# origin/main arm, each in its own worktree, so the candidate judges under the new rubric and the base
# under the old one — and tools/evalgate then correctly refuses to pool two rubric versions (TG-194).
# Observed 2026-08-06 gating TG-60: both arms returned INTEGRITY: OK and the comparison was refused. The
# gate that demands the evidence and the gate that produces it disagreed, and the edit was unmergeable.
#
# So this measures a different thing. It RE-JUDGES a FIXED set of captured sessions under both rubrics:
# the triage runs are data, identical in both arms, so triage nondeterminism is exactly zero and the
# rubric is the only variable. That is a stronger A/B than the change gate's, not a weaker one — and it
# is the only shape in which two rubric versions may be compared (gate.VerifyComparableRejudge, which
# INVERTS the version check: the arms must DIFFER, or the comparison is vacuous).
#
#   eval/eval-gate.sh rubric [sessions.json]     # default eval/sessions.json (committed capture)
if [ "$MODE" = "rubric" ]; then
  SESSIONS="${2:-eval/sessions.json}"
  [ -f "$HERE/$SESSIONS" ] || { echo "rubric gate: no captured session set at $SESSIONS — capture one with a corpus run first." >&2; exit 1; }
  BASE_REF_NAME="${TG_BASE_REF:-origin/main}"
  git -C "$HERE" fetch --quiet origin main 2>/dev/null || echo "warn: could not fetch origin/main — using the local ref"
  BASE_REF=$(git -C "$HERE" rev-parse "$BASE_REF_NAME")
  CAND_REF=$(git -C "$HERE" rev-parse HEAD)
  echo "== rubric gate: re-judging $SESSIONS under candidate ${CAND_REF:0:12} vs base ${BASE_REF_NAME} @ ${BASE_REF:0:12} =="

  TMPROOT=$(mktemp -d)
  BASE_WT="$TMPROOT/tg-base"
  git -C "$HERE" worktree add --quiet --detach "$BASE_WT" "$BASE_REF"
  # THE SAME BYTES IN BOTH ARMS. The sessions are the fixed input; copying the candidate's file into the
  # base worktree is what makes "only the rubric moved" true rather than merely intended.
  mkdir -p "$BASE_WT/$(dirname "$SESSIONS")"
  cp -f "$HERE/$SESSIONS" "$BASE_WT/$SESSIONS"

  rubric_version() { python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["version"])' "$1"; }
  echo "== [candidate] re-judge under rubric $(rubric_version "$HERE/core/judge/rubric.json") =="
  (cd "$HERE" && go run ./tools/rejudge -gateway "http://127.0.0.1:$LPORT" "$SESSIONS")
  cp -f "$HERE/$SESSIONS.rejudge.json" "$OUT/rejudge.cand.json"

  echo "== [base] re-judge under rubric $(rubric_version "$BASE_WT/core/judge/rubric.json") =="
  (cd "$BASE_WT" && go run ./tools/rejudge -gateway "http://127.0.0.1:$LPORT" "$SESSIONS")
  cp -f "$BASE_WT/$SESSIONS.rejudge.json" "$OUT/rejudge.base.json"

  # THE NEGATIVE CONTROLS MUST STILL BE SUPPLIED, and the first run of this mode proved why: without
  # --controls the gate returned INCONCLUSIVE — "the run did not measure a capability this gate exists to
  # bar on" — and refused to certify. Correct, and the right refusal to hit: a rubric edit cannot change
  # whether the agent PROPOSED on a benign incident (that is fixed data in a re-judge, identical in both
  # arms), but "cannot have changed" is not "was measured", and this gate does not certify the unmeasured.
  # The captured controls scorecard carries the proposals as they actually happened.
  CTRL_ARG=()
  if [ -f "$HERE/eval/controls-scorecard.json" ]; then
    CTRL_ARG=(--controls "$HERE/eval/controls-scorecard.json")
  else
    echo "warn: no eval/controls-scorecard.json — the gate will report INCONCLUSIVE on the negative-control bar" >&2
  fi

  echo "== deterministic rubric gate (same sessions, two rubrics — the rubric is the only variable) =="
  go run ./tools/evalgate --mode change --rejudge "${CTRL_ARG[@]}" \
    --base "$OUT/rejudge.base.json" --candidate "$OUT/rejudge.cand.json" \
    --base-git-sha "$BASE_REF" --git-sha "$CAND_REF" \
    --archive-dir "$HERE/eval/history"
  exit $?
fi

# --------------------------------------------------------------------------------------------------------
if [ "$MODE" = "trend" ]; then
  # TREND-WATCH (the nightly): measure the CURRENT checkout (main) N times vs the COMMITTED baseline for long-
  # horizon drift tracking, then SELF-REFRESH the committed baseline on a clean, non-regressing run so the
  # anchor tracks main and never goes stale. A regressing run files an issue and does NOT refresh.
  CAND_ARGS=(); CTRL_ARGS=()
  for k in $(seq 1 "$RUNS"); do
    echo "== trend run ${k}/${RUNS} (main measurement) =="
    sc="$OUT/scorecard.run${k}.json"; ctrl="$OUT/controls.run${k}.json"
    run_arm "main" "$HERE" 'TestEvalCorpusOnBox|TestEvalControlsOnBox' "$sc" "$ctrl"
    CAND_ARGS+=(--candidate "$sc")
    [ -f "$ctrl" ] && CTRL_ARGS+=(--controls "$ctrl")
  done
  MAIN_REF=$(git -C "$HERE" rev-parse HEAD)
  echo "== deterministic trend-watch (main vs committed baseline ${BASELINE}) + self-refresh =="
  go run ./tools/evalgate --mode trend --runs "$RUNS" $EXPECT_N_ARG \
    --baseline "$BASELINE" --refresh-baseline "$BASELINE" --git-sha "$MAIN_REF" \
    --archive-dir "$HERE/eval/history" \
    "${CAND_ARGS[@]}" "${CTRL_ARGS[@]}"
  exit $?
fi

# --------------------------------------------------------------------------------------------------------
# CHANGE GATE (default): candidate arm vs a FRESH origin/main base arm, measured in the SAME window (TG-64).
BASE_REF_NAME="${TG_BASE_REF:-origin/main}"
git -C "$HERE" fetch --quiet origin main 2>/dev/null || echo "warn: could not fetch origin/main — using the local ref"
BASE_REF=$(git -C "$HERE" rev-parse "$BASE_REF_NAME")
CAND_REF=$(git -C "$HERE" rev-parse HEAD)
echo "== change gate: candidate ${CAND_REF:0:12} vs FRESH base arm ${BASE_REF_NAME} @ ${BASE_REF:0:12} =="

# Check out the base arm in a temp worktree OUTSIDE the repo (no nesting/gitignore issues).
TMPROOT=$(mktemp -d)
BASE_WT="$TMPROOT/tg-base"
git -C "$HERE" worktree add --quiet --detach "$BASE_WT" "$BASE_REF"
# Both arms evaluate the IDENTICAL eval set (the candidate's data fixtures); only the SYSTEM-under-test differs.
cp -f "$HERE/eval/corpus.json" "$HERE/eval/controls.json" "$HERE/eval/estate_fixture.json" "$BASE_WT/eval/"

BASE_ARGS=(); CAND_ARGS=(); CTRL_ARGS=()
for k in $(seq 1 "$RUNS"); do
  echo "== gate run ${k}/${RUNS} =="
  base_sc="$OUT/scorecard.base.run${k}.json"
  cand_sc="$OUT/scorecard.cand.run${k}.json"
  cand_ctrl="$OUT/controls.run${k}.json"
  # Alternate arm order every run to cancel time-of-day drift (odd: candidate→base; even: base→candidate).
  if [ $((k % 2)) -eq 1 ]; then
    run_arm "candidate" "$HERE"    'TestEvalCorpusOnBox|TestEvalControlsOnBox' "$cand_sc" "$cand_ctrl"
    run_arm "base"      "$BASE_WT" 'TestEvalCorpusOnBox'                       "$base_sc"
  else
    run_arm "base"      "$BASE_WT" 'TestEvalCorpusOnBox'                       "$base_sc"
    run_arm "candidate" "$HERE"    'TestEvalCorpusOnBox|TestEvalControlsOnBox' "$cand_sc" "$cand_ctrl"
  fi
  BASE_ARGS+=(--base "$base_sc")
  CAND_ARGS+=(--candidate "$cand_sc")
  [ -f "$cand_ctrl" ] && CTRL_ARGS+=(--controls "$cand_ctrl")
done

echo "== deterministic change-gate (candidate vs FRESH base arm, drift-cancelled + absolute floors) =="
# The base arm's own resolved sha rides the archived comparator (self-verifying change records).
BASE_REF="$(git -C "$BASE_WT" rev-parse HEAD 2>/dev/null || echo unknown)"
go run ./tools/evalgate --mode change --runs "$RUNS" $EXPECT_N_ARG --git-sha "$CAND_REF" \
  --base-git-sha "$BASE_REF" \
  --archive-dir "$HERE/eval/history" \
  "${BASE_ARGS[@]}" "${CAND_ARGS[@]}" "${CTRL_ARGS[@]}"
