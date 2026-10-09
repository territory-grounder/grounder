#!/usr/bin/env bash
# Run the grounding/quality eval against the DEPLOYED TG on dc1tg01.
# The model gateway is loopback-only on the box, so we tunnel it to localhost and run `go test` here
# (where the Go toolchain + source live). Read-only, mutation OFF. Optional: TG_EVAL_LIMIT=5 for a quick pass.
set -euo pipefail
KEY="${TG_SSH_KEY:-$HOME/.ssh/one_key}"
BOX="${TG_BOX:-root@dc1tg01}"
LPORT="${TG_EVAL_PORT:-4010}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"

# resolve the gateway master key from the box .env (never printed)
MK=$(ssh -i "$KEY" "$BOX" 'grep "^LITELLM_MASTER_KEY=" /srv/tg/deploy/.env | cut -d= -f2-')
[ -n "$MK" ] || { echo "could not read LITELLM_MASTER_KEY from the box"; exit 1; }

# open the tunnel: local $LPORT -> box localhost:4000 (the litellm gateway). Bind 127.0.0.1 explicitly (not
# "localhost", which also tries [::1] and fails "Cannot assign requested address" on hosts without IPv6
# loopback); keepalive holds the tunnel across idle gaps.
ssh -i "$KEY" -o ServerAliveInterval=30 -o ServerAliveCountMax=6 -f -N -L "127.0.0.1:${LPORT}:localhost:4000" "$BOX"
TUN_PID=$(pgrep -f "127.0.0.1:${LPORT}:localhost:4000" | head -1 || true)
trap '[ -n "${TUN_PID:-}" ] && kill "$TUN_PID" 2>/dev/null || true' EXIT

cd "$HERE"
TG_EVAL_GATEWAY="http://127.0.0.1:${LPORT}" LITELLM_MASTER_KEY="$MK" \
  go test ./eval/ -run TestEvalCorpusOnBox -v -timeout 40m -count=1

echo "== scorecard =="; cat eval/scorecard.json
