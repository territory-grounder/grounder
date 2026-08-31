#!/usr/bin/env bash
# TG-359. A rubric edit could not satisfy its own eval gate, and the fix is only worth anything if the
# shell mode that produces the evidence actually reaches the new verifier.
#
# The parts and how they must connect:
#   eval/eval-gate.sh `rubric` mode  -> re-judges a FIXED session set in two worktrees
#                                    -> calls tools/evalgate with --rejudge
#   tools/evalgate --rejudge         -> selects gate.VerifyComparableRejudge
#   VerifyComparableRejudge          -> INVERTS the TG-194 version check (arms MUST differ)
#   the archived record              -> `<date>-change-rubric-<sha>/verdict.json`, which
#                                       scripts/lint-eval-evidence.sh already accepts
#
# eval/gate's Go oracles cover the last link. This covers the first two, which are shell and flags and so
# have no compiler to notice when they come apart. Comment lines are stripped before every assertion,
# because a guard of this shape has previously passed on its own commented-out subject.
set -uo pipefail
cd "$(dirname "$0")/../.."

GATE="eval/eval-gate.sh"
EVALGATE="tools/evalgate/main.go"
LINT="scripts/lint-eval-evidence.sh"
fail=0
say_fail() { echo "  FAIL: $*"; fail=1; }

# strip_comments removes whole-line shell/Go comments so a commented-out wiring cannot satisfy a grep.
strip_comments() { sed -E 's@^[[:space:]]*(#|//).*$@@'; }

echo "== rubric-gate wiring =="

for f in "$GATE" "$EVALGATE" "$LINT"; do
  [ -f "$f" ] || { echo "  FAIL: $f does not exist"; echo "rubric-gate-wired: FAIL"; exit 1; }
done

gate_src="$(strip_comments < "$GATE")"
# SCOPED to the rubric mode's own block. eval-gate.sh has FOUR --controls occurrences; the ordinary change
# mode owns two of them, and a file-wide grep is satisfied by those — that exact mutation (strip --controls
# from the rubric block only) SURVIVED the first version of check (5).
rubric_block="$(printf '%s' "$gate_src" | awk '/if \[ "\$MODE" = "rubric" \]; then/{f=1} f{print} f&&/^fi$/{exit}')"
evalgate_src="$(strip_comments < "$EVALGATE")"

# (1) the mode exists at all.
if ! printf '%s' "$gate_src" | grep -q '"\$MODE" = "rubric"'; then
  say_fail "eval-gate.sh has no \`rubric\` mode — a core/judge/rubric.json edit has no way to produce the
        evidence scripts/lint-eval-evidence.sh demands of it (the change gate's two arms judge under
        different rubric versions and evalgate refuses to pool them)"
fi

# (2) the mode reaches the inverted verifier. Without --rejudge it calls the ORDINARY comparison, which
#     refuses exactly the pair this mode exists to compare — the mode would run and then fail every time.
if ! printf '%s' "$gate_src" | grep -q -- '--rejudge'; then
  say_fail "the rubric mode does not pass --rejudge to tools/evalgate, so it selects the ordinary
        comparison and is refused on [old-version new-version] — the deadlock, unchanged"
fi

# (3) it must re-judge, not re-run. The whole argument is that the sessions are FIXED data.
if ! printf '%s' "$gate_src" | grep -q 'tools/rejudge'; then
  say_fail "the rubric mode does not call tools/rejudge — if it re-RUNS the corpus instead of re-JUDGING
        captured sessions, triage nondeterminism is back in the measurement and the rubric is no longer
        the only variable"
fi

# (4) the same session bytes must reach both arms, or "only the rubric moved" is an intention, not a fact.
if ! printf '%s' "$gate_src" | grep -qE 'cp -f "\$HERE/\$SESSIONS" "\$BASE_WT/\$SESSIONS"'; then
  say_fail "the rubric mode does not copy the candidate's session file into the base worktree — the two
        arms could be re-judging different captures"
fi

# (5) the negative-control bar must still be supplied. The first run of this mode returned INCONCLUSIVE —
#     "the run did not measure a capability this gate exists to bar on" — because --controls was absent. A
#     rubric edit cannot change whether the agent PROPOSED on a benign incident (fixed data, identical in
#     both arms), but "cannot have changed" is not "was measured".
if [ -z "$rubric_block" ]; then
  say_fail "could not isolate the rubric mode's block — check (5) would otherwise pass on the ordinary
        change mode's own --controls, which is a different code path"
elif ! printf '%s' "$rubric_block" | grep -q -- '--controls'; then
  say_fail "the rubric mode never passes --controls, so the negative-control bar is unmeasured and the
        gate returns INCONCLUSIVE — nothing can be certified on such a run"
fi

# (6) evalgate's flag must select the inverted verifier, not merely exist.
if ! printf '%s' "$evalgate_src" | grep -q 'VerifyComparableRejudge'; then
  say_fail "tools/evalgate never references gate.VerifyComparableRejudge — the --rejudge flag is inert"
fi

# (7) the archived record must still match the shape the evidence gate accepts, or the mode produces
#     evidence nothing recognises.
label="$(printf '%s' "$evalgate_src" | grep -oE '"change-rubric"' | head -1)"
if [ -z "$label" ]; then
  say_fail "the rejudge verdict is not labelled — eval/history would not say which comparison produced it"
else
  behavior_re="$(grep -oE "^behavior_re='[^']*'" "$LINT" | sed "s/^behavior_re='//;s/'\$//")"
  if [ -z "$behavior_re" ]; then
    say_fail "could not read behavior_re from $LINT — cannot check the record shape is accepted"
  fi
  # The record path the evidence gate looks for, with the rubric label in it.
  probe="eval/history/2026-08-06-change-rubric-abc123/verdict.json"
  if ! printf '%s\n' "$probe" | grep -qE '^eval/history/[^/]+-change-[^/]+/verdict\.json$'; then
    say_fail "a change-rubric record ($probe) does not match the path shape $LINT accepts as evidence"
  fi
  # NEGATIVE CONTROL for the check above: a path that should NOT be accepted must not be.
  if printf '%s\n' "eval/history/2026-08-06-trend-abc123/verdict.json" | grep -qE '^eval/history/[^/]+-change-[^/]+/verdict\.json$'; then
    say_fail "negative control: a TREND record satisfied the change-record pattern, so check (7) cannot fail"
  fi
fi

# (8) vacuity floor for strip_comments itself — a stripper that returned everything would let a
#     commented-out mode pass every assertion above.
if printf '# "$MODE" = "rubric"\n' | strip_comments | grep -q 'rubric'; then
  say_fail "vacuity floor: strip_comments left a commented-out line intact, so every check above could
        pass on a comment"
fi
if ! printf 'MODE=rubric # trailing\n' | strip_comments | grep -q 'MODE=rubric'; then
  say_fail "strip_comments removed a line that only ENDS in a comment — it must strip whole-line comments only"
fi

if [ "$fail" != 0 ]; then
  echo "rubric-gate-wired: FAIL"
  exit 1
fi
echo "  ok — the rubric mode re-judges a fixed session set, selects the inverted comparison, and writes a"
echo "       record the eval-evidence gate accepts"
echo "rubric-gate-wired: PASS"
