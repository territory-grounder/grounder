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
#   TG_SSH_KEY   (default ~/.ssh/one_key)   TG_BOX (default root@dc1tg01)   TG_EVAL_PORT (default 4010)
#   TG_EVAL_RUNS (per arm; default 1 for the fast change gate, else 3)          TG_GATE_OUT (default eval/out)
#   TG_EVAL_LIMIT (corpus subset; default 8 for the fast change gate, else the full corpus) — exported to go test
#   TG_EVAL_FULL (=1 -> full 3-run x full-corpus rigor for the change gate; `make eval-gate-full` sets it)
#   TG_BASELINE  (default eval/baseline-scorecard.json)   TG_EVAL_MAX_RETRY (default 2, per-arm 429 reruns)
#   TG_BASE_REF  (default origin/main — the branch's merge target; the fresh base arm is checked out here)
set -euo pipefail

MODE="${1:-change}"
[ "$MODE" = "gate" ] && MODE="change"   # back-compat alias
KEY="${TG_SSH_KEY:-$HOME/.ssh/one_key}"
BOX="${TG_BOX:-root@dc1tg01}"
LPORT="${TG_EVAL_PORT:-4010}"
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
REUSE_TUN=0   # 1 = we ADOPTED a pre-existing healthy tunnel; never kill a forwarder we did not open (TG-117)
BASE_WT=""
TMPROOT=""
cleanup() {
  if [ "${REUSE_TUN:-0}" -eq 0 ] && [ -n "${TUN_PID:-}" ]; then kill "$TUN_PID" 2>/dev/null || true; fi
  [ -n "${BASE_WT:-}" ] && git -C "$HERE" worktree remove --force "$BASE_WT" 2>/dev/null || true
  [ -n "${TMPROOT:-}" ] && rm -rf "$TMPROOT" 2>/dev/null || true
}
trap cleanup EXIT

# Resolve the gateway master key + LibreNMS token from the box .env (never printed, never committed).
MK=$(ssh -i "$KEY" -o StrictHostKeyChecking=no "$BOX" 'grep "^LITELLM_MASTER_KEY=" /srv/tg/deploy/.env | cut -d= -f2-')
LT=$(ssh -i "$KEY" -o StrictHostKeyChecking=no "$BOX" 'grep "^LIBRENMS_TOKEN=" /srv/tg/deploy/.env | cut -d= -f2-' || true)
[ -n "$MK" ] || { echo "could not read LITELLM_MASTER_KEY from $BOX"; exit 1; }

# Open (or REUSE) the tunnel: local $LPORT -> box localhost:4000 (the litellm gateway).
# Bind the LOCAL end to 127.0.0.1 explicitly (not the ambiguous "localhost", which makes ssh ALSO try to
# bind [::1] and emit "bind [::1]:PORT: Cannot assign requested address" on hosts without IPv6 loopback — and,
# worse, lets the eval client resolve localhost→::1 FIRST and hit a dead address, the intermittent "0 sessions
# judged" abort). Keepalive holds the long tunnel open across idle gaps so a mid-run arm never loses it.
#
# TG-117: a run killed mid-flight (Ctrl-C / OOM) leaves a stale forwarder bound to $LPORT; the old code then
# hard-FAILED the next run with "bind: Address already in use". Pre-flight instead: if a forwarder already
# holds $LPORT AND actually forwards to the live gateway, REUSE it (and never kill it on exit — it may be a
# parallel run's); if it holds the port but is dead, close it and re-open; otherwise open fresh. Any HTTP
# response (even 401/404) through the port proves the tunnel forwards; only a 000 (no connection) is "dead".
probe_tunnel() {  # 0 = a live gateway answers through 127.0.0.1:$LPORT
  local code
  code=$(curl -sS -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${LPORT}/health/liveliness" 2>/dev/null || echo 000)
  [ "$code" != "000" ]
}
EXISTING_TUN=$(pgrep -f "127.0.0.1:${LPORT}:localhost:4000" | head -1 || true)
if [ -n "$EXISTING_TUN" ] && probe_tunnel; then
  echo "== reusing healthy existing tunnel on 127.0.0.1:${LPORT} (pid ${EXISTING_TUN}) =="
  TUN_PID="$EXISTING_TUN"; REUSE_TUN=1
else
  if [ -n "$EXISTING_TUN" ]; then
    echo "== stale tunnel on 127.0.0.1:${LPORT} (pid ${EXISTING_TUN}) — closing and re-opening =="
    kill "$EXISTING_TUN" 2>/dev/null || true
    sleep 1
  fi
  ssh -i "$KEY" -o StrictHostKeyChecking=no -o ServerAliveInterval=30 -o ServerAliveCountMax=6 \
      -f -N -L "127.0.0.1:${LPORT}:localhost:4000" "$BOX" || true
  sleep 2
  TUN_PID=$(pgrep -f "127.0.0.1:${LPORT}:localhost:4000" | head -1 || true)
  REUSE_TUN=0
  probe_tunnel || { echo "FATAL: no working gateway tunnel on 127.0.0.1:${LPORT} -> ${BOX}:4000" >&2; exit 1; }
fi

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

echo "== deterministic change-gate (candidate vs FRESH base arm, drift-cancelled) =="
go run ./tools/evalgate --mode change --runs "$RUNS" $EXPECT_N_ARG --git-sha "$CAND_REF" \
  "${BASE_ARGS[@]}" "${CAND_ARGS[@]}" "${CTRL_ARGS[@]}"
