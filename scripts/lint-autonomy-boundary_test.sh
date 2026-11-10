#!/usr/bin/env bash
# lint-autonomy-boundary_test.sh — deterministic drills for the [R#] tag gate. Fixtures only;
# no git state needed. Includes the EMPTY-input drill (TG-365: mutate toward emptiness).
set -u
pass=0; fail=0
t() { # name, expected_rc, board_content ('-' = no file at all)
  local name="$1" want="$2" content="$3" d rc out
  d=$(mktemp -d)
  if [ "$content" != "-" ]; then printf '%s\n' "$content" > "$d/BOARD.md"; fi
  out=$(TG_BOUNDARY_BOARD="$d/BOARD.md" bash scripts/lint-autonomy-boundary.sh 2>&1); rc=$?
  if [ "$rc" -eq "$want" ]; then echo "  ok: $name (rc=$rc)"; pass=$((pass+1))
  else echo "  FAIL: $name — want rc $want got $rc"; printf '%s\n' "$out" | sed 's/^/    /'; fail=$((fail+1)); fi
  rm -rf "$d"
}
echo "== autonomy-boundary gate drills =="
H='## Owner list — boundary-tagged'
t "healthy: every entry tagged" 0 "$H
- [R3] fund the thing
- [R2] wide partition scenario
## Next section"
t "one untagged entry is RED" 1 "$H
- [R3] fund the thing
- reopen the debate someday
## Next section"
t "missing owner-list section is RED with the repair named" 1 "# Board
## Something else entirely
- [R3] a tag outside the section does not count"
t "EMPTY list PASSES (goal state)" 0 "$H

## Next section"
t "missing board file is a tooling error" 2 "-"
d=$(mktemp -d); printf '%s\n' "$H

## Next" > "$d/BOARD.md"
out=$(TG_BOUNDARY_BOARD="$d/BOARD.md" bash scripts/lint-autonomy-boundary.sh); rm -rf "$d"
case "$out" in
  *"0 owner-list entries of 0"*) echo "  ok: the empty case prints its denominator"; pass=$((pass+1));;
  *) echo "  FAIL: the empty case does not print its denominator"; fail=$((fail+1));;
esac
echo "autonomy-boundary drills: $pass ok, $fail failed"
[ "$fail" -eq 0 ]
