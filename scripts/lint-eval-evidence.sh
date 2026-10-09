#!/usr/bin/env bash
# EVAL-EVIDENCE GATE (TG-237 / MECH-611): make the eval gate BLOCK something.
#
# The predecessor's equivalent limb is one line — `.gitlab-ci.yml:381 allow_failure: false`. TG cannot copy
# it: the LLM-judge eval needs the on-box model gateway, and an MR pipeline has no model, no Postgres and no
# Temporal (.gitlab-ci.yml records this). So `make all` deliberately skips eval-gate and no MR pipeline runs
# it — leaving the whole eval apparatus (three-set discipline, sealed holdout, negative controls, additive
# promotion rail: all built, all sound) blocking exactly nothing. A nightly trend-watch files an issue after
# the fact.
#
# THIS GATE ENFORCES THE THING CI CAN ACTUALLY KNOW. It cannot judge a prompt change; it CAN refuse to let one
# merge with no evidence that the on-box gate was ever run. A change touching agent-behavior paths must either
#
#   (a) ADD a committed `eval/history/<date>-change-<sha>/verdict.json` whose `"pass": true` — the artifact
#       `make eval-gate` already writes and the operator already commits; or
#   (b) carry an `Eval-Gate-Waived-By: @<handle>` trailer, so a deliberate skip is a RECORDED decision by a
#       named owner instead of a silent one.
#
# The behavior set is deliberately NARROW — the rubric, the agent loop, the retrieval plane, the eval corpus,
# and the runner's prompt assembly. Measured over the last 40 commits on main these move ~3 times, so this
# gate fires on real behavior work and stays out of the way of everything else. A wide set would have blocked
# nearly every merge, which is not a gate — it is a wall, and a wall gets routed around.
#
# Test hooks (deterministic, no real git needed): EVAL_EVIDENCE_FILES overrides the changed-file list;
# EVAL_EVIDENCE_TRAILERS overrides the trailer scan; EVAL_EVIDENCE_BASE overrides the diff base.
set -uo pipefail
cd "$(dirname "$0")/.."

# The paths whose change can silently move judged quality between merges. LOADED surfaces only,
# owner-ruled 2026-08-14: `skills/` (the inert prose library, distillation output) was listed here from
# this gate's founding commit as future-proofing, matched nothing until the tree first existed, and then
# blocked a batch of load-inert FILES as if they were behavior — "a wall gets routed around" (above).
# Nothing under skills/ reaches the model from the repo tree; prose becomes behavior only through the
# store's seeding/graduation rail, and THAT write path is where evals bind.
#
# WIDENED same day (owner ruling TG-488 B9): `temporal/runner/compose_seed.go` and `core/skillstore/`
# enter the set — the C-train made them the machinery that SHAPES what reaches the model (the seed
# composer selects and assembles the prompt surface; the store's validation/transition/admission law
# decides which prose can ever compose). The TG-476 leak proved the gap concretely: a composer change
# altered every production session's guidance while sitting outside this gate. Same principle both
# directions — inert files OUT, loading machinery IN.
# WIDENED 2026-08-23: the OP-CLASS CATALOG (core/actuate/opschema/opschema.json) is a LOADED prompt surface,
# not config. It is `//go:embed`ed, and opschema.Catalog() renders EVERY registered class — its op hint, typed
# params, enums and examples — into the agent preamble (agent/loop.go:35). The registry's own doc records the
# measurement: "the change gate measured the judged dimensions drop the moment the stop verb + its registry
# meta-prose entered every preamble" — i.e. a DATA-only edit to this file moved judged quality, which is
# exactly what this gate exists to catch. It sat outside the set while `agent/` (the code that renders it) sat
# inside: the renderer was gated and the rendered content was not, the same shape as the TG-476 composer leak.
# Same principle as every entry: loaded surfaces IN, inert files OUT.
behavior_re='^(core/judge/rubric\.json|agent/|core/knowledge/|eval/corpus\.json|eval/controls\.json|temporal/runner/activities\.go|temporal/runner/compose_seed\.go|core/skillstore/|core/actuate/opschema/opschema\.json)'

base="${EVAL_EVIDENCE_BASE:-${CI_MERGE_REQUEST_DIFF_BASE_SHA:-}}"
if [ -z "$base" ] && [ -n "${CI_COMMIT_BEFORE_SHA:-}" ]; then
  case "$CI_COMMIT_BEFORE_SHA" in
    *[!0]*) base="$CI_COMMIT_BEFORE_SHA" ;;
  esac
fi

if [ -n "${EVAL_EVIDENCE_FILES+x}" ]; then
  changed="$EVAL_EVIDENCE_FILES"
elif [ -n "$base" ] && git rev-parse --verify -q "$base" >/dev/null 2>&1; then
  changed="$(git diff --name-only "$base"...HEAD 2>/dev/null)"
elif [ "${CI_PIPELINE_SOURCE:-}" = "schedule" ] || [ "${CI_PIPELINE_SOURCE:-}" = "web" ] || [ "${CI_PIPELINE_SOURCE:-}" = "api" ] || [ "${CI_PIPELINE_SOURCE:-}" = "trigger" ]; then
  # A scheduled or manually-triggered (web/api/trigger) pipeline introduces no change; the commit was gated when it landed.
  echo "== eval-evidence gate =="
  echo "  skipped — ${CI_PIPELINE_SOURCE:-manual} pipeline introduces no change to gate."
  echo "eval-evidence gate: PASS"
  exit 0
elif [ -n "${CI:-}${GITLAB_CI:-}" ]; then
  # FAIL CLOSED IN CI, for the same reason the protected-path gate does: a gate that cannot see what changed
  # and passes anyway is worse than no gate — it prints a green tick that proves nothing.
  echo "== eval-evidence gate =="
  echo "  FORBIDDEN: running in CI but no diff base could be resolved."
  echo "  Tried: EVAL_EVIDENCE_BASE, CI_MERGE_REQUEST_DIFF_BASE_SHA, CI_COMMIT_BEFORE_SHA."
  echo "eval-evidence gate: FAIL"
  exit 1
elif base="$(git merge-base HEAD origin/main 2>/dev/null)" && [ -n "$base" ]; then
  changed="$(git diff --name-only "$base"...HEAD 2>/dev/null)"
else
  echo "== eval-evidence gate =="
  echo "  skipped — local run with no origin/main (set EVAL_EVIDENCE_BASE to check a range). CI enforces."
  echo "eval-evidence gate: PASS"
  exit 0
fi

hits="$(printf '%s\n' "$changed" | grep -E "$behavior_re" || true)"

echo "== eval-evidence gate (base=$base) =="
if [ -z "${hits//[$'\n ']/}" ]; then
  # ★ SAME FALSE-GREEN AS THE PROTECTED-PATH GATE, AND IT COST A RED PIPELINE BEFORE THIS WAS ADDED.
  #
  # This gate diffs $base..HEAD. Anything edited — or even `git add`-ed — but NOT YET COMMITTED is invisible
  # to it, so running it before committing prints PASS in precisely the situation it exists to catch. The
  # sibling warning was added to lint-protected-paths.sh on 2026-08-03 and NOT to this file; on 2026-08-04 a
  # change to agent/loop.go was verified against a tree that did not contain it, reported PASS locally, and
  # failed in CI — which on this project means an email to the owner.
  #
  # A stale local BRANCH produces the same lie: `git checkout <branch>` on a branch whose commit only exists
  # on the remote leaves the gate grading a tree without the change in it. So the note names both.
  if [ -z "${EVAL_EVIDENCE_FILES+x}" ]; then
    pending="$( { git diff --name-only 2>/dev/null; git diff --cached --name-only 2>/dev/null; } \
                 | sort -u | grep -E "$behavior_re" || true)"
    if [ -n "${pending//[$'\n ']/}" ]; then
      echo "  NOTE — uncommitted agent-behavior edits are present and this gate CANNOT see them:"
      printf '%s\n' "$pending" | sed 's/^/      /'
      echo "      They need on-box eval evidence or an Eval-Gate-Waived-By: trailer on the commit that lands them."
      echo "      Re-run this gate AFTER committing; a PASS before the commit proves nothing about them."
    fi
  fi
  echo "  ok (no agent-behavior path changed)"
  echo "eval-evidence gate: PASS"
  exit 0
fi

echo "  agent-behavior paths changed:"
printf '%s\n' "$hits" | sed 's/^/    /'

# (a) an eval-history change-record ADDED in this range, whose verdict passed.
records="$(printf '%s\n' "$changed" | grep -E '^eval/history/[^/]+-change-[^/]+/verdict\.json$' || true)"
while IFS= read -r rec; do
  [ -n "$rec" ] || continue
  [ -f "$rec" ] || continue
  # No jq in the lint image, so the TOP-LEVEL verdict is matched by its INDENTATION.
  #
  # This used to grep the whole file for '"pass": true' and the comment claimed "a stable
  # "pass": <bool> field" — singular. There are SIX: one per scored dimension plus the top-level
  # verdict. So a record whose overall result was FAIL satisfied this gate on the strength of any one
  # dimension that happened to pass, and the REFUSED branch below was very nearly unreachable.
  #
  # Measured 2026-08-07 on a record already committed to a branch:
  #   eval/history/2026-08-05-change-10c4f7a606cd/verdict.json
  #     "outcome": "fail"   "pass": false        <- the verdict
  #     5 x '"pass": true'                       <- the dimensions
  #   ...and this gate reported "(pass)" and exited 0.
  #
  # evalgate writes the archive with json.MarshalIndent-style 2-space nesting, so the top-level field
  # is at exactly two spaces and a per-dimension one at six. Anchoring on that is the narrowest change
  # that distinguishes them without adding jq to the image.
  if grep -qE '^  "pass"[[:space:]]*:[[:space:]]*true' "$rec"; then
    echo "  ok — on-box eval evidence added in this change: $rec (pass)"
    echo "eval-evidence gate: PASS"
    exit 0
  fi
  # TG-500 (Law-Change-Approved-By @ncpjfuzl citing TG-488): a QUALIFIED-INCONCLUSIVE is valid evidence. The
  # sample-aware band resolves a drop the run cannot distinguish from its own measured judge-noise to
  # UNMEASURED -> INCONCLUSIVE (escalate), which is NOT a regression: no bar was broken. The gate WAS run and
  # found no certifiable regression; the under-powered capability escalates to the pooled full gate. This
  # accepts ONLY a top-level '"outcome": "inconclusive"' (the SAME 2-space anchor, so a per-dimension field
  # cannot spoof it); a top-level '"outcome": "fail"' falls through to REFUSED exactly as before, so the set
  # of records this gate lets through is widened by qualified-INCONCLUSIVE and nothing else.
  if grep -qE '^  "outcome"[[:space:]]*:[[:space:]]*"inconclusive"' "$rec"; then
    echo "  ok — on-box eval evidence added in this change: $rec (INCONCLUSIVE — no regression; an under-powered"
    echo "       capability is UNMEASURED and escalates to the pooled full gate, TG-500)"
    echo "eval-evidence gate: PASS"
    exit 0
  fi
  echo "  REFUSED: $rec records a FAILING gate run — a regression may not merge on its own evidence."
  echo "eval-evidence gate: FAIL"
  exit 1
done <<EOF
$records
EOF

# (b) an explicit, named waiver.
owners="$(grep -vE '^[[:space:]]*#' CODEOWNERS 2>/dev/null | grep -oE '@[A-Za-z0-9_/-]+' | sort -u)"
if [ -n "${EVAL_EVIDENCE_TRAILERS+x}" ]; then
  trailers="$EVAL_EVIDENCE_TRAILERS"
else
  trailers="$(git log "$base"..HEAD --pretty=format:'%(trailers:key=Eval-Gate-Waived-By,valueonly)' 2>/dev/null)"
fi
while IFS= read -r t; do
  t="$(printf '%s' "$t" | tr -d '[:space:]')"
  [ -n "$t" ] || continue
  if printf '%s\n' "$owners" | grep -qx -- "$t"; then
    echo "  ok — eval gate WAIVED by $t (recorded in a commit trailer, not skipped silently)"
    echo "eval-evidence gate: PASS"
    exit 0
  fi
  echo "  waiver handle $t is not a declared CODEOWNERS owner — ignored."
done <<EOF
$trailers
EOF

cat <<'MSG'
  FORBIDDEN: an agent-behavior path changed with no eval evidence.

  CI cannot run the LLM-judge eval (no on-box model gateway), so it enforces the one thing it can know:
  that the on-box gate WAS run for this change. Do one of:

    1. Run the gate and commit its record:
         make eval-gate            # ~10-15 min on the box
       then commit the eval/history/<date>-change-<sha>/ directory it writes.

    2. If the change genuinely cannot regress judged quality (a comment, a rename), record that decision
       in the commit instead of skipping it silently:
         Eval-Gate-Waived-By: @<codeowners-handle>
MSG
echo "eval-evidence gate: FAIL"
exit 1
