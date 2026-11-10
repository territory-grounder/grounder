#!/usr/bin/env bash
# Run the grounding/quality eval NATIVELY ON THE BOX (dc1tg01) — NO ssh tunnel.
#
# WHY (2026-07-25, axis R1 harness reliability): the tunnel-based runners (eval-gate.sh, run-on-box.sh) push
# every eval HTTP connection through ONE `ssh -L` forward, which resets channels under the full-corpus load —
# `EOF` / "connection reset by peer" on ~half the proposal-heavy sessions (they make the most model calls),
# degrading the run and collapsing proposals to zero so falsifiable_prediction can never be measured. A
# controlled on-box parallel burst PROVED the gateway itself is fine (12 `fast` + 6 `primary` parallel calls →
# all 200); it is the single tunnel that fails. So: build a static `go test` binary, ship it + the eval
# fixtures to the box, and run it against the DIRECT loopback gateway (127.0.0.1:4000). Read-only, mutation OFF.
#
# Env: TG_SSH_KEY (~/.ssh/one_key) · TG_BOX (root@dc1tg01) · TG_EVAL_LIMIT (corpus cap; default full) ·
#      TG_EVAL_RUN (default TestEvalCorpusOnBox). Secrets are read from the box at runtime, never printed.
set -euo pipefail
KEY="${TG_SSH_KEY:-$HOME/.ssh/one_key}"
BOX="${TG_BOX:-root@dc1tg01}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
RUN="${TG_EVAL_RUN:-TestEvalCorpusOnBox}"
DIR="/tmp/tgeval"

echo "== build static eval test binary (CGO off → runs on the box unchanged) =="
CGO_ENABLED=0 go test -c "$HERE/eval/" -o /tmp/eval.test

echo "== ship binary + fixtures to ${BOX}:${DIR} =="
ssh -i "$KEY" -o StrictHostKeyChecking=no "$BOX" "mkdir -p $DIR"
scp -i "$KEY" -o StrictHostKeyChecking=no /tmp/eval.test "$HERE"/eval/*.json "$BOX:$DIR/" >/dev/null

echo "== run the eval ON THE BOX (direct 127.0.0.1:4000, no tunnel) =="
# The remote block is a QUOTED heredoc — nothing here expands locally (the key never touches this shell). The
# caller's TG_EVAL_LIMIT/TG_EVAL_RUN are passed as an env prefix to the remote bash.
ssh -i "$KEY" -o StrictHostKeyChecking=no -o ServerAliveInterval=30 "$BOX" \
  "TG_EVAL_RUN='${RUN}' TG_EVAL_LIMIT='${TG_EVAL_LIMIT:-}' bash -s" <<'REMOTE'
set -euo pipefail
cd /tmp/tgeval
lc=$(docker ps -q -f name=litellm | head -1)
MK=$(docker exec "$lc" sh -c 'tr "\0" "\n" < /proc/1/environ' 2>/dev/null | sed -n 's/^LITELLM_MASTER_KEY=//p' | head -1)
[ -n "$MK" ] || { echo "could not resolve LITELLM_MASTER_KEY from the running litellm"; exit 1; }
LT=$(grep '^LIBRENMS_TOKEN=' /srv/tg/deploy/.env | cut -d= -f2- | sed -e "s/^[\"']//" -e "s/[\"']$//")
export TG_EVAL_GATEWAY='http://127.0.0.1:4000'
export LITELLM_MASTER_KEY="$MK" LIBRENMS_TOKEN="$LT"
export TG_LIBRENMS_URL='https://dc1nms01.example.net' TG_LIBRENMS_INSECURE=true
[ -n "$TG_EVAL_LIMIT" ] && export TG_EVAL_LIMIT
./eval.test -test.run "$TG_EVAL_RUN" -test.v -test.timeout 40m
REMOTE

echo "== fetch scorecard =="
scp -i "$KEY" -o StrictHostKeyChecking=no "$BOX:$DIR/scorecard.json" "$HERE/eval/scorecard.json" >/dev/null
echo "== scorecard =="; cat "$HERE/eval/scorecard.json"
