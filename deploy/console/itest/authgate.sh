#!/usr/bin/env bash
# Runtime integration test for the console auth-gate (nginx.conf + login.html), invoked by
# deploy/console_authgate_integration_test.go (which SKIPS when docker is unavailable). It spins up the REAL
# nginx with the repo's config against a stub grounder and asserts the behavioural contract the static config
# oracles cannot: an unauthenticated request leaks NOTHING (login page only, never the bundle / fixtures /
# /v1 names), an authenticated request gets the app, a grounder blip fails to the login page (not a bare
# 500), and hardened headers ship on every response — including the healthz probe. Exit 0 = all pass.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
CONSOLE="$(cd "$HERE/.." && pwd)"                 # deploy/console
# TG-492: every fixture name carries this run's PID, and the host port is DYNAMIC (docker picks a free
# one) unless CONSOLE_IT_PORT pins it. The old fixed name + fixed 18097 meant two concurrent `make all`
# runs (parallel sessions/worktrees on one box) yanked each other's containers mid-probe and the loser
# reported a completely credible auth-gate failure ("want [200] got [000]") — the grade-a-stranger's-
# artifact hazard, container edition. Uniqueness makes concurrent runs independent; the trap still
# reaps THIS run's fixtures exactly.
RUN=$$
NET=tgconsole_authgate_it_${RUN}; STUB=${NET}_grounder; NGINX=${NET}_console
cleanup(){ docker rm -f "$STUB" "$NGINX" >/dev/null 2>&1; docker network rm "$NET" >/dev/null 2>&1; }
trap cleanup EXIT; cleanup
docker network create "$NET" >/dev/null || exit 2
docker run -d --name "$STUB" --network "$NET" --network-alias grounder \
  -v "$HERE/stub_grounder.py:/stub.py:ro" python:3.12-alpine python /stub.py >/dev/null || exit 2
if [ -n "${CONSOLE_IT_PORT:-}" ]; then PORTMAP="127.0.0.1:${CONSOLE_IT_PORT}:8080"; else PORTMAP="127.0.0.1:0:8080"; fi
docker run -d --name "$NGINX" --network "$NET" -p "$PORTMAP" \
  -v "$CONSOLE/nginx.conf:/etc/nginx/conf.d/default.conf:ro" \
  -v "$CONSOLE/v2/index.html:/usr/share/nginx/html/index.html:ro" \
  -v "$CONSOLE/login.html:/usr/share/nginx/login/login.html:ro" \
  nginx:1.27-alpine >/dev/null || exit 2
PORT=$(docker port "$NGINX" 8080/tcp | head -1 | sed 's/.*://')
[ -n "$PORT" ] || { echo "TG-492: could not resolve the dynamically-mapped port"; exit 2; }
B="http://localhost:$PORT"
curl -s -o /dev/null --retry-connrefused --retry-all-errors --retry 30 --retry-delay 1 "$B/" || true
if ! curl -s -o /dev/null -w '%{http_code}' "$B/" | grep -q 200; then
  echo "READINESS FAILED — nginx not serving:"; docker logs "$NGINX" 2>&1 | tail -15; exit 1
fi
pass=0; fail=0
chk(){ [ "$2" = "$3" ] && { echo "  ok   $1"; pass=$((pass+1)); } || { echo "  FAIL $1 — want [$3] got [$2]"; fail=$((fail+1)); }; }
has(){ echo "$2" | grep -q "$3" && { echo "  ok   $1"; pass=$((pass+1)); } || { echo "  FAIL $1 — [$3] absent"; fail=$((fail+1)); }; }
no(){ echo "$2" | grep -q "$3" && { echo "  FAIL $1 — [$3] LEAKED"; fail=$((fail+1)); } || { echo "  ok   $1"; pass=$((pass+1)); }; }

U=$(curl -s "$B/")
chk "unauth GET / → 200" "$(curl -s -o /dev/null -w '%{http_code}' "$B/")" "200"
has "unauth GET / serves login" "$U" "Operator sign-in required"
no  "unauth GET / no bundle (const LEDGER)" "$U" "const LEDGER"
no  "unauth GET / no SESSIONS var" "$U" "const SESSIONS"
no  "unauth GET / no /v1 endpoint names" "$U" "v1/ledger"
UI=$(curl -s "$B/index.html")
no  "unauth /index.html no bundle" "$UI" "const LEDGER"
has "unauth /index.html → login" "$UI" "Operator sign-in required"
A=$(curl -s -H "Cookie: tg_session=valid" "$B/")
chk "authed GET / → 200" "$(curl -s -o /dev/null -w '%{http_code}' -H 'Cookie: tg_session=valid' "$B/")" "200"
has "authed GET / serves app" "$A" "const LEDGER"
no  "authed GET / not login" "$A" "Operator sign-in required"
has "GET /login.html serves login" "$(curl -s "$B/login.html")" "Operator sign-in required"
chk "healthz body" "$(curl -s "$B/console-healthz")" "ok"
chk "login POST → 200" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'X-TG-Operator: a' -H 'Authorization: Bearer p' "$B/api/v1/session")" "200"
H=$(curl -s -D - -o /dev/null "$B/login.html")
has "CSP on login" "$H" "Content-Security-Policy"
has "XFO DENY on login" "$H" "X-Frame-Options: DENY"
has "nosniff on login" "$H" "X-Content-Type-Options: nosniff"
no  "no nginx version banner" "$H" "nginx/1"
HZ=$(curl -s -D - -o /dev/null "$B/console-healthz")
has "healthz keeps CSP (default_type, not add_header)" "$HZ" "Content-Security-Policy"
docker stop "$STUB" >/dev/null 2>&1
D=$(curl -s "$B/")
chk "grounder-DOWN GET / → 200 (not bare 500)" "$(curl -s -o /dev/null -w '%{http_code}' "$B/")" "200"
has "grounder-DOWN serves login (fail-closed but usable)" "$D" "Operator sign-in required"
no  "grounder-DOWN no bundle" "$D" "const LEDGER"

echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && exit 0 || exit 1
