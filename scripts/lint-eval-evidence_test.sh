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

# TG-591 — the MULTI-RECORD policy. With TG-587 a same-day re-run of one candidate keeps BOTH records (`<key>` and
# `<key>-rNN`). The gate used to grade the FIRST record in diff order (`-` sorts before `/`, so the re-run was read
# first): a committed [FAIL original, PASS re-roll] passed on the re-roll. Now: records are grouped by key, the
# highest RIGOR (top-level "runs", then the scorecard's "n") decides, and among equal rigor ANY fail refuses —
# whatever the order. Fixtures carry a sibling scorecard.json so rigor is read the way evalgate writes it.
MR=eval/history/9999-01-03-change-multirec
mkdir -p "$MR" "$MR-r02"
mkrec() { # dir runs n outcome(pass|fail|inconclusive)
  local dir="$1" runs="$2" n="$3" oc="$4" pass=false ov=false
  [ "$oc" = pass ] && { pass=true; ov=true; }
  [ "$oc" = inconclusive ] && ov=true
  mkdir -p "$dir"
  printf '{\n  "runs": %s,\n  "dims": [\n    { "dim": "appropriate_band", "delta": 0.1, "pass": true }\n  ],\n  "overall_pass": %s,\n  "pass": %s,\n  "outcome": "%s"\n}\n' "$runs" "$ov" "$pass" "$oc" > "$dir/verdict.json"
  printf '{\n  "n": %s,\n  "overall": 3.5\n}\n' "$n" > "$dir/scorecard.json"
}
trap 'rm -rf "$FAILREC" "$INCREC" "$MR" "$MR-r02" "$MR-r03"' EXIT

mkrec "$MR" 1 8 fail           # the fast gate FAILED at n=8 (recall floor at n=8 — the TG-578 specimen)
mkrec "$MR-r02" 3 36 pass      # the FULL gate then PASSED: higher rigor decides
check "a fast-gate FAIL cleared by a full-gate PASS on the same key is ACCEPTED (rigor decides)" 0 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_TRAILERS='' \
      EVAL_EVIDENCE_FILES=$'agent/loop.go\n'"$MR-r02/verdict.json"$'\n'"$MR/verdict.json" bash "$G"

mkrec "$MR" 1 8 pass           # a fast-gate PASS…
mkrec "$MR-r02" 3 36 fail      # …does NOT override a full-gate FAIL
check "a fast-gate PASS does not override a full-gate FAIL on the same key" 1 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_TRAILERS='' \
      EVAL_EVIDENCE_FILES=$'agent/loop.go\n'"$MR/verdict.json"$'\n'"$MR-r02/verdict.json" bash "$G"

mkrec "$MR" 1 8 fail           # equal rigor: FAIL original…
mkrec "$MR-r02" 1 8 inconclusive  # …INCONCLUSIVE re-roll — the re-roll must NOT launder the FAIL
check "equal rigor [FAIL, INCONCLUSIVE] REFUSES — the re-roll listed first in diff order does not launder the FAIL" 1 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_TRAILERS='' \
      EVAL_EVIDENCE_FILES=$'agent/loop.go\n'"$MR-r02/verdict.json"$'\n'"$MR/verdict.json" bash "$G"

mkrec "$MR" 1 8 pass
mkrec "$MR-r02" 1 8 fail
check "equal rigor [PASS, FAIL] REFUSES in this order too (order-independent)" 1 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_TRAILERS='' \
      EVAL_EVIDENCE_FILES=$'agent/loop.go\n'"$MR/verdict.json"$'\n'"$MR-r02/verdict.json" bash "$G"

mkrec "$MR" 3 36 pass
mkrec "$MR-r02" 3 36 pass
mkrec "$MR-r03" 1 8 fail       # a LOWER-rigor fail beside two full passes is reported, not graded
check "a lower-rigor FAIL beside full-gate PASSES is ACCEPTED (the full gate is the evidence)" 0 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_TRAILERS='' \
      EVAL_EVIDENCE_FILES=$'agent/loop.go\n'"$MR-r03/verdict.json"$'\n'"$MR/verdict.json"$'\n'"$MR-r02/verdict.json" bash "$G"

# Records of an OLDER DATE in the range (an older candidate's record left in the branch beside the current one)
# are listed, not graded — even at HIGHER rigor than the newest date's record. A SAME-DAY sibling candidate (a
# different sha, same date) IS graded with the rest: its FAIL refuses. (The old gate decided by diff order; a
# sha-ordered key rule would have let a same-day FAIL hide behind a lexically greater PASS.)
OLD=eval/history/9999-01-01-change-oldcand
SIB=eval/history/9999-01-03-change-sibling
trap 'rm -rf "$FAILREC" "$INCREC" "$MR" "$MR-r02" "$MR-r03" "$OLD" "$SIB"' EXIT
mkrec "$MR" 1 8 pass
mkrec "$OLD" 3 36 fail         # older DATE, higher rigor — still not this candidate's evidence
check "an older-dated candidate's FAIL (even at higher rigor) beside the newest date's PASS does not refuse" 0 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_TRAILERS='' \
      EVAL_EVIDENCE_FILES=$'agent/loop.go\n'"$OLD/verdict.json"$'\n'"$MR/verdict.json" bash "$G"
rm -rf "$OLD"
mkrec "$SIB" 1 8 fail          # same date, different sha, equal rigor — graded together, refuses
check "a SAME-DAY sibling candidate's FAIL beside this candidate's PASS REFUSES (graded together)" 1 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_TRAILERS='' \
      EVAL_EVIDENCE_FILES=$'agent/loop.go\n'"$MR/verdict.json"$'\n'"$SIB/verdict.json" bash "$G"
rm -rf "$SIB"
mkrec "$MR" 1 8 fail           # this candidate's fast-gate FAIL…
mkrec "$SIB" 3 36 pass         # …beside a same-day SIBLING's full-gate PASS: rigor is per key, the sibling clears nothing
check "a same-day sibling's HIGHER-rigor PASS does not clear this candidate's FAIL (rigor is per key)" 1 \
  env EVAL_EVIDENCE_BASE=HEAD EVAL_EVIDENCE_TRAILERS='' \
      EVAL_EVIDENCE_FILES=$'agent/loop.go\n'"$SIB/verdict.json"$'\n'"$MR/verdict.json" bash "$G"
rm -rf "$SIB" "$MR"

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
