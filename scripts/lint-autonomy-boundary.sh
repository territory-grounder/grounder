#!/usr/bin/env bash
# lint-autonomy-boundary.sh — the [R#] tag gate on the board's owner list.
#
# TG-488 (owner-ruled 2026-08-14): every entry on docs/BOARD.md's owner list must cite the
# reserved-boundary clause that makes it the owner's — a tag [R1]..[R7] opening the bullet.
# An untagged entry is a session manufacturing an owner-blocker outside the ratified boundary;
# this gate turns that habit into a red build.
#
# Exit: 0 PASS (including the EMPTY list — the goal state; the denominator prints either way)
#       1 gate failure (untagged entry, or the owner-list section is missing)
#       2 tooling error (board file absent — wrong cwd)
set -euo pipefail
BOARD="${TG_BOUNDARY_BOARD:-docs/BOARD.md}"
HDR='## Owner list — boundary-tagged'
if [ ! -f "$BOARD" ]; then
  echo "lint-autonomy-boundary: TOOLING ERROR — $BOARD does not exist (wrong cwd?)" >&2; exit 2
fi
if ! grep -qF "$HDR" "$BOARD"; then
  echo "lint-autonomy-boundary: FAIL — $BOARD has no '$HDR' section." >&2
  echo "  repair: restore the section (owner-ruled 2026-08-14, TG-488 A3); the list may be empty, but the section must exist." >&2
  exit 1
fi
section=$(awk -v hdr="$HDR" 'index($0, hdr) == 1 {insec=1; next} insec && /^## / {exit} insec {print}' "$BOARD")
entries=$(printf '%s\n' "$section" | grep -E '^- ' || true)
total=$(printf '%s' "$entries" | grep -c '^- ' || true)
if [ "${total:-0}" -eq 0 ]; then
  echo "lint-autonomy-boundary: PASS — 0 owner-list entries of 0 (an empty owner list is the goal state)"
  exit 0
fi
untagged=$(printf '%s\n' "$entries" | grep -Ev '^- \[R[1-7]\]' || true)
if [ -n "$untagged" ]; then
  n=$(printf '%s\n' "$untagged" | grep -c '^- ')
  echo "lint-autonomy-boundary: FAIL — $n of $total owner-list entries carry no [R1]..[R7] clause tag:" >&2
  printf '%s\n' "$untagged" | sed 's/^/    /' >&2
  echo "  repair: cite the reserved clause, or DECIDE the item (TG-488 A1: decide, do, record)." >&2
  exit 1
fi
echo "lint-autonomy-boundary: PASS — $total of $total owner-list entries tagged [R1]..[R7]"
