#!/usr/bin/env bash
# DRILL FOR THE ORIENTATION-BUDGET GATE (TG-428). A gate nobody drills is a gate nobody knows can fail.
# Same convention as scripts/lint-image-pins_test.sh and scripts/lint-protected-paths_test.sh: every arm
# is proven against mktemp fixtures driven through the gate's env hooks (RESUME_BUDGET_FILES /
# RESUME_BUDGET_CONTRIBUTING), never against repository history — a drill coupled to what the tree
# happens to contain is flaky by construction. check() asserts BOTH the exit code AND a message
# substring, because a gate that fails unreadably gets "fixed" by deletion.
#
# KILLING MUTATIONS (each EXECUTED 2026-08-10 — applied, confirmed present, drill observed RED,
# restored from a cp backup, drill observed green again):
#   1. DELETE the absence check (the `exit 3` RESUME FILE ABSENT block) — "a DELETED resume file
#      refuses with its own exit code" goes RED: the gate falls through to `wc -c < missing`,
#      returns the TOOLING rc 2 instead of 3, and the ABSENT wording is gone.
#   2. SUM ONLY EXISTING FILES (replace the absence block's body with `continue`, the naive
#      implementation) — the same arm goes RED with rc 0: two small survivors are under budget, so a
#      deleted BOARD.md would read as the THINNEST resume path instead of a broken one.
#   3. REMOVE THE DENOMINATOR from the verdict line (`$total of $MAX bytes` -> `$total bytes`) —
#      "an under-budget resume path PASSES stating the byte denominator" goes RED: rc stays 0 but
#      'of 40000 bytes' never appears, which is how a verdict quietly stops being checkable.
#   4. BLIND THE RATCHET (hits=""), — "a planted stale claim ... REFUSES" goes RED with rc 0: the
#      coherence probe reports 0 hits over a file that says 'Multi-tenant by default' on line 3.
set -uo pipefail
cd "$(dirname "$0")/.."
G=scripts/lint-resume-budget.sh
fail=0
ran=0

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# check <name> <want-rc> <files-list> <contrib-file> [<expected-substring-in-output>]
check() {
  local name="$1" want="$2" files="$3" contrib="$4" want_msg="${5:-}"
  local out rc
  out="$(RESUME_BUDGET_FILES="$files" RESUME_BUDGET_CONTRIBUTING="$contrib" bash "$G" 2>&1)"; rc=$?
  ran=$((ran + 1))
  if [ "$rc" -ne "$want" ]; then
    echo "  FAIL: $name — want rc=$want got rc=$rc"
    printf '%s\n' "$out" | sed 's/^/      /'
    fail=1
    return
  fi
  if [ -n "$want_msg" ] && ! printf '%s' "$out" | grep -qF "$want_msg"; then
    echo "  FAIL: $name — rc was right but the output never said '$want_msg'"
    printf '%s\n' "$out" | sed 's/^/      /'
    fail=1
    return
  fi
  echo "  ok: $name (rc=$rc)"
}

# fixture <name> — a resume path of three small files + a clean culture file; echoes the dir
fixture() {
  local d="$TMP/$1"
  mkdir -p "$d"
  printf 'claude: read agents, then the board\n'            > "$d/claude.md"
  printf 'agents: stable orientation, guardrails, resume\n' > "$d/agents.md"
  printf 'board: the one authoritative queue, top-down\n'   > "$d/board.md"
  printf 'contributing: honesty over marketing\n'           > "$d/contributing.md"
  printf '%s' "$d"
}
trio() { printf '%s/claude.md %s/agents.md %s/board.md' "$1" "$1" "$1"; }

echo "== resume-budget gate drill =="

# (a) UNDER BUDGET PASSES — and the verdict states its denominators. A verdict without the budget
#     beside the number is a number nobody can dispute; a scan count the reader cannot see is a scan
#     the reader cannot audit (four one-day instances of that class: a-check-that-cannot-report-
#     nothing-to-check).
u=$(fixture under)
check "an under-budget resume path PASSES stating the byte denominator" 0 "$(trio "$u")" "$u/contributing.md" \
  "of 40000 bytes"
check "the PASS verdict carries the file-count denominator" 0 "$(trio "$u")" "$u/contributing.md" \
  "across 3 of 3 files"
check "the coherence ratchet states its scan-set denominator" 0 "$(trio "$u")" "$u/contributing.md" \
  "coherence ratchet: 0 hits in 4 files scanned"

# (b) OVER BUDGET REFUSES, naming the offending file and BOTH numbers. 50,000 bytes exactly, alone in
#     the list, so the expected verdict numbers are deterministic.
o=$(fixture over)
head -c 50000 /dev/zero | tr '\0' 'x' > "$o/big.md"
check "an oversized resume surface REFUSES with both numbers" 1 "$o/big.md" "$o/contributing.md" \
  "50000 of 40000 bytes"
check "the over-budget refusal NAMES the offending file" 1 "$o/big.md" "$o/contributing.md" \
  "big.md (50000 bytes)"

# (c) A DELETED FILE IS A BROKEN RESUME PATH, not a thin one: its own exit code (3), its own wording,
#     and NEVER a smaller total. This is the arm mutations 1 and 2 exist for.
a=$(fixture absent)
rm "$a/board.md"
check "a DELETED resume file refuses with its own exit code" 3 "$(trio "$a")" "$a/contributing.md" \
  "RESUME FILE ABSENT"
check "the absence refusal states the doctrine (no 0-byte pass)" 3 "$(trio "$a")" "$a/contributing.md" \
  "absence is not a 0-byte pass"

# (d) PRESENT-BUT-EMPTY is the same defect wearing a file entry: exit 3, its own wording.
e=$(fixture empty)
: > "$e/board.md"
check "a 0-byte resume file refuses as EMPTY, not as cheap" 3 "$(trio "$e")" "$e/contributing.md" \
  "EMPTY"

# (e) THE COHERENCE RATCHET: a planted retired claim reds the gate, naming file:line so the fix is a
#     one-jump edit. Planted on line 3 exactly.
p=$(fixture planted)
printf 'a clean line\nanother clean line\nTG is Multi-tenant by default, says this stale file\n' \
  > "$p/contributing.md"
check "a planted 'Multi-tenant by default' REFUSES naming file:line" 1 "$(trio "$p")" "$p/contributing.md" \
  "contributing.md:3"

# (e2) …and the SECOND retired claim trips it too, in a RESUME file this time — the scan set is the
#      resume path + CONTRIBUTING.md, not CONTRIBUTING.md alone.
p2=$(fixture planted2)
printf 'the queue\nmutation stays globally disabled\n' > "$p2/board.md"
check "a planted 'mutation stays globally disabled' in a resume file REFUSES" 1 "$(trio "$p2")" "$p2/contributing.md" \
  "board.md:2"

# (f) An ABSENT ratchet file refuses too: "0 hits" over a file that is not there is a claim about
#     nothing (the vacuous-PASS class that closed TG-159 while busybox ran unpinned).
check "an ABSENT ratchet file refuses instead of counting 0 hits" 3 "$(trio "$u")" "$TMP/nowhere/contributing.md" \
  "RATCHET FILE ABSENT"

# The drill's own vacuity floor: if the fixtures stopped being built, every check above could vanish
# and this script would still exit 0 with a cheerful banner.
if [ "$ran" -lt 10 ]; then
  echo "resume-budget gate drill: FAIL — only $ran assertion(s) ran; the drill itself is vacuous"
  exit 1
fi

if [ "$fail" -eq 0 ]; then
  echo "resume-budget gate drill: PASS ($ran assertions)"
else
  echo "resume-budget gate drill: FAIL"
fi
exit "$fail"
