#!/usr/bin/env bash
# POST-DEPLOY PROOF: assert the control plane is actually SERVING, from outside the box.
#
# "AWX reported successful" is not "the system is running". On 2026-07-28 a deploy left litellm, worker,
# grounder and console in state `Created` — started never, exited never — and the pipeline reported SUCCESS
# for ~14 minutes while TG triaged nothing and the console was unreachable. Anything that inspects only the
# playbook's exit status cannot tell that apart from a healthy rollout.
#
# The two probes, and why these two:
#   GET /                 must be 200   — nginx and the console container are up and serving the artifact.
#   GET /api/v1/whoami    must be 401   — the console is proxying to a LIVE grounder. 401 is the fail-closed
#                                         answer of a HEALTHY control plane (no session presented). A 502 or
#                                         504 is nginx failing to reach `grounder:8080` — the exact signature
#                                         of a container stuck in `Created`.
#
# A 401 is the strongest assertion available without putting an operator credential in CI, and it is the one
# that actually discriminates "grounder is running" from "grounder was never started". A 200 here would mean
# an UNAUTHENTICATED caller got an answer, which is its own alarm — so 200 is a failure too, not a success.
#
# Usage: verify-deploy.sh <base-url> [attempts] [sleep-seconds]
set -uo pipefail

BASE="${1:?usage: verify-deploy.sh <base-url> [attempts] [sleep-seconds]}"
ATTEMPTS="${2:-30}"
DELAY="${3:-10}"

root=000
api=000
for i in $(seq 1 "$ATTEMPTS"); do
  root=$(curl -s -o /dev/null -m 10 -w '%{http_code}' "$BASE/" 2>/dev/null || echo 000)
  api=$(curl -s -o /dev/null -m 10 -w '%{http_code}' "$BASE/api/v1/whoami" 2>/dev/null || echo 000)
  echo "  post-deploy probe $i/$ATTEMPTS: / =$root  /api/v1/whoami =$api"
  if [ "$root" = 200 ] && [ "$api" = 401 ]; then
    echo "deploy OK — control plane is serving (console 200, grounder reachable and fail-closed at 401)"
    exit 0
  fi
  [ "$i" -lt "$ATTEMPTS" ] && sleep "$DELAY"
done

echo "POST-DEPLOY VERIFICATION FAILED: the rollout reported success but the control plane is not serving."
echo "  last probe: / =$root   /api/v1/whoami =$api"
case "$api" in
  502|503|504) echo "  a $api on the api probe means nginx cannot reach grounder:8080 — check for containers"
               echo "  stuck in 'Created' and for a tg-secretenv resolve failure in the litellm-secrets init." ;;
  200)         echo "  a 200 on /api/v1/whoami means an UNAUTHENTICATED caller was answered — the session" ;;
  000)         echo "  no HTTP response at all — the origin is unreachable from here." ;;
esac
exit 1
