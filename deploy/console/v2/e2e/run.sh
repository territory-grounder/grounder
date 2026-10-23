#!/usr/bin/env bash
# Serve the SERVED console index.html over http (so same-origin /api interception works), run EVERY
# Playwright oracle against it, then tear the server down. Exit code is the suite's. Run from anywhere.
#
# Two things this script used to get wrong, both of which made the suite quieter than it looked:
#
#  1. IT RAN 2 OF THE 7 ORACLES. The loop listed tracer.mjs and secrets.mjs only, so nav-switch,
#     boot-resilience, deeplink, estatedepth and band-cell existed in this directory and were never
#     executed by anything. The list is now derived from the directory, so a new *.mjs oracle is picked up
#     by existing — it cannot be added and silently skipped.
#
#  2. IT COULD GRADE A DIFFERENT BUILD. `python3 -m http.server $PORT &` does not fail this script when the
#     port is already taken: the backgrounded server dies, the curl readiness probe is answered by WHATEVER
#     else is listening, and the oracles run against a stranger's artifact. That happened during this
#     script's revision — an unrelated ssh tunnel on 8099 served a build 449 bytes different from the one
#     on disk and produced a completely credible failure. serve.mjs exits non-zero if it cannot bind, and
#     the readiness probe now also proves the server is serving OUR file, byte for byte.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
CONSOLE_DIR="$(cd "$HERE/.." && pwd)"   # deploy/console/v2
PORT="${CONSOLE_E2E_PORT:-8137}"

cd "$HERE"
node serve.mjs "$CONSOLE_DIR" "$PORT" &
HTTPD=$!
trap 'kill "$HTTPD" 2>/dev/null || true' EXIT

ready=0
for _ in $(seq 1 40); do
  if curl -sf "http://127.0.0.1:$PORT/index.html" >/dev/null 2>&1; then ready=1; break; fi
  # If the server process is already gone, the port was taken — do not keep probing a stranger.
  kill -0 "$HTTPD" 2>/dev/null || { echo "run.sh: serve.mjs exited (port $PORT unavailable)"; exit 1; }
  sleep 0.3
done
[ "$ready" = 1 ] || { echo "run.sh: static server never became ready on $PORT"; exit 1; }

served=$(curl -sf "http://127.0.0.1:$PORT/index.html" | wc -c)
ondisk=$(wc -c < "$CONSOLE_DIR/index.html")
if [ "$served" != "$ondisk" ]; then
  echo "run.sh: served index.html is $served bytes but $CONSOLE_DIR/index.html is $ondisk — refusing to run"
  exit 1
fi

rc=0
# Each oracle gets a HARD time limit. Without one, a single test that never exits stops the whole serial
# loop: console-foundations.mjs leaked a second browser (an inline `await (await chromium.launch())` whose
# handle nothing kept), printed "all checks passed", and then hung forever. Every oracle sorting after it
# never ran, and the suite looked like a slow healthy run rather than a truncated one. A timeout turns that
# into a named failure.
PER_TEST_TIMEOUT="${CONSOLE_E2E_TIMEOUT:-300}"
# postdeploy/ IS DELIBERATELY OUT OF THIS GLOB (which is non-recursive). Those checks need a real
# credential and a reachable deployment, and they answer a question this suite structurally cannot: all
# 60 oracles below stub /v1/** — including /v1/whoami, the call that flips liveState.on — so they grade a
# SIMULATED live shell and would pass on a page that shows an operator nothing.
#
# They are a separate directory rather than a skip-if-unset guard in this loop, because a check that
# silently no-ops when its env is missing is how `make all` came to be green while skipping 34 DB tests.
# Absent is visible; skipped is not. Run them by hand after a deploy — see postdeploy/live-authenticated.mjs.
for t in $(ls -1 ./*.mjs | sed 's#^\./##' | grep -v '^serve\.mjs$' | sort); do
  echo "== console e2e: $t =="
  if CONSOLE_BASE="http://127.0.0.1:$PORT" timeout "$PER_TEST_TIMEOUT" node "$t"; then :; else
    code=$?
    [ "$code" = 124 ] && echo "run.sh: $t EXCEEDED ${PER_TEST_TIMEOUT}s and was killed — it did not finish, so it proved nothing"
    rc=1
  fi
done
exit $rc
