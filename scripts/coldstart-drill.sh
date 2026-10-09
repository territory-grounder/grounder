#!/usr/bin/env bash
# COLD-START DRILL (TG-428) — the versioned SPEC for the parent-directory resume kit.
#
# WHY THIS FILE IS THE SOURCE OF TRUTH: a fresh Claude session starts in the PARENT directory of this
# repo. What it auto-loads there — the router CLAUDE.md and the .claude/agents symlink — is MACHINE-LOCAL
# and can never be committed: the kit lives outside the repo tree, and the repo is public-mirrored, so
# estate hostnames/IPs/credentials (which the router's ESTATE section carries) must never enter committed
# files. An uncommitted artifact rots silently; this drill is the falsifiable spec that catches the rot,
# and its FAIL messages carry the exact repair instructions because there is nowhere else to version them.
#
# THE KIT (everything below is REQUIRED; $PARENT = the directory containing grounder/ and www/):
#   $PARENT/CLAUDE.md — the router. Non-empty, with four EXACT marker headings:
#     ## ROUTE          — you are in the parent; the product is grounder/, the website www/. Immediately
#                         Read grounder/CLAUDE.md (reading it also satisfies the repo territory gate's
#                         ack — until then Edits into grounder/ are hook-blocked), then follow
#                         grounder/AGENTS.md § "Resume here" and its operating loop. Verify this kit
#                         with `make -C grounder coldstart`. Work from inside grounder/.
#     ## AGENTS         — tg-code-reviewer / tg-eval-runner / tg-spec-navigator are defined in
#                         grounder/.claude/agents/ and register only when the session can SEE them; the
#                         .claude/agents symlink beside the router makes them resolve from the parent.
#     ## NIGHTLY RESET  — a 03:40 cron runs `git checkout main && git pull --ff-only` inside the main
#                         grounder/ tree, then the nightly eval-drift (may hold the gateway lock 1-2h)
#                         and may commit+push eval baseline files. NEVER leave uncommitted work or a
#                         checked-out feature branch in the main grounder/ tree across 03:40 — push
#                         same-session, or do MR work in a worktree under grounder/.claude/worktrees/.
#     ## ESTATE         — where prod lives and how to reach it: the prod+eval box's ssh alias, the
#                         console origin (HTTPS-only Secure cookie), the YouTrack write path (a sourced
#                         env file; the MCP token is read-only), and the session-memory location. The
#                         CONCRETE hostnames/IPs/paths live ONLY in the router itself — never copy them
#                         into this repo (public mirror).
#   $PARENT/.claude/agents — a RELATIVE symlink -> ../grounder/.claude/agents, through which
#                         tg-spec-navigator.md must be readable.
#                         Repair: ln -sfn ../grounder/.claude/agents $PARENT/.claude/agents
#
# PROBES (each prints PASS/FAIL; the final line always carries the denominator):
#   P1 router present + non-empty + all 4 marker headings
#   P2 agent defs resolve through the parent symlink
#   P3 scripts/lint-resume-budget.sh present and green (the resume budget is otherwise unenforced)
#   P4 docs/BOARD.md carries its 4 headings and a TG- id in the 60 lines after 'THE QUEUE'
#   P5 scripts/verify-pipeline.sh present + executable
#   P6 glab authenticated to gitlab.example.net (timeout 15)
#
# EXIT CODES: 0 all green · 1 a probe FAILED · 2 tooling error (bad COLDSTART_ROOT) · 3 COLD-START
# BROKEN (router absent or empty — a fresh session would auto-load NOTHING; deliberately distinct from
# a probe failure, per the absent-input-is-not-a-failure rule).
#
# HOOKS: COLDSTART_ROOT=<dir> points the drill at a FIXTURE parent (the repo tree is then expected at
# $COLDSTART_ROOT/grounder) — scripts/coldstart-drill_test.sh uses this; without it PARENT resolves
# RELATIVE to this script's location (never a literal home path — mirror-safe). COLDSTART_SKIP_GLAB=1
# skips P6 and counts it green — a DRILL-ONLY fixture hook (fixtures must not depend on the box's glab
# auth); the skip is printed beside the verdict, never silent.
#
# LIVE ARM (--live): prints the fixed acceptance prompt it WOULD run — `claude -p` FROM $PARENT, asking
# for TOP=<ticket-id> + NEXT=<sentence>, asserting TOP matches a TG- id present in docs/BOARD.md's queue
# and total token usage <= COLDSTART_TOKEN_BUDGET (default 20000) — and EXECUTES it only when
# COLDSTART_LIVE=1 is ALSO set. Double opt-in on purpose: execution SPENDS REAL TOKENS on the local
# gateway. Without COLDSTART_LIVE=1 the live arm prints and exits 0 having run nothing.
set -uo pipefail

SELF="$(cd "$(dirname "$0")" && pwd)"
if [ -n "${COLDSTART_ROOT:-}" ]; then
  if [ ! -d "$COLDSTART_ROOT" ]; then
    echo "coldstart drill: TOOLING ERROR — COLDSTART_ROOT='$COLDSTART_ROOT' is not a directory" >&2
    exit 2
  fi
  PARENT="$(cd "$COLDSTART_ROOT" && pwd)"
  REPO="$PARENT/grounder"
else
  PARENT="$(cd "$SELF/../.." && pwd)"
  REPO="$(cd "$SELF/.." && pwd)"
fi

# ---------------------------------------------------------------------------------------------------
# LIVE ARM — prints always, executes only under double opt-in (COLDSTART_LIVE=1). Spends real tokens.
# ---------------------------------------------------------------------------------------------------
if [ "${1:-}" = "--live" ]; then
  BUDGET="${COLDSTART_TOKEN_BUDGET:-20000}"
  PROMPT="You are a fresh session starting in this directory. Follow this directory's CLAUDE.md router.
Then reply with exactly two lines and nothing else:
TOP=<the TG- ticket id you would work FIRST, taken from THE QUEUE in grounder/docs/BOARD.md>
NEXT=<one sentence: the first concrete action you would take on it>"
  echo "coldstart --live: the fixed acceptance prompt (would run FROM $PARENT):"
  echo "  claude -p --output-format json '<prompt below>'"
  printf '%s\n' "$PROMPT" | sed 's/^/  | /'
  echo "asserts: TOP matches a TG- id present in the 60 lines after 'THE QUEUE' in $REPO/docs/BOARD.md,"
  echo "         and total token usage (input+output) <= COLDSTART_TOKEN_BUDGET ($BUDGET)."
  if [ "${COLDSTART_LIVE:-0}" != "1" ]; then
    echo "coldstart --live: NOT executed — double opt-in required (also set COLDSTART_LIVE=1)." \
         "This arm spends real tokens on the local gateway."
    exit 0
  fi
  command -v claude >/dev/null 2>&1 || { echo "coldstart --live: TOOLING ERROR — claude CLI not found" >&2; exit 2; }
  command -v jq >/dev/null 2>&1 || { echo "coldstart --live: TOOLING ERROR — jq not found" >&2; exit 2; }
  [ -r "$REPO/docs/BOARD.md" ] || { echo "coldstart --live: TOOLING ERROR — $REPO/docs/BOARD.md unreadable; cannot judge TOP" >&2; exit 2; }
  live_json="$(cd "$PARENT" && claude -p "$PROMPT" --output-format json 2>&1)" || {
    echo "coldstart --live: FAIL — claude -p itself failed: $(printf '%s' "$live_json" | tail -1)"; exit 1; }
  live_text="$(printf '%s' "$live_json" | jq -r '.result // empty')"
  live_tok="$(printf '%s' "$live_json" | jq -r '((.usage.input_tokens // 0) + (.usage.output_tokens // 0))')"
  top="$(printf '%s' "$live_text" | grep -oE 'TOP=TG-[0-9]+' | head -1 | cut -d= -f2)"
  queue_ids="$(awk 'f && NR<=f+60 { print } !f && index($0,"THE QUEUE") { f=NR }' "$REPO/docs/BOARD.md" | grep -oE 'TG-[0-9]+' | sort -u)"
  n_ids="$(printf '%s\n' "$queue_ids" | grep -c . || true)"
  live_fail=0
  if [ -z "$top" ]; then
    echo "coldstart --live: FAIL — no TOP=TG-<id> line in the reply"; live_fail=1
  elif ! printf '%s\n' "$queue_ids" | grep -qxF "$top"; then
    echo "coldstart --live: FAIL — TOP=$top is not among the $n_ids TG- id(s) in BOARD.md's queue window"; live_fail=1
  else
    echo "coldstart --live: TOP=$top is in BOARD.md's queue window ($n_ids candidate id(s))"
  fi
  if [ "$live_tok" -gt "$BUDGET" ]; then
    echo "coldstart --live: FAIL — token usage $live_tok exceeds budget $BUDGET"; live_fail=1
  else
    echo "coldstart --live: token usage $live_tok of budget $BUDGET"
  fi
  exit "$live_fail"
fi

# ---------------------------------------------------------------------------------------------------
# STATIC ARM (default) — six probes, denominator always printed.
# ---------------------------------------------------------------------------------------------------
green=0
failed=0
broken=0

pass() { echo "P$1 PASS — $2"; green=$((green + 1)); }
fail() { echo "P$1 FAIL — $2"; failed=1; }
broke() { echo "P$1 FAIL — $2"; broken=1; }

# P1 — the router itself.
ROUTER="$PARENT/CLAUDE.md"
if [ ! -e "$ROUTER" ]; then
  broke 1 "COLD-START BROKEN: router absent at $ROUTER — a fresh session auto-loads NOTHING here. Repair: create it with headings ## ROUTE ## AGENTS ## NIGHTLY RESET ## ESTATE (see scripts/coldstart-drill.sh header for the content spec)"
elif [ ! -s "$ROUTER" ]; then
  broke 1 "COLD-START BROKEN: router EMPTY (0 bytes) at $ROUTER — a fresh session auto-loads a blank file, which routes nowhere. Repair: fill it with headings ## ROUTE ## AGENTS ## NIGHTLY RESET ## ESTATE (see scripts/coldstart-drill.sh header for the content spec)"
else
  missing=""
  for h in "## ROUTE" "## AGENTS" "## NIGHTLY RESET" "## ESTATE"; do
    grep -qF "$h" "$ROUTER" || missing="$missing '$h'"
  done
  if [ -n "$missing" ]; then
    fail 1 "router at $ROUTER is missing heading(s):$missing — a section a fresh session needs is gone. Repair: restore the exact heading(s); scripts/coldstart-drill.sh's header says what each must contain"
  else
    pass 1 "router present, non-empty, 4 of 4 marker headings ($ROUTER)"
  fi
fi

# P2 — the agent definitions must resolve THROUGH the parent symlink (a dead link registers nothing).
NAV="$PARENT/.claude/agents/tg-spec-navigator.md"
if [ -r "$NAV" ]; then
  pass 2 "agent defs resolve through the symlink ($NAV)"
else
  fail 2 "tg-spec-navigator.md does NOT resolve at $NAV — the tg-* agents will not register from the parent dir. Repair: ln -sfn ../grounder/.claude/agents $PARENT/.claude/agents"
fi

# P3 — the resume-budget gate: adopt its verdict when present; its ABSENCE is itself a failure, because
# an unenforced budget is not a budget.
RB="$REPO/scripts/lint-resume-budget.sh"
if [ -e "$RB" ]; then
  rb_out="$(bash "$RB" 2>&1)"; rb_rc=$?
  if [ "$rb_rc" -eq 0 ]; then
    pass 3 "resume-budget gate green (rc=0): $(printf '%s' "$rb_out" | tail -1)"
  else
    fail 3 "resume-budget gate rc=$rb_rc: $(printf '%s' "$rb_out" | tail -1)"
  fi
else
  fail 3 "resume-budget gate not on this branch yet (TG-428 MR-5) — the budget is unenforced"
fi

# P4 — the board the router routes to must still be the board the loop expects.
BOARD="$REPO/docs/BOARD.md"
if [ ! -r "$BOARD" ]; then
  fail 4 "docs/BOARD.md absent/unreadable at $BOARD — the queue a fresh session resumes from is gone"
else
  b_missing=""
  for h in "Working rules" "Live posture" "THE QUEUE" "Definition of done"; do
    grep -qF "$h" "$BOARD" || b_missing="$b_missing '$h'"
  done
  tg_in_queue="$(awk 'f && NR<=f+60 { print } !f && index($0,"THE QUEUE") { f=NR }' "$BOARD" | grep -cE 'TG-[0-9]+')"
  if [ -n "$b_missing" ]; then
    fail 4 "docs/BOARD.md is missing heading(s):$b_missing (of the 4 required)"
  elif [ "$tg_in_queue" -eq 0 ]; then
    fail 4 "docs/BOARD.md has all 4 headings but NO TG- id in the 60 lines after 'THE QUEUE' — the queue window is empty of tickets"
  else
    pass 4 "docs/BOARD.md: 4 of 4 headings, $tg_in_queue TG- line(s) in the 60 lines after 'THE QUEUE'"
  fi
fi

# P5 — the merge-verification step of the loop must be runnable.
VP="$REPO/scripts/verify-pipeline.sh"
if [ -x "$VP" ]; then
  pass 5 "scripts/verify-pipeline.sh present + executable"
else
  fail 5 "scripts/verify-pipeline.sh missing or not executable at $VP — the merge-verification step of the loop is not runnable"
fi

# P6 — glab auth (the merge/pipeline half of the loop).
if [ "${COLDSTART_SKIP_GLAB:-0}" = "1" ]; then
  pass 6 "glab probe SKIPPED (COLDSTART_SKIP_GLAB=1 — drill-only fixture hook; asserts nothing about glab)"
elif ! command -v glab >/dev/null 2>&1; then
  fail 6 "glab missing — merge/pipeline steps of the loop are not executable"
else
  g_out="$(timeout 15 glab auth status 2>&1)"; g_rc=$?
  if [ "$g_rc" -eq 0 ] && printf '%s' "$g_out" | grep -q 'gitlab\.example\.net'; then
    pass 6 "glab authenticated to gitlab.example.net"
  else
    fail 6 "glab auth status rc=$g_rc (or no gitlab.example.net stanza) — merge/pipeline steps of the loop are not executable"
  fi
fi

echo "coldstart: $green of 6 probes green"
[ "$broken" -ne 0 ] && exit 3
[ "$failed" -ne 0 ] && exit 1
exit 0
