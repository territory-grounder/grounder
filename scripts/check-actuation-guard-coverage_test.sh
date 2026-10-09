#!/usr/bin/env bash
# DRILL for check-actuation-guard-coverage.sh (TG-281 + TG-123).
#
# A coverage check that cannot fail is a coverage claim, not a check. This drives the script against a
# stubbed AWX and asserts it REFUSES every state that matters — above all the one its first version
# could not even represent: an allowlisted host with an actuation surface and NO key, which fell out of
# the old denominator instead of failing it (canary bug #4, PASS 7/7 over 13 keyless hosts).
#
# TG-403 lesson: every assertion runs at top level — no `( ... )` subshells around checks, because a
# fail recorded in a subshell variable dies with the subshell and the drill reports PASS.
set -uo pipefail
cd "$(dirname "$0")/.."
ran=0 bad=0
check() { ran=$((ran+1)); if [ "$2" = "$3" ]; then echo "  ok   $1"; else echo "  FAIL $1 (got rc=$2 want rc=$3)"; bad=$((bad+1)); fi; }

# The stub serves TWO ad-hoc runs: the POST whose body carries "limit" (the plane-env read) gets id 1,
# the inventory-wide probe gets id 2; stdout fetches are routed by the id in the URL.
stub() { # $1 = env-read stdout, $2 = estate-probe stdout
  D=$(mktemp -d)
  cat > "$D/curl" <<SH
#!/bin/sh
body=""; grab=0
for a in "\$@"; do
  [ "\$grab" = 1 ] && { body="\$a"; grab=0; }
  [ "\$a" = "-d" ] && grab=1
  case "\$a" in
    *ad_hoc_commands/1/stdout*) cat "$D/out1"; exit 0;;
    *ad_hoc_commands/2/stdout*) cat "$D/out2"; exit 0;;
    *ad_hoc_commands/1/*) echo '{"id":"1","status":"successful"}'; exit 0;;
    *ad_hoc_commands/2/*) echo '{"id":"2","status":"successful"}'; exit 0;;
  esac
done
case "\$body" in *'"limit"'*) echo '{"id":"1"}';; *) echo '{"id":"2"}';; esac
SH
  chmod +x "$D/curl"
  printf '%s' "$1" > "$D/out1"
  printf '%s' "$2" > "$D/out2"
  echo "$D"
}

run() { D=$(stub "$1" "$2"); PATH="$D:$PATH" AWX_URL=http://x AWX_TOKEN=t TG_ACTUATION_PUBKEY=AAAAKEY \
  TG_ADHOC_POLL_DELAY=0 \
  bash scripts/check-actuation-guard-coverage.sh >/dev/null 2>&1; rc=$?; rm -rf "$D"; return $rc; }

# stub_race: like stub, but the PLANE-ENV run's stdout (id 1) is EMPTY on the first fetch and appears on
# the second — the TG-531 shape: AWX's status turns terminal before the callback receiver lands stdout.
stub_race() { # $1 = env-read stdout (second fetch onward), $2 = estate-probe stdout
  D=$(mktemp -d)
  cat > "$D/curl" <<SH
#!/bin/sh
body=""; grab=0
for a in "\$@"; do
  [ "\$grab" = 1 ] && { body="\$a"; grab=0; }
  [ "\$a" = "-d" ] && grab=1
  case "\$a" in
    *ad_hoc_commands/1/stdout*) if [ -f "$D/fetched1" ]; then cat "$D/out1"; else : > "$D/fetched1"; fi; exit 0;;
    *ad_hoc_commands/2/stdout*) cat "$D/out2"; exit 0;;
    *ad_hoc_commands/1/*) echo '{"id":"1","status":"successful"}'; exit 0;;
    *ad_hoc_commands/2/*) echo '{"id":"2","status":"successful"}'; exit 0;;
  esac
done
case "\$body" in *'"limit"'*) echo '{"id":"1"}';; *) echo '{"id":"2"}';; esac
SH
  chmod +x "$D/curl"
  printf '%s' "$1" > "$D/out1"
  printf '%s' "$2" > "$D/out2"
  echo "$D"
}

ENVOK='TGENV TG_PROXMOX_ALLOWED_GUESTS=g1,g2
TGENV TG_ACTUATION_ALLOWED_UNITS=nginx;nginx.service
TGENV TG_ACTUATION_ALLOWED_CONTAINERS=mealie'

# 1. The consistent estate: g1 has a surface and a pinned key; g2 has neither key nor surface.
run "$ENVOK" 'TGA host=g1 key=yes pinned=yes subject=yes
TGA host=g2 key=no pinned=na subject=no'
check "consistent estate PASSES (pinned where a surface exists, keyless only where none does)" $? 0

# 2. TG-281: a key without the forced command, anywhere.
run "$ENVOK" 'TGA host=g1 key=yes pinned=yes subject=yes
TGA host=g2 key=yes pinned=no subject=no'
check "an unpinned key REFUSES (a leaked key would get arbitrary root there)" $? 1

# 3. TG-123 — the branch the old check could not represent.
run "$ENVOK" 'TGA host=g1 key=yes pinned=yes subject=yes
TGA host=g2 key=no pinned=na subject=yes'
check "allowlisted host with a surface and NO key REFUSES (canary bug #4)" $? 1

# 4. An allowlisted host that produced no probe line is unproven, not passed over.
run "$ENVOK" 'TGA host=g1 key=yes pinned=yes subject=yes'
check "allowlisted host missing from the probe REFUSES (unproven is not proven safe)" $? 1

# 5. The denominator itself failing to load must never be read as an empty-set pass — and it must fail
# FOR THAT REASON. rc alone cannot see this: with the env floor deleted, an empty allowlist still
# exits 1 via the zero-pinned floor, which reads as "coverage gone" when the truth is "population
# unknown" — a diagnosis that sends the operator to the wrong subsystem.
D=$(stub 'no env lines at all' 'TGA host=g1 key=yes pinned=yes subject=yes')
out5=$(PATH="$D:$PATH" AWX_URL=http://x AWX_TOKEN=t TG_ACTUATION_PUBKEY=AAAAKEY \
  bash scripts/check-actuation-guard-coverage.sh 2>&1); rc5=$?; rm -rf "$D"
check "unreadable plane env REFUSES (the expected population is unknown)" $rc5 1
if printf '%s' "$out5" | grep -q "expected population is unknown"; then
  check "and it names the POPULATION as the failure, not a downstream symptom" 0 0
else
  check "and it names the POPULATION as the failure, not a downstream symptom" 1 0
fi

# 6. Zero probe lines: broken snippet, wrong credential, empty inventory.
run "$ENVOK" ''
check "empty probe output REFUSES rather than reporting coverage" $? 1

# 7. Zero key+pinned across the whole allowlist: coverage entirely gone.
run "$ENVOK" 'TGA host=g1 key=no pinned=na subject=no
TGA host=g2 key=no pinned=na subject=no'
check "zero pinned hosts REFUSES (TG can SSH-actuate nothing)" $? 1

# 8. Hostname anchoring: wallos01's line must not satisfy wallos-mylab01's row.
run 'TGENV TG_PROXMOX_ALLOWED_GUESTS=wallos-mylab01
TGENV TG_ACTUATION_ALLOWED_UNITS=nginx
TGENV TG_ACTUATION_ALLOWED_CONTAINERS=' 'TGA host=wallos01 key=yes pinned=yes subject=yes'
check "a prefix hostname does not satisfy a longer allowlist entry" $? 1

# 9. TG-531 — the stdout race: the plane-env probe's status is terminal but its stdout lands one fetch
# late. Before the retry this read as an empty population and REFUSED a healthy plane (live 2026-08-22,
# ad-hoc 36781); with it, the second fetch must satisfy the gate.
D=$(stub_race "$ENVOK" 'TGA host=g1 key=yes pinned=yes subject=yes
TGA host=g2 key=no pinned=na subject=no')
PATH="$D:$PATH" AWX_URL=http://x AWX_TOKEN=t TG_ACTUATION_PUBKEY=AAAAKEY TG_ADHOC_POLL_DELAY=0 \
  bash scripts/check-actuation-guard-coverage.sh >/dev/null 2>&1; rc9=$?; rm -rf "$D"
check "stdout lagging the terminal status is retried, not read as an empty population (TG-531)" $rc9 0

# 10. Stdout STILL empty after every retry must refuse AND name the ad-hoc id on stderr — the failure
# line is the diagnosis, so it must point at the AWX run instead of a bare 'could not read'.
D=$(stub '' 'TGA host=g1 key=yes pinned=yes subject=yes')
out10=$(PATH="$D:$PATH" AWX_URL=http://x AWX_TOKEN=t TG_ACTUATION_PUBKEY=AAAAKEY TG_ADHOC_POLL_DELAY=0 \
  bash scripts/check-actuation-guard-coverage.sh 2>&1); rc10=$?; rm -rf "$D"
check "stdout empty after all retries still REFUSES" $rc10 1
if printf '%s' "$out10" | grep -q "stdout still EMPTY after"; then
  check "and the failure names the ad-hoc id for the next diagnosis" 0 0
else
  check "and the failure names the ad-hoc id for the next diagnosis" 1 0
fi

# VACUITY FLOOR on the drill itself.
if [ "$ran" -lt 12 ]; then echo "  FORBIDDEN: only $ran assertion(s) ran — the drill proved nothing"; exit 1; fi
[ "$bad" -eq 0 ] && { echo "actuation-guard coverage drill: PASS ($ran assertions)"; exit 0; }
echo "actuation-guard coverage drill: FAIL"; exit 1
