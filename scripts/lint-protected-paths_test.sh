#!/usr/bin/env bash
# Test for the protected-path gate (TG-185): drives scripts/lint-protected-paths.sh through its deterministic
# env hooks (no real git state needed) and asserts it FAILS on an unapproved law-surface change and PASSES on
# an approved one / a non-protected change.
set -uo pipefail
cd "$(dirname "$0")/.."
gate=scripts/lint-protected-paths.sh
owner="$(grep -vE '^[[:space:]]*#' CODEOWNERS 2>/dev/null | grep -oE '@[A-Za-z0-9_/-]+' | head -1)"
[ -z "$owner" ] && { echo "FAIL: CODEOWNERS declares no @owner to test against"; exit 1; }
fail=0
check() { # desc expected-rc  (env already exported)
  local desc="$1" want="$2"; shift 2
  bash "$gate" >/dev/null 2>&1; local rc=$?
  if [ "$rc" = "$want" ]; then echo "  ok: $desc (rc=$rc)"; else echo "  FAIL: $desc — want rc=$want got rc=$rc"; fail=1; fi
}

echo "== protected-path gate tests (owner=$owner) =="

# (1) a protected law-surface file changed, NO approval trailer ⇒ FAIL (rc=1)
PROTECTED_PATHS_FILES=$'docs/CONSTITUTION.md\ncore/app.go' PROTECTED_PATHS_TRAILERS='' \
  check "constitution change without approval trips the gate" 1

# (2) a protected file changed WITH a valid CODEOWNERS-handle trailer ⇒ PASS (rc=0)
PROTECTED_PATHS_FILES=$'docs/CONSTITUTION.md' PROTECTED_PATHS_TRAILERS="$owner (approved 2026-07-26)" \
  check "constitution change WITH an owner approval trailer passes" 0

# (3) a protected file changed with an UNKNOWN handle (not a CODEOWNERS owner) ⇒ FAIL (rc=1)
PROTECTED_PATHS_FILES=$'core/safety/safety.go' PROTECTED_PATHS_TRAILERS='@random-stranger' \
  check "an unknown approver handle does NOT satisfy the gate" 1

# (4) only non-protected files changed ⇒ PASS (rc=0)
PROTECTED_PATHS_FILES=$'core/app.go\nREADME.md' PROTECTED_PATHS_TRAILERS='' \
  check "a non-law-surface change passes without any marker" 0

# (5) ONLY the mechanical lockstep lock changed (a routine re-stamp for a spec-owned code edit), NO trailer ⇒
#     PASS (rc=0): it is a spec<->code hash guarded by the spec-lattice job, not law content.
PROTECTED_PATHS_FILES=$'spec/.lockstep.lock\ncmd/worker/main.go' PROTECTED_PATHS_TRAILERS='' \
  check "a bare lockstep re-stamp (+ non-protected code) passes without a trailer" 0

# (6) spec CONTENT is NOT law surface (2026-07-30): the lockstep gate forces a spec edit alongside every
#     governed code change, and the governed code paths below already require the trailer — protecting spec/
#     double-tripped the gate on every governed change and made routine oracle/task edits a governance event.
#     Spec correctness is enforced by specvalidate, not by this gate. A spec-only change passes trailer-free…
PROTECTED_PATHS_FILES=$'spec/012-runner-workflow/requirements.md\nspec/012-runner-workflow/tasks.json' PROTECTED_PATHS_TRAILERS='' \
  check "spec content alone is not law surface (specvalidate owns it)" 0

# …while the governed code change that FORCED that spec edit still requires the trailer on its own hit:
PROTECTED_PATHS_FILES=$'spec/007-spec-code-lockstep/design.md\ncore/verify/verdict.go' PROTECTED_PATHS_TRAILERS='' \
  check "the decision-core half of a spec+code change still trips the gate" 1

# ---- P0-3: the DECISION CORE is law surface too -------------------------------------------------------
# Before 2026-07-26 the regex named `core/safety/` and no other code path, so every one of these passed
# silently. Each is a surface that answers "may this actuate?" — a change to any of them without an attributed
# marker is exactly the unattended-agent rewrite this gate exists to stop.
for p in core/actuate/interceptor.go core/policy/graduation.go core/risk/classifier.go \
         core/predict/prediction.go core/verify/verdict.go core/breaker/breaker.go core/territory/territory.go; do
  PROTECTED_PATHS_FILES="$p" PROTECTED_PATHS_TRAILERS='' \
    check "decision-core path $p is protected without a trailer" 1
done

# ...and the same change WITH a valid owner trailer still passes (the gate attributes, it does not forbid).
PROTECTED_PATHS_FILES=$'core/actuate/interceptor.go' PROTECTED_PATHS_TRAILERS="$owner (approved)" \
  check "a decision-core change WITH an owner trailer passes" 0

# A neighbouring core package that is NOT part of the gate chain must stay unprotected — the widening is
# bounded, not "all of core/".
PROTECTED_PATHS_FILES=$'core/knowledge/retriever.go\ncore/lessons/sink.go' PROTECTED_PATHS_TRAILERS='' \
  check "a non-gate-chain core package is NOT protected (widening is bounded)" 0

# ---- P0-2: in CI, an unresolvable base must FAIL, never silently pass -----------------------------------
# The regression this locks down: on a push pipeline to main, CI_MERGE_REQUEST_DIFF_BASE_SHA is never set, so
# the old code printed "skipped … PASS" and exited 0 — the law-surface gate passed VACUOUSLY on every main
# commit. These cases must run WITHOUT the PROTECTED_PATHS_FILES hook so the base-resolution branch is reached.
# NOTE THE `exit "$fail"` AT THE END OF THIS SUBSHELL — it is load-bearing. `check` sets `fail=1`, but inside a
# subshell that is a COPY; the parent never sees it. The closing `|| fail=1` only fires on a non-zero subshell
# EXIT STATUS, which would otherwise be the status of the last command run — so every assertion in this block
# could fail while the suite still printed PASS. That is exactly what happened: CI showed
# "FAIL: CI WITH a resolvable base ..." immediately followed by "protected-path gate tests: PASS".
(
  unset PROTECTED_PATHS_FILES
  CI=1 PROTECTED_PATHS_BASE='' CI_MERGE_REQUEST_DIFF_BASE_SHA='' CI_COMMIT_BEFORE_SHA='' \
    check "CI with NO resolvable base fails closed (was: skipped -> PASS)" 1

  # The all-zero sentinel GitLab sends for a brand-new branch is not a usable base ⇒ still fails closed in CI.
  CI=1 PROTECTED_PATHS_BASE='' CI_MERGE_REQUEST_DIFF_BASE_SHA='' \
  CI_COMMIT_BEFORE_SHA='0000000000000000000000000000000000000000' \
    check "CI with the all-zero new-branch sentinel fails closed" 1

  # A real, resolvable base in CI must NOT hit the fail-closed branch — it must EVALUATE the diff.
  #
  # The base is HEAD, not HEAD~1, and that matters. Keyed on HEAD~1 this assertion silently depended on what
  # the branch's most recent commit happened to touch: any commit carrying a spec change (routine here — the
  # lockstep gate REQUIRES a spec amendment alongside a governed code change) makes the gate correctly return
  # 1, and the test then "fails" for being right. A test coupled to repository history is flaky by
  # construction. HEAD..HEAD is resolvable AND empty, so it exercises the same base-resolution branch and
  # asserts the same property deterministically, whatever the branch contains.
  CI=1 PROTECTED_PATHS_BASE="$(git rev-parse HEAD)" \
    check "CI WITH a resolvable base evaluates the diff instead of failing closed" 0

  # A SCHEDULED pipeline gates nothing (no change) and must NOT fail closed — otherwise the nightly eval-gate
  # schedule documented at .gitlab-ci.yml:245 reds this job for no change, and the obvious "fix" is to weaken
  # the gate. Semantic skip, not a bypass: the commit was gated when it landed.
  CI=1 CI_PIPELINE_SOURCE=schedule PROTECTED_PATHS_BASE='' CI_MERGE_REQUEST_DIFF_BASE_SHA='' \
  CI_COMMIT_BEFORE_SHA='0000000000000000000000000000000000000000' \
    check "a scheduled pipeline skips (no change to gate) instead of failing closed" 0

  # ...but a non-schedule CI source with the same unresolvable base still FAILS — the exemption is keyed on
  # the pipeline source alone and cannot be reached by a push/MR pipeline.
  CI=1 CI_PIPELINE_SOURCE=push PROTECTED_PATHS_BASE='' CI_MERGE_REQUEST_DIFF_BASE_SHA='' \
  CI_COMMIT_BEFORE_SHA='0000000000000000000000000000000000000000' \
    check "a push pipeline with the same unresolvable base still fails closed" 1

  # Outside CI the gate now EVALUATES against an explicit or merge-base-derived range instead of skipping
  # (the 2026-07-30 fix for the local-green/CI-red trap). HEAD..HEAD is deterministic whatever the branch
  # contains — the assertion is that the local path evaluates rather than printing the old skip.
  out="$(env -u CI -u GITLAB_CI PROTECTED_PATHS_BASE="$(git rev-parse HEAD)" \
    CI_MERGE_REQUEST_DIFF_BASE_SHA='' CI_COMMIT_BEFORE_SHA='' bash "$gate" 2>&1)"
  rc=$?
  if [ "$rc" = 0 ] && printf '%s' "$out" | grep -q 'base=' && ! printf '%s' "$out" | grep -q 'skipped'; then
    echo "  ok: local run with a resolvable base EVALUATES the diff (rc=0, no skip)"
  else
    echo "  FAIL: local run with a resolvable base — want evaluated rc=0, got rc=$rc: $out"; fail=1
  fi
  exit "$fail" # carry this subshell's verdict to the parent — without it the whole block is unfalsifiable
) || fail=1

# TG-310: the widened glob must cover the lints that were NOT enumerated before. Named individually
# rather than asserted as a pattern, because the point of the widening is these specific gates — including
# the supply-chain one that had just been found passing vacuously.
for g in scripts/lint-image-pins.sh scripts/lint-eval-evidence.sh scripts/lint-ci-shell.sh; do
  PROTECTED_PATHS_FILES="$g" PROTECTED_PATHS_TRAILERS='' \
    check "newly-protected gate $g is covered without a trailer" 1
done
# 2026-08-23: the gate-name widening (lint-* AND check-*, verify-*, *-gate, *-witness). This SUPERSEDES
# TG-310's example above, which used scripts/verify-deploy.sh as its "stays unprotected" case — that file is
# the DEPLOY-VERIFICATION GATE, so it was the wrong specimen for the bounded-ness property it was pinning.
# The property itself is kept and still pinned, just by a script that is genuinely not a gate.
#
# WHY IT MOVED, measured rather than argued: weakening scripts/check-actuation-guard-coverage.sh (the
# ACTUATION guard's coverage check) and scripts/deployed-sha-witness.sh (the deploy witness) on a branch
# with no trailer made this gate answer "ok (no protected law-surface path changed)", PASS. The gate's own
# refusal text promises to cover "CI gates"; it covered only the lint-named ones.
for g in scripts/check-actuation-guard-coverage.sh scripts/check-merge-gate-setting.sh \
         scripts/deployed-sha-witness.sh scripts/verify-deploy.sh scripts/verify-pipeline.sh \
         scripts/deadcode-gate.sh scripts/release-gate.sh; do
  PROTECTED_PATHS_FILES="$g" PROTECTED_PATHS_TRAILERS='' \
    check "gate $g is protected without a trailer" 1
done

# BOUNDED-NESS STILL HOLDS: the widening is a set of GATE NAME SHAPES, not scripts/. A producer and an
# operator tool in the same directory stay unprotected — pinned here with specimens that are genuinely not
# gates, which is what TG-310's case meant to assert.
PROTECTED_PATHS_FILES=$'scripts/estate-docs-corpus.sh\nscripts/claim-worktree.sh' PROTECTED_PATHS_TRAILERS='' \
  check "a non-gate script stays unprotected (the widening is bounded)" 0

# The DRILLS ARE IN (TG-538, decided 2026-08-25): the drill is what proves a gate can go red, so a
# weakened drill disarms its gate exactly as effectively as weakening the gate — and did so unattributed
# while drills sat outside the pattern. All four gate-name shapes carry their _test.sh variant now.
for g in scripts/deadcode-gate_test.sh scripts/deployed-sha-witness_test.sh \
         scripts/lint-forbidden_test.sh scripts/check-nightly-drift_test.sh \
         scripts/verify-pipeline_test.sh; do
  PROTECTED_PATHS_FILES="$g" PROTECTED_PATHS_TRAILERS='' \
    check "a gate DRILL is protected: $g refuses without a trailer (TG-538)" 1
done
PROTECTED_PATHS_FILES='scripts/deadcode-gate_test.sh' PROTECTED_PATHS_TRAILERS='Law-Change-Approved-By: @ncpjfuzl' \
  check "a gate DRILL change WITH the trailer passes" 0

[ "$fail" = 0 ] && { echo "protected-path gate tests: PASS"; exit 0; } || { echo "protected-path gate tests: FAIL"; exit 1; }
