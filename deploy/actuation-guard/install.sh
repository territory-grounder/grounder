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
# THE COMMAND SHAPES ARE NOT AUTHORED HERE ANY MORE. This loop emitted three shapes — systemctl
# restart|reload and docker restart — while the op-class registry could emit five: `systemctl start`
# (start-service) and `docker start` (start-container) were missing, and BOTH are graduated to AUTO. TG
# could choose a start autonomously, clear all six gates, record the approval, and be refused here with
# exit 42. Rollback was worse: `systemctl stop` / `docker stop` were never written at all, so an action
# could be taken and never undone.
#
# The allowlist is now GENERATED from the registry by tools/guardallow, so a new op-class is authorised by
# the same act that registers it. The operator still owns the SUBJECTS (which units, which containers) —
# that is what makes this guard narrower than the registry, and it is the whole point of having it.
#
#   go run ./tools/guardallow -units "$UNITS" -containers "$CONTAINERS" > tg-guard.allow
#
# Pass the generated file as TG_GUARD_ALLOW_SRC. It is NOT generated here because the target host has no Go
# toolchain, and it is REQUIRED rather than defaulted: silently falling back to a hand-written subset is the
# defect this replaces.
# PATTERN MODE (TG-626, follow-estate): pass TG_GUARD_PATTERN_SRC = the file `go run ./tools/guardallow
# -mode=pattern` prints. The pattern then authorises the registry verbs on any non-denied subject, so the exact
# allowlist may be empty (it is still installed: the guard refuses everything without one).
PATTERN_DST=/etc/tg-actuator-guard.pattern
if [ -n "${TG_GUARD_PATTERN_SRC:-}" ]; then
  [ -s "$TG_GUARD_PATTERN_SRC" ] || { echo "install.sh: $TG_GUARD_PATTERN_SRC is empty" >&2; exit 1; }
  grep -q '^ALLOW ' "$TG_GUARD_PATTERN_SRC" && grep -q '^DENY ' "$TG_GUARD_PATTERN_SRC" || { echo "install.sh: $TG_GUARD_PATTERN_SRC lacks ALLOW/DENY lines — not a guardallow -mode=pattern file" >&2; exit 1; }
  # Never on a stateful host (the leaf refuses every subject there; the guard checks its hostname too).
  deny_re=$(sed -n 's/^DENY //p' "$TG_GUARD_PATTERN_SRC" | head -n 1)
  if hostname | LC_ALL=C grep -Eiq -- "$deny_re"; then echo "install.sh: $(hostname) is a stateful-workload host — refusing pattern mode" >&2; exit 1; fi
  # Atomic: a truncated pattern file must never be the live one.
  install -m 0644 -o root -g root "$TG_GUARD_PATTERN_SRC" "$PATTERN_DST.tmp" && mv -f "$PATTERN_DST.tmp" "$PATTERN_DST"
  : "${TG_GUARD_ALLOW_SRC:=/dev/null}"
fi
: "${TG_GUARD_ALLOW_SRC:?set TG_GUARD_ALLOW_SRC to a file produced by: go run ./tools/guardallow -units \"$UNITS\" -containers \"$CONTAINERS\"}"
if [ -z "${TG_GUARD_PATTERN_SRC:-}" ]; then
  [ -s "$TG_GUARD_ALLOW_SRC" ] || { echo "install.sh: $TG_GUARD_ALLOW_SRC is empty — refusing to install an allowlist that authorises nothing" >&2; exit 1; }
fi
tmp="$(mktemp)"
cat "$TG_GUARD_ALLOW_SRC" > "$tmp"
install -m 0644 -o root -g root "$tmp" "$ALLOW"
rm -f "$tmp"

echo "installed $GUARD_DST + $ALLOW ($(wc -l < "$ALLOW") allowlisted command(s)):"
sed 's/^/    /' "$ALLOW"
echo "NEXT: pin it as the forced command on the tg-actuator key (see README.md), then verify."
