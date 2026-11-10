#!/usr/bin/env bash
# Oracle for verify-deploy.sh — the post-deploy gate that exists because a failed deploy reported SUCCESS
# for ~14 minutes on 2026-07-28 while the control plane was down.
#
# A gate is only worth its runtime if it FAILS on the state it was built to catch. This drives the script
# against a local server returning controlled status codes, and asserts both directions:
#   - it PASSES on the healthy shape (200 / 401), and
#   - it FAILS on the outage shape (grounder unreachable behind a working nginx), which is the case the
#     old pipeline could not distinguish from success.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
fails=0
ok() { if [ "$1" = "$2" ]; then echo "  ok   — $3"; else echo "  FAIL — $3 (got rc=$1, want rc=$2)"; fails=1; fi; }

# A tiny server whose responses are driven by two files, so each case is exact and hermetic.
start_fake() { # $1=root-status $2=api-status  -> prints port
  local rootst="$1" apist="$2" port
  port=$(python3 - <<'PY'
import socket
s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()
PY
)
  python3 - "$port" "$rootst" "$apist" >/dev/null 2>&1 <<'PY' &
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port, rootst, apist = int(sys.argv[1]), int(sys.argv[2]), int(sys.argv[3])
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        code = apist if self.path.startswith("/api/") else rootst
        self.send_response(code); self.end_headers(); self.wfile.write(b"x")
    def log_message(self, *a): pass
HTTPServer(("127.0.0.1", port), H).serve_forever()
PY
  echo "$!:$port"
}

# THE READINESS PROBE FAILS CLOSED, AND THAT IS THE POINT OF THIS FUNCTION.
#
# It used to loop 40 x 0.1s waiting for the fake server and then run verify-deploy.sh REGARDLESS. On a loaded
# runner (a CI job where apt-get alone took 100s) the server had not bound yet, verify-deploy.sh probed a dead
# port, and the HEALTHY case reported rc=1 — a green product reported as broken. That happened on pipeline
# 44180 and stopped a five-MR merge train.
#
# ★ THE NEGATIVE CASES CANNOT DETECT THIS, AND THAT IS WHY IT LOOKED LIKE A PRODUCT DEFECT. Every other case
# EXPECTS a non-zero rc, so a dead mock satisfies all of them — they pass for the wrong reason. Only the one
# positive case can reveal that the harness never started, so the whole suite degrades to a single meaningful
# assertion exactly when the environment is worst. A harness that cannot distinguish "the thing under test
# failed" from "I never started the thing under test" reports the wrong subject.
#
# So: wait longer (the budget is generous because a slow runner is not a defect), and if the server never
# answers, ABORT with a message naming the harness rather than running the assertion and blaming the product.
run_case() { # $1=root $2=api $3=want-rc $4=label
  local h port pid ready=0
  h=$(start_fake "$1" "$2"); pid="${h%%:*}"; port="${h##*:}"
  for _ in $(seq 1 150); do
    # NO -f HERE. Readiness means "the server ANSWERS", not "the server answers 2xx": the 502/502 and
    # 404/401 cases serve errors deliberately, so a -f probe can never succeed against a perfectly healthy
    # fake and would report a HARNESS FAILURE for the very cases it is meant to protect. curl without -f
    # returns 0 for any HTTP response and non-zero only when the connection itself fails — which is exactly
    # the distinction being drawn. (The original probe hid this: its `&& break` never fired for those cases
    # either, so it silently degraded to a fixed 4-second sleep.)
    curl -s -o /dev/null "http://127.0.0.1:$port/" >/dev/null 2>&1 && { ready=1; break; }
    sleep 0.1
  done
  if [ "$ready" != 1 ]; then
    kill "$pid" 2>/dev/null || true
    echo "  HARNESS FAILURE — the fake server on port $port never answered within 15s, so this case tested"
    echo "  NOTHING. Reporting it as a product failure would blame verify-deploy.sh for the runner being slow."
    echo "  (case: $4)"
    fails=1
    return 1
  fi
  bash "$HERE/verify-deploy.sh" "http://127.0.0.1:$port" 5 2 >/dev/null 2>&1
  local rc=$?
  kill "$pid" 2>/dev/null || true
  ok "$rc" "$3" "$4"
}

echo "== verify-deploy.sh oracle =="
run_case 200 401 0 "HEALTHY: console 200 + grounder fail-closed 401 -> PASS"
run_case 200 502 1 "THE OUTAGE: console up but grounder unreachable (502) -> FAIL"
run_case 200 504 1 "grounder timing out (504) -> FAIL"
run_case 502 502 1 "whole stack down (502/502) -> FAIL"
run_case 200 200 1 "UNAUTHENTICATED 200 from whoami -> FAIL (that is an auth alarm, not health)"
run_case 404 401 1 "console artifact missing (404) -> FAIL"

if [ "$fails" != 0 ]; then echo "verify-deploy oracle: FAIL"; exit 1; fi
echo "verify-deploy oracle: PASS"
