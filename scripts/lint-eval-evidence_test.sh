#!/usr/bin/env bash
# DRILL FOR THE EVAL-EVIDENCE GATE (TG-237). A gate nobody drills is a gate nobody knows can fail — this
# repo shipped a protected-path gate that passed VACUOUSLY on every main pipeline for weeks, and the lesson
# taken was that each gate carries a test proving each of its arms both PASSES and REFUSES.
set -uo pipefail
cd "$(dirname "$0")/.."
G=scripts/lint-eval-evidence.sh
fail=0

check() { # name, want_rc, env-prefixed command
  local name="$1" want="$2"; shift 2
  local out rc
  out="$("$@" 2>&1)"; rc=$?
  if [ "$rc" -eq "$want" ]; then
    echo "  ok: $name (rc=$rc)"
  else
    echo "  FAIL: $name — want rc=$want got rc=$rc"
    printf '%s\n' "$out" | sed 's/^/      /'
    fail=1
  fi
}

echo "== eval-evidence gate drill =="

check "a change touching no behavior path passes" 0 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_FILES=$'core/db/x.go\ndocs/BOARD.md' bash "$G"

check "a behavior change with NO evidence REFUSES" 1 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_FILES=$'agent/loop.go' EVAL_EVIDENCE_TRAILERS='' bash "$G"

# Owner-ruled 2026-08-14: the inert prose library is CONTENT, not behavior — nothing under skills/ is
# loaded from the repo tree, so a distillation batch must merge without evidence or waiver. The arm that
# proves the gate still bites sits directly above (agent/ with no evidence refuses).
check "the inert prose library (skills/) is NOT a behavior path" 0 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_FILES=$'skills/skill/drift-check/SKILL.md\ntools/prosedistill/manifest.json' EVAL_EVIDENCE_TRAILERS='' bash "$G"

# Owner-ruled 2026-08-14 (TG-488 B9), the same principle's other direction: the machinery that SHAPES
# what reaches the model IS behavior. The TG-476 leak proved it — a composer change altered every
# production session's guidance from outside the old set.
check "the seed composer (compose_seed.go) IS a behavior path" 1 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_FILES=$'temporal/runner/compose_seed.go' EVAL_EVIDENCE_TRAILERS='' bash "$G"

check "the skill store's law (core/skillstore/) IS a behavior path" 1 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_FILES=$'core/skillstore/validate.go' EVAL_EVIDENCE_TRAILERS='' bash "$G"

# The op-class CATALOG is rendered into every preamble (opschema.Catalog() -> agent/loop.go), and the registry's
# own doc records a measured judged-quality drop from a DATA-only class addition. A change to it is a behavior
# change; a sibling Go file in the same package is NOT (it is machinery gated elsewhere, not rendered content).
check "the op-class catalog (opschema.json) IS a behavior path" 1 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_FILES=$'core/actuate/opschema/opschema.json' EVAL_EVIDENCE_TRAILERS='' bash "$G"

check "a non-rendered file in the same package is NOT a behavior path" 0 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_FILES=$'core/actuate/opschema/overlay.go' bash "$G"

check "the rubric is a behavior path" 1 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_FILES=$'core/judge/rubric.json' EVAL_EVIDENCE_TRAILERS='' bash "$G"

check "a behavior change WITH a passing on-box record passes" 0 \
  env EVAL_EVIDENCE_BASE=HEAD \
      EVAL_EVIDENCE_FILES=$'agent/loop.go\neval/history/2026-08-01-change-31b177ecaf84/verdict.json' bash "$G"

# THE ARM THAT WAS NEVER DRILLED, AND SO WAS BROKEN FOR AS LONG AS IT EXISTED (TG-409 family).
#
# The gate's REFUSED branch says "a regression may not merge on its own evidence". Until 2026-08-07 it was
# very nearly unreachable: the check grepped the whole verdict file for '"pass": true', and every verdict
# carries one such field PER SCORED DIMENSION plus the top-level result. So a record whose overall outcome
# was "fail" satisfied the gate on the strength of any single dimension that happened to pass.
#
# Caught on a real record already committed to a branch — eval/history/2026-08-05-change-10c4f7a606cd:
# "outcome": "fail", "pass": false, and FIVE '"pass": true' dimension entries. The gate printed "(pass)".
#
# The fixture below is written rather than borrowed from eval/history, because the only records that
# reproduce the shape are ones this very gate would refuse to let anyone commit.
FAILREC=eval/history/9999-01-01-change-drillfixture
mkdir -p "$FAILREC"
cat > "$FAILREC/verdict.json" <<'JSON'
{
  "runs": 1,
  "dims": [
    { "dim": "appropriate_band", "delta": -0.5, "pass": false },
    { "dim": "correct_diagnosis", "delta": 0.1, "pass": true },
    { "dim": "evidence_grounded", "delta": 0.1, "pass": true }
  ],
  "overall_pass": false,
  "pass": false,
  "outcome": "fail"
}
JSON
trap 'rm -rf "$FAILREC"' EXIT

check "a FAILING record REFUSES even though its dimensions pass" 1 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_TRAILERS='' \
      EVAL_EVIDENCE_FILES=$'agent/loop.go\n'"$FAILREC/verdict.json" bash "$G"

# TG-500 (Law-Change-Approved-By @ncpjfuzl citing TG-488): a QUALIFIED-INCONCLUSIVE record is valid evidence.
# band-replaces-floor resolves a drop the run cannot distinguish from its own measured judge-noise to
# UNMEASURED -> INCONCLUSIVE (escalate), which is NOT a regression. The gate must ACCEPT it — while the FAILING
# record above still REFUSES, so this widens the gate by qualified-INCONCLUSIVE and nothing else. The
# top-level "outcome" is "inconclusive" and "pass" is false; the dimensions carry their own "pass" fields,
# so this also re-proves the 2-space anchor (a per-dimension pass:true must not spoof the top-level result).
INCREC=eval/history/9999-01-02-change-incfixture
mkdir -p "$INCREC"
cat > "$INCREC/verdict.json" <<'JSON'
{
  "runs": 1,
  "dims": [
    { "dim": "appropriate_band", "delta": -0.3, "pass": true, "unresolved": true },
    { "dim": "correct_diagnosis", "delta": 0.1, "pass": true }
  ],
  "overall_pass": true,
  "pass": false,
  "outcome": "inconclusive",
  "unmeasured": [ "appropriate_band at this measurement power: escalate to the pooled full gate (make eval-gate-full)" ]
}
JSON
trap 'rm -rf "$FAILREC" "$INCREC"' EXIT

check "a QUALIFIED-INCONCLUSIVE record is ACCEPTED (under-powered, not a regression)" 0 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_TRAILERS='' \
      EVAL_EVIDENCE_FILES=$'agent/loop.go\n'"$INCREC/verdict.json" bash "$G"

check "a named CODEOWNERS waiver passes" 0 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_FILES=$'agent/loop.go' EVAL_EVIDENCE_TRAILERS='@ncpjfuzl' bash "$G"

check "a waiver from a NON-owner still REFUSES" 1 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_FILES=$'agent/loop.go' EVAL_EVIDENCE_TRAILERS='@not-an-owner' bash "$G"

# The fail-closed arm: in CI with no resolvable base, the gate must REFUSE rather than print a green tick
# that proves nothing. This is the exact defect the protected-path gate shipped with.
check "CI with no resolvable base fails closed" 1 \
  env CI=1 EVAL_EVIDENCE_BASE= CI_MERGE_REQUEST_DIFF_BASE_SHA= CI_COMMIT_BEFORE_SHA=0000000000000000000000000000000000000000 bash "$G"

check "a scheduled pipeline skips (no change to gate) instead of failing closed" 0 \
  env CI=1 CI_PIPELINE_SOURCE=schedule EVAL_EVIDENCE_BASE= CI_MERGE_REQUEST_DIFF_BASE_SHA= CI_COMMIT_BEFORE_SHA=0000000000000000000000000000000000000000 bash "$G"

if [ "$fail" -eq 0 ]; then
  echo "eval-evidence gate drill: PASS"
else
  echo "eval-evidence gate drill: FAIL"
fi
exit "$fail"
