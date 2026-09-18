#!/usr/bin/env bash
# ACTUATION-GUARD COVERAGE CHECK (TG-281 + TG-123).
#
# WHAT IT ASSERTS, in both directions:
#   1. TG-281: every host that ACCEPTS TG's actuation SSH key also PINS that key to the forced command.
#      A host that accepts the key without `command="/usr/local/sbin/tg-actuator-guard"` gives a leaked
#      key arbitrary root, and TG's own gates cannot help — they bind the commands TG constructs, not
#      the ones a stolen key sends.
#   2. TG-123: every host TG EXPECTS to actuate over SSH actually accepts the key. The expected
#      population is derived from the RUNNING actuation plane — the guest allowlist intersected with
#      "an allowed unit or container exists on the host" — never from this script's imagination.
#
# WHY BOTH DIRECTIONS. The first version counted only hosts that already HAD the key, so its denominator
# was its own conclusion: a host that lost the key (rebuilt from an older image, a new target provisioned
# without it — canary bug #4) fell OUT of the population instead of failing it. Measured 2026-08-07 it
# reported PASS 7/7 while 13 of 20 allowlisted hosts had no key at all. Those 13 turned out to have no
# actuation surface either — consistent, but only by luck, and this check could not have told the
# difference. A coverage check whose denominator can shrink to fit the failure is a coverage claim, not
# a check.
#
# WHY A RECURRING CHECK AND NOT A ONE-OFF ROLLOUT. Coverage measured once is coverage that drifts. A host
# rebuilt from an older image reopens the TG-281 hole; a guest gaining an allowed unit (or a new
# allowlist entry) opens the TG-123 hole — actuation then dies on the SSH handshake, masquerading as a
# transport failure. Both drift without a commit, so per-MR CI cannot see them.
#
# WHAT IT CANNOT SEE, stated so its green is not over-read: it enumerates hosts through the AWX
# inventory, so a host AWX cannot reach is NOT scanned. An ALLOWLISTED host that produces no probe line
# FAILS the check (unproven is not proven safe); other unreachable hosts are only counted.
set -uo pipefail

# Auth matches what actually exists in CI (TG-402): the group-level AWX_TOKEN bearer the deploy job and
# check-deploy-host-cosign.sh already use. The first version required an AWX_AUTH basic-auth pair that
# was provisioned nowhere — one more way this check could never actually have run.
: "${AWX_URL:=https://awx.example.net}"
: "${AWX_TOKEN:?set AWX_TOKEN — the bearer token the deploy job uses}"
# TWO inventories, on purpose (first live run, pipeline 46680): the plane-env read targets the deploy
# inventory (11, "Territory Grounder" — exactly dc1tg01), while the estate probe MUST sweep the
# full estate inventory (2, "Proxmox Dynamic Inventory", ~200 hosts). Using 11 for both probed exactly
# one host and reported all 20 allowlisted guests unproven — the vacuity floors caught it, which is
# their job, but the fix is the right denominator, not a softer floor.
: "${TG_PLANE_INVENTORY:=11}"
: "${TG_ESTATE_INVENTORY:=2}"
: "${TG_ACTUATION_PUBKEY:?set TG_ACTUATION_PUBKEY to the base64 key body (field 2 of the .pub)}"
# The docker host running the actuation plane — the limit for the env read.
: "${TG_PLANE_HOST:=dc1tg01}"
# Poll cadence for status waits AND stdout-race retries (TG-531). The drill sets 0 to run instantly.
: "${TG_ADHOC_POLL_DELAY:=5}"

echo "== actuation-guard coverage =="

# launch_adhoc <inventory> <module_args> [limit] -> prints the ad-hoc id, empty on failure.
launch_adhoc() {
  local inv="$1" args="$2" limit="${3:-}" body
  # jq, not python3: the ci-deploy-tools image ships curl+jq+bash only. The first version of this
  # script used python3 here — a THIRD reason it could never have run in the job that carries it.
  if [ -n "$limit" ]; then
    body=$(jq -n --arg args "$args" --arg lim "$limit" \
      '{module_name:"shell", module_args:$args, credential:24, become_enabled:true, limit:$lim}')
  else
    body=$(jq -n --arg args "$args" \
      '{module_name:"shell", module_args:$args, credential:24, become_enabled:true}')
  fi
  curl -sk -H "Authorization: Bearer $AWX_TOKEN" -X POST "$AWX_URL/api/v2/inventories/$inv/ad_hoc_commands/" -m 60 \
    -H 'Content-Type: application/json' -d "$body" \
    | jq -r '.id // empty'
}

# wait_adhoc <id> -> waits to a terminal status, then prints the run's stdout text.
#
# TG-531: AWX flips a run's status to terminal BEFORE the callback receiver has written its stdout, so a
# fast run's first stdout fetch can return an empty body. Observed live 2026-08-22: ad-hoc 36781 was
# `successful` with full stdout via the API, while this gate read "" one fetch too early and REFUSED a
# healthy plane. So: retry the stdout fetch until non-empty (bounded), and name the ad-hoc id + final
# status on stderr for both failure shapes — a run that ended failed/error is that failure, not the race,
# and the next diagnosis should not need the AWX API by hand.
wait_adhoc() {
  local id="$1" st="" out=""
  # 120×5s: the estate sweep covers ~200 hosts and AWX runs them in fork-sized batches.
  for _ in $(seq 1 120); do
    st=$(curl -sk -H "Authorization: Bearer $AWX_TOKEN" "$AWX_URL/api/v2/ad_hoc_commands/$id/" -m 30 \
      | jq -r '.status // "unknown"')
    case "$st" in successful|failed|error) break;; esac
    sleep "$TG_ADHOC_POLL_DELAY"
  done
  if [ "$st" != successful ]; then
    echo "  ad-hoc $id ended status=$st — its stdout (below, possibly empty) is that failure's output" >&2
  fi
  for _ in $(seq 1 6); do
    out=$(curl -sk -H "Authorization: Bearer $AWX_TOKEN" "$AWX_URL/api/v2/ad_hoc_commands/$id/stdout/?format=txt" -m 60)
    [ -n "$out" ] && break
    sleep "$TG_ADHOC_POLL_DELAY"
  done
  [ -n "$out" ] || echo "  ad-hoc $id: stdout still EMPTY after 6 fetches (status=$st) — see the AWX run" >&2
  printf '%s\n' "$out"
}

# ---- 1. the EXPECTED population, read from the RUNNING plane (never a repo file) --------------------
# ★ NO JINJA BRACES in module_args, ever — AWX renders them (see check-deploy-host-cosign.sh).
envprobe='docker inspect territory-grounder-worker-actuate-1 2>/dev/null \
  | grep -oE "TG_(PROXMOX_ALLOWED_GUESTS|ACTUATION_ALLOWED_UNITS|ACTUATION_ALLOWED_CONTAINERS)=[^\"]*" \
  | sed "s/^/TGENV /"'
envid=$(launch_adhoc "$TG_PLANE_INVENTORY" "$envprobe" "$TG_PLANE_HOST")
[ -n "$envid" ] || { echo "  FORBIDDEN: could not launch the plane-env probe — the check proved nothing"; exit 1; }
envout=$(wait_adhoc "$envid")

allow=$(printf '%s\n' "$envout" | grep -o 'TGENV TG_PROXMOX_ALLOWED_GUESTS=[^ ]*' | head -1 | cut -d= -f2 | tr ',' ' ')
units=$(printf '%s\n' "$envout" | grep -o 'TGENV TG_ACTUATION_ALLOWED_UNITS=[^ ]*' | head -1 | cut -d= -f2 | tr ';' ' ')
containers=$(printf '%s\n' "$envout" | grep -o 'TGENV TG_ACTUATION_ALLOWED_CONTAINERS=[^ ]*' | head -1 | cut -d= -f2 | tr ';' ' ')

# VACUITY FLOOR on the denominator. An empty allowlist means the env read failed, the container was
# renamed, or actuation truly targets nothing — each needs a human, and continuing would assert
# coverage of an empty set.
if [ -z "$(echo $allow)" ]; then
  echo "  FORBIDDEN: could not read TG_PROXMOX_ALLOWED_GUESTS from the running actuation plane on $TG_PLANE_HOST."
  echo "  The expected population is unknown, so nothing below could mean anything."
  exit 1
fi

# Normalise unit names to their .service form, deduped, so `nginx;nginx.service` probes once.
unitsvc=$(for u in $units; do case "$u" in *.service) echo "$u";; *) echo "$u.service";; esac; done | sort -u | tr '\n' ' ')

# ---- 2. probe EVERY host: key? pinned? any allowed subject present? ---------------------------------
# Each host reports one self-identifying line, so classification never depends on AWX output framing.
probe='key=no; pinned=na
if grep -qF "'"$TG_ACTUATION_PUBKEY"'" /root/.ssh/authorized_keys 2>/dev/null; then
  key=yes
  if grep -F "'"$TG_ACTUATION_PUBKEY"'" /root/.ssh/authorized_keys | grep -q tg-actuator-guard && [ -x /usr/local/sbin/tg-actuator-guard ]; then pinned=yes; else pinned=no; fi
fi
subject=no
for u in '"$unitsvc"'; do systemctl list-unit-files "$u" 2>/dev/null | grep -q "^$u" && subject=yes; done
for c in '"$containers"'; do docker inspect "$c" >/dev/null 2>&1 && subject=yes; done
echo "TGA host=$(hostname) key=$key pinned=$pinned subject=$subject"'
probeid=$(launch_adhoc "$TG_ESTATE_INVENTORY" "$probe")
[ -n "$probeid" ] || { echo "  FORBIDDEN: could not launch the estate probe — the check proved nothing"; exit 1; }
out=$(wait_adhoc "$probeid")

lines=$(printf '%s\n' "$out" | grep -o 'TGA host=[^ ]* key=[a-z]* pinned=[a-z]* subject=[a-z]*')
probed=$(printf '%s\n' "$lines" | grep -c 'TGA host=')
unreachable=$(printf '%s' "$out" | grep -c 'UNREACHABLE!')

# VACUITY FLOOR on the probe. Zero lines = wrong credential, empty inventory, or a broken snippet.
if [ "$probed" -eq 0 ]; then
  echo "  FORBIDDEN: the estate probe returned ZERO host lines. Either the probe is broken (credential,"
  echo "  inventory, snippet) or AWX reaches nothing. Both need a human; neither is PASS."
  exit 1
fi

# ---- 3. classify ------------------------------------------------------------------------------------
ok=0 nosurface=0 fail=0
unpinned_hosts="" missing_hosts="" unproven_hosts=""

# TG-281 direction: an unpinned key ANYWHERE is arbitrary root for a leaked key — allowlisted or not.
unpinned_hosts=$(printf '%s\n' "$lines" | grep ' key=yes pinned=no ' | sed 's/.*host=\([^ ]*\).*/ \1/' | tr -d '\n')

# TG-123 direction: walk the ALLOWLIST — the denominator the first version never had. The trailing
# space in the match anchors the hostname, so wallos01 can never satisfy wallos-mylab01's row.
for g in $allow; do
  l=$(printf '%s\n' "$lines" | grep -F "host=$g " | head -1)
  if [ -z "$l" ]; then
    unproven_hosts="$unproven_hosts $g"
    continue
  fi
  case "$l" in
    *"key=yes pinned=yes"*)          ok=$((ok+1));;
    *"key=yes pinned=no"*)           : ;; # already collected by the TG-281 sweep above
    *"key=no pinned=na subject=yes"*) missing_hosts="$missing_hosts $g";;
    *)                               nosurface=$((nosurface+1));;
  esac
done

echo "  expected population (running allowlist)    : $(echo $allow | wc -w)"
echo "  ...key present AND pinned                  : $ok"
echo "  ...no key, and no actuation surface either : $nosurface (stated, not counted as coverage)"
echo "  hosts probed / AWX-unreachable             : $probed / $unreachable"

if [ -n "$unpinned_hosts" ]; then
  fail=1
  echo "  FORBIDDEN: key accepted WITHOUT the forced command on:$unpinned_hosts"
  echo "  A leaked key runs arbitrary commands as root there — every TG-side gate is bypassed."
fi
if [ -n "$missing_hosts" ]; then
  fail=1
  echo "  FORBIDDEN: allowlisted host(s) WITH an actuation surface and NO key:$missing_hosts"
  echo "  TG would choose an action there, clear every gate, and die on the SSH handshake —"
  echo "  canary bug #4, presenting as a transport failure."
fi
if [ -n "$unproven_hosts" ]; then
  fail=1
  echo "  FORBIDDEN: allowlisted host(s) produced no probe line:$unproven_hosts"
  echo "  Unproven is not proven safe — an actuation target AWX cannot reach cannot be provisioned either."
fi
# VACUITY FLOOR on the conclusion: an allowlist yielding zero key+pinned hosts means either the env
# read broke or SSH-actuation coverage is entirely gone. Both need a human.
if [ "$ok" -eq 0 ]; then
  fail=1
  echo "  FORBIDDEN: ZERO allowlisted hosts are key+pinned — TG cannot SSH-actuate anything."
fi

[ "$fail" -eq 0 ] && echo "actuation-guard coverage: PASS ($ok pinned of $(echo $allow | wc -w) allowlisted; $nosurface no-surface)"
exit $fail
