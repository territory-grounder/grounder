#!/usr/bin/env bash
# Serve the SERVED console index.html over http (so same-origin /api interception works), run the Playwright
# tracer e2e oracle against it, then tear the server down. Exit code is the test's. Run from anywhere.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
CONSOLE_DIR="$(cd "$HERE/.." && pwd)"   # deploy/console/v2
PORT="${CONSOLE_E2E_PORT:-8099}"

cd "$CONSOLE_DIR"
python3 -m http.server "$PORT" >/dev/null 2>&1 &
HTTPD=$!
trap 'kill "$HTTPD" 2>/dev/null || true' EXIT
# wait for the static server to accept connections
for _ in $(seq 1 20); do curl -sf "http://127.0.0.1:$PORT/index.html" >/dev/null 2>&1 && break; sleep 0.3; done

cd "$HERE"
rc=0
for t in tracer.mjs secrets.mjs; do
  echo "== console e2e: $t =="
  CONSOLE_BASE="http://127.0.0.1:$PORT" node "$t" || rc=1
done
exit $rc
