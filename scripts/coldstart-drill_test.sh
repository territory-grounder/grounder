#!/usr/bin/env bash
# DRILL FOR THE COLD-START GATE (TG-428). The parent-dir resume kit is machine-local and uncommitted
# (public mirror; estate coordinates), so the ONLY thing standing between a healthy kit and a fresh
# session that auto-loads nothing is scripts/coldstart-drill.sh — and a gate nobody drills is a gate
# nobody knows can fail. Every arm here is proven against mktemp fixture parents via COLDSTART_ROOT,
# same convention as scripts/lint-image-pins_test.sh: check() asserts exit code AND message substring,
# and a vacuity floor keeps the drill from passing with zero assertions run.
#
# KILLING MUTATIONS (executed 2026-08-10, each applied to scripts/coldstart-drill.sh, shown RED here,
# then restored from a cp backup):
#   1. P1's absence branch made green (broke -> pass) ⇒ "router MISSING" arm goes RED
#      (want rc=3 got rc=0 — the drill would call a kit-less box healthy). Restore ⇒ green.
#   2. The per-heading loop deleted ⇒ "router missing '## NIGHTLY RESET'" arm goes RED
#      (want rc=1 got rc=0 — a router with sections torn out would still pass). Restore ⇒ green.
#   3. The `-s` empty-file branch deleted ⇒ "router EMPTY (0 bytes)" arm goes RED
#      (want rc=3 got rc=1 — a truncated router would be misfiled as a mere heading failure,
#      hiding that a fresh session auto-loads a blank file). Restore ⇒ green.
set -uo pipefail
cd "$(dirname "$0")/.."
G=scripts/coldstart-drill.sh
fail=0
ran=0

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# check <name> <want-rc> <fixture-parent> [<expected-substring-in-output>] [<extra-arg>]
check() {
  local name="$1" want="$2" root="$3" want_msg="${4:-}" arg="${5:-}"
  local out rc
  out="$(COLDSTART_ROOT="$root" COLDSTART_SKIP_GLAB=1 bash "$G" $arg 2>&1)"; rc=$?
  ran=$((ran + 1))
  if [ "$rc" -ne "$want" ]; then
    echo "  FAIL: $name — want rc=$want got rc=$rc"
    printf '%s\n' "$out" | sed 's/^/      /'
    fail=1
    return
  fi
  # The message is part of the contract: the kit is uncommitted, so the FAIL text IS the repair manual.
  if [ -n "$want_msg" ] && ! printf '%s' "$out" | grep -qF "$want_msg"; then
    echo "  FAIL: $name — rc was right but the output never said '$want_msg'"
    printf '%s\n' "$out" | sed 's/^/      /'
    fail=1
    return
  fi
  echo "  ok: $name (rc=$rc)"
}

# mk_kit <dir> — builds a FULL healthy fixture parent: router with all 4 headings, live agents symlink,
# and a fixture repo tree (BOARD with the 4 headings + a TG-000 queue line, stub gates) so P1-P5 are
# green and P6 is skipped via the drill-only COLDSTART_SKIP_GLAB hook. Arms then break ONE thing each.
mk_kit() {
  local p="$1"
  mkdir -p "$p/.claude" "$p/grounder/.claude/agents" "$p/grounder/scripts" "$p/grounder/docs"
  printf '# router\n\n## ROUTE\ngo to grounder/\n\n## AGENTS\nsymlinked\n\n## NIGHTLY RESET\n03:40 cron pulls main\n\n## ESTATE\nmachine-local\n' > "$p/CLAUDE.md"
  printf '# tg-spec-navigator fixture\n' > "$p/grounder/.claude/agents/tg-spec-navigator.md"
  ln -s ../grounder/.claude/agents "$p/.claude/agents"
  printf '# BOARD\n\n## Working rules\n\n## Live posture\n\n# THE QUEUE\n\n| 1 | TG-000 | fixture ticket |\n\n## Definition of done\n' > "$p/grounder/docs/BOARD.md"
  printf '#!/usr/bin/env bash\nexit 0\n' > "$p/grounder/scripts/lint-resume-budget.sh"
  printf '#!/usr/bin/env bash\nexit 0\n' > "$p/grounder/scripts/verify-pipeline.sh"
  chmod +x "$p/grounder/scripts/verify-pipeline.sh"
}

echo "== cold-start drill tests =="

# (a) THE HEALTHY KIT PASSES with the full denominator. Without this arm the drill could be satisfied
#     by a gate that refuses everything.
mk_kit "$TMP/healthy"
check "a full healthy kit passes 6 of 6" 0 "$TMP/healthy" "coldstart: 6 of 6 probes green"

# (b) Router MISSING is the broken-not-failed state: its own exit code 3 and the repair text, because
#     "the kit is absent" and "a probe failed" demand different responses.
mk_kit "$TMP/norouter"; rm "$TMP/norouter/CLAUDE.md"
check "router MISSING is BROKEN (rc 3) and names the repair" 3 "$TMP/norouter" "router absent"

# (c) ONE heading torn out ⇒ ordinary failure (rc 1) NAMING the missing heading — the fix is surgical,
#     not a rebuild.
mk_kit "$TMP/noheading"
printf '# router\n\n## ROUTE\ngo\n\n## AGENTS\nsym\n\n## ESTATE\nlocal\n' > "$TMP/noheading/CLAUDE.md"
check "router missing '## NIGHTLY RESET' fails rc 1 naming it" 1 "$TMP/noheading" "## NIGHTLY RESET"

# (d) Router EMPTY (0 bytes) is BROKEN too, with its OWN message — a blank auto-load routes nowhere,
#     and must not be misfiled as a heading problem.
mk_kit "$TMP/emptyrouter"; : > "$TMP/emptyrouter/CLAUDE.md"
check "router EMPTY (0 bytes) is BROKEN (rc 3) with the EMPTY message" 3 "$TMP/emptyrouter" "EMPTY"

# (e) A DEAD symlink: the path exists but resolves to nothing, so the agents silently never register.
#     The FAIL must carry the exact ln -sfn repair.
mk_kit "$TMP/deadlink"; rm -rf "$TMP/deadlink/grounder/.claude/agents"
check "a dead agents symlink fails rc 1 with the ln -sfn repair" 1 "$TMP/deadlink" "ln -sfn ../grounder/.claude/agents"

# (f) A bogus COLDSTART_ROOT is a TOOLING error (rc 2), never a verdict about the kit.
check "a nonexistent COLDSTART_ROOT is a tooling error (rc 2)" 2 "$TMP/does-not-exist" "TOOLING ERROR"

# (g) The live arm without COLDSTART_LIVE=1 prints the prompt and runs NOTHING (rc 0) — the double
#     opt-in defaults closed, so no fixture run can ever spend tokens by accident.
check "--live without COLDSTART_LIVE=1 prints but does not execute" 0 "$TMP/healthy" "NOT executed" "--live"

# Vacuity floor: if the fixtures stopped being built, every check above could vanish and this script
# would still exit 0 with a cheerful banner.
if [ "$ran" -lt 7 ]; then
  echo "cold-start drill tests: FAIL — only $ran assertion(s) ran; the drill itself is vacuous"
  exit 1
fi

if [ "$fail" -eq 0 ]; then
  echo "cold-start drill tests: PASS ($ran assertions)"
else
  echo "cold-start drill tests: FAIL"
fi
exit "$fail"
