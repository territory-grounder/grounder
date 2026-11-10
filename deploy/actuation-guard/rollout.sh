#!/bin/sh
# rollout.sh — install tg-actuator-guard in PATTERN MODE and pin the TG actuator key on many hosts (TG-626).
#
# WHY. Measured 2026-11-06: of 195 estate hosts exactly ONE carried the actuator key pinned to the guard, so a
# service/container heal had an execution path on one host. AWX could not reach 111 of them (TG-557), so this
# runs over plain SSH with an operator admin key and is idempotent; AWX can import the same remote script.
#
# WHAT IT DOES PER HOST (as root, over the admin key — never over the actuator key):
#   1. installs /usr/local/sbin/tg-actuator-guard and /etc/tg-actuator-guard.pattern (from guardallow
#      -mode=pattern); creates an EMPTY /etc/tg-actuator-guard.allow only if none exists (an existing exact
#      list is kept: the guard consults it first);
#   2. backs up /root/.ssh/authorized_keys, removes ONLY lines carrying the actuator key's base64 blob, and
#      appends it pinned:  restrict,command="/usr/local/sbin/tg-actuator-guard" <actuator.pub>
#      Every other key (the operator's own admin key above all) is left byte-identical;
#   3. self-checks the guard on the host: an arbitrary command must exit 42.
#
# Usage:
#   HOSTS_FILE=hosts.txt ACTUATOR_PUB=actuator.pub PATTERN_FILE=guard.pattern ADMIN_KEY=~/.ssh/admin \
#     [PARALLEL=8] [DRY_RUN=1] sh deploy/actuation-guard/rollout.sh
# Hosts: one per line (short names as TG dials them). Output: one "OK <host>" / "FAIL <host> <why>" per host.
# Host names are deployment data and are passed in — never committed (the repo is public-mirrored).
set -eu
here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
: "${HOSTS_FILE:?set HOSTS_FILE}" "${ACTUATOR_PUB:?set ACTUATOR_PUB}" "${PATTERN_FILE:?set PATTERN_FILE}" "${ADMIN_KEY:?set ADMIN_KEY}"
PARALLEL="${PARALLEL:-8}"
grep -q '^ALLOW ' "$PATTERN_FILE" && grep -q '^DENY ' "$PATTERN_FILE" || { echo "rollout: $PATTERN_FILE is not a guardallow -mode=pattern file" >&2; exit 1; }
set -- $(cat "$ACTUATOR_PUB")
case "${1:-}" in ssh-ed25519|ssh-rsa|ecdsa-*) ;; *) echo "rollout: $ACTUATOR_PUB is not a public key line" >&2; exit 1 ;; esac

bundle=$(mktemp -d)
trap 'rm -rf "$bundle"' EXIT
cp "$here/tg-actuator-guard" "$bundle/guard"
cp "$PATTERN_FILE" "$bundle/pattern"
cp "$ACTUATOR_PUB" "$bundle/actuator.pub"
cat > "$bundle/remote.sh" <<'REMOTE'
set -eu
d=$(dirname "$0")
deny_re=$(sed -n 's/^DENY //p' "$d/pattern" | head -n 1)
if hostname | LC_ALL=C grep -Eiq -- "$deny_re"; then echo "stateful-workload host — pattern mode refused"; exit 4; fi
# Atomic installs: a half-copied guard or pattern file must never be the live one.
install -m 0755 -o root -g root "$d/guard" /usr/local/sbin/tg-actuator-guard.tmp && mv -f /usr/local/sbin/tg-actuator-guard.tmp /usr/local/sbin/tg-actuator-guard
install -m 0644 -o root -g root "$d/pattern" /etc/tg-actuator-guard.pattern.tmp && mv -f /etc/tg-actuator-guard.pattern.tmp /etc/tg-actuator-guard.pattern
[ -e /etc/tg-actuator-guard.allow ] || install -m 0644 -o root -g root /dev/null /etc/tg-actuator-guard.allow
mkdir -p /root/.ssh && chmod 700 /root/.ssh
ak=/root/.ssh/authorized_keys
touch "$ak"
cp -p "$ak" "$ak.bak-tg626-$(date +%s)"
blob=$(awk '{print $2}' "$d/actuator.pub")
{ grep -v -F -- "$blob" "$ak" || true; printf 'restrict,command="/usr/local/sbin/tg-actuator-guard" %s\n' "$(cat "$d/actuator.pub")"; } > "$ak.tg626.new"
chmod 600 "$ak.tg626.new" && mv "$ak.tg626.new" "$ak"
rc=0; SSH_ORIGINAL_COMMAND="id" /usr/local/sbin/tg-actuator-guard >/dev/null 2>&1 || rc=$?
[ "$rc" = 42 ] || { echo "guard self-check: arbitrary command exited $rc, want 42"; exit 3; }
REMOTE

one() {
  h=$1
  if [ -n "${DRY_RUN:-}" ]; then echo "DRY $h"; return 0; fi
  if out=$(tar -C "$bundle" -cf - guard pattern actuator.pub remote.sh | ssh -i "$ADMIN_KEY" -o BatchMode=yes \
      -o ConnectTimeout=8 root@"$h" 'd=$(mktemp -d) && tar -xf - -C "$d" && sh "$d/remote.sh"; rc=$?; rm -rf "$d"; exit $rc' 2>&1); then
    echo "OK $h"
  else
    echo "FAIL $h $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200)"
  fi
}
# Bounded fan-out, portable /bin/sh: launch up to PARALLEL hosts, then wait for the batch.
n=0
grep -v '^[[:space:]]*\(#\|$\)' "$HOSTS_FILE" | {
  while read -r h; do
    one "$h" &
    n=$((n + 1))
    if [ "$n" -ge "$PARALLEL" ]; then wait; n=0; fi
  done
  wait
}
