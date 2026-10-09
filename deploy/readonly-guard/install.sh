#!/bin/sh
# install.sh — put the READ-ONLY guard and its allowlist on one host TG reads (TG-280).
#
# Run ON THE TARGET HOST, as root. Idempotent.
#
# WHAT THIS BUYS. Before it, TG read every estate host as root with the UNRESTRICTED estate key. After
# it, the diagnostic key can run exactly TG's twelve read-only commands (plus any bounded log tails the
# operator enabled) and nothing else — enforced by the host's own sshd, below and independent of every
# TG application-layer gate. A worker compromise that steals the key gets a read-only view, not root.
#
#   ./install.sh --allow /path/to/generated.allow --pubkey /path/to/tg-hostdiag.pub
#
# Generate the allowlist on a machine with the repo (NEVER hand-author it — see tools/readallow):
#   go run ./tools/readallow -log-paths "/var/log/syslog" > generated.allow
set -eu

GUARD_DST=/usr/local/sbin/tg-readonly-guard
ALLOW_DST=/etc/tg-readonly-guard.allow
AUTHKEYS=/root/.ssh/authorized_keys
ALLOW_SRC=""; PUBKEY=""
SELF_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

while [ $# -gt 0 ]; do
  case "$1" in
    --allow) ALLOW_SRC="$2"; shift 2 ;;
    --pubkey) PUBKEY="$2"; shift 2 ;;
    *) echo "install.sh: unknown argument $1" >&2; exit 2 ;;
  esac
done

[ -n "$ALLOW_SRC" ] || { echo "install.sh: --allow is required" >&2; exit 2; }
[ -r "$ALLOW_SRC" ] || { echo "install.sh: cannot read $ALLOW_SRC" >&2; exit 2; }

# VACUITY FLOOR. An allowlist with no command lines installs cleanly and denies every read — and this
# lane reports a denied read as an unreachable host, so the estate would look quiet rather than blind.
# Refuse here, where a human is watching, rather than on 38 hosts where nobody is.
CMDS=$(grep -cv '^#' "$ALLOW_SRC" || true)
[ "${CMDS:-0}" -ge 1 ] || {
  echo "install.sh: $ALLOW_SRC holds no command lines — installing it would silently blind TG on this host" >&2
  exit 1
}

install -m 0755 "$SELF_DIR/tg-readonly-guard" "$GUARD_DST"
install -m 0644 "$ALLOW_SRC" "$ALLOW_DST"
echo "installed $GUARD_DST and $ALLOW_DST ($CMDS command line(s))"

if [ -n "$PUBKEY" ]; then
  [ -r "$PUBKEY" ] || { echo "install.sh: cannot read $PUBKEY" >&2; exit 2; }
  KEY=$(cat "$PUBKEY")
  # The options are the other half of the control: restrict turns OFF pty, agent/port/X11 forwarding and
  # user-rc, and command= pins every session to the guard regardless of what the client asks for.
  OPTS="restrict,command=\"$GUARD_DST\""
  mkdir -p "$(dirname "$AUTHKEYS")"; touch "$AUTHKEYS"; chmod 600 "$AUTHKEYS"
  FPR=$(printf '%s\n' "$KEY" | awk '{print $2}')
  # Drop any prior entry for this key (restricted or not), then add the restricted one. Doing it in this
  # order is what upgrades a host that already trusts the key WITHOUT a window where it trusts neither.
  TMP=$(mktemp); grep -vF -- "$FPR" "$AUTHKEYS" > "$TMP" || true
  printf '%s %s\n' "$OPTS" "$KEY" >> "$TMP"
  install -m 0600 "$TMP" "$AUTHKEYS"; rm -f "$TMP"
  echo "pinned the diagnostic key in $AUTHKEYS with: $OPTS"
fi
