#!/bin/sh
# Install tg-actuator-guard on an actuation TARGET host and generate its allowlist from the operator's
# allowed units/containers. Run AS ROOT on each target (or via AWX/ansible). Idempotent. It does NOT touch
# authorized_keys — that edit is deliberate and separate (see README.md § pin the forced command), so a bug
# here can never corrupt a host's key file.
#
# Usage:  UNITS="nginx nginx.service" CONTAINERS="" sh install.sh
set -eu
GUARD_SRC="${GUARD_SRC:-$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/tg-actuator-guard}"
GUARD_DST=/usr/local/sbin/tg-actuator-guard
ALLOW=/etc/tg-actuator-guard.allow
UNITS="${UNITS:-}"
CONTAINERS="${CONTAINERS:-}"

[ -r "$GUARD_SRC" ] || { echo "install: guard source $GUARD_SRC not found" >&2; exit 1; }
install -m 0755 -o root -g root "$GUARD_SRC" "$GUARD_DST"

# The allowlist holds the EXACT SSH_ORIGINAL_COMMAND strings TG sends: each argv element single-quoted
# (systemctl restart|reload <unit>, docker restart <container>), one per line. Nothing else can pass the guard.
tmp="$(mktemp)"
for u in $UNITS; do
  printf "'systemctl' 'restart' '%s'\n" "$u" >> "$tmp"
  printf "'systemctl' 'reload' '%s'\n" "$u" >> "$tmp"
done
for c in $CONTAINERS; do
  printf "'docker' 'restart' '%s'\n" "$c" >> "$tmp"
done
install -m 0644 -o root -g root "$tmp" "$ALLOW"
rm -f "$tmp"

echo "installed $GUARD_DST + $ALLOW ($(wc -l < "$ALLOW") allowlisted command(s)):"
sed 's/^/    /' "$ALLOW"
echo "NEXT: pin it as the forced command on the tg-actuator key (see README.md), then verify."
