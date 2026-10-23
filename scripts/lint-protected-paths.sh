#!/usr/bin/env bash
# Protected-path gate (TG-185): make the "law surface" mechanically un-rewritable by an unattended agent.
# CODEOWNERS names the constitution + safety core + ADRs + this CI as owner-review-required, but that file
# self-declares ADVISORY (its enforcement is an out-of-repo GitLab setting that is not enabled). This gate is
# the IN-REPO mechanical control ADR-0009(ii) calls for ("if it must happen, it belongs in a gate the agent
# cannot bypass"): a change touching a protected path FAILS the pipeline unless a commit in the range carries
# an explicit `Law-Change-Approved-By: @<handle>` trailer whose handle is a declared CODEOWNERS owner.
#
# Test hooks (deterministic, no real git needed): PROTECTED_PATHS_FILES (newline-separated changed-file list)
# overrides the git diff; PROTECTED_PATHS_TRAILERS (newline-separated trailer values) overrides the git log
# scan. PROTECTED_PATHS_BASE overrides the diff/log base (default: origin/main, then main).
set -uo pipefail
cd "$(dirname "$0")/.."
fail=0

# The law surface: the constitution, the ADR record, the spec lattice, the CI/lint gates, CODEOWNERS itself,
# the assistant guidance that frames all of it — and the DECISION CORE: the code that decides whether TG may
# actuate. A change to ANY of these is a governance event that must carry an explicit approval marker.
#
# The decision-core set is deliberate and bounded to the gate chain (roadmap P0-3). Before 2026-07-26 this
# regex named `core/safety/` and NO other code path, so the interceptor + mode chokepoint (`core/actuate/`),
# the policy engine + graduation ladder (`core/policy/`), the band classifier (`core/risk/`), the prediction
# gate (`core/predict/`), the verdict author (`core/verify/`), the mutation kill-switch (`core/breaker/`) and
# the territory gate (`core/territory/`) could ALL be rewritten by an unattended agent with no marker at all —
# i.e. every surface that answers "may this actuate?" except the never-auto floor. Grandfathered at 9d82a46:
# history before that SHA predates this widening and is not retroactively flagged (the gate only ever diffs
# base..HEAD, so this is a statement of intent, not a code path).
#
# `spec/` was REMOVED from the law surface on 2026-07-30: the lockstep gate requires a spec-prose edit in
# the same MR as any governed code change, so protecting spec/ made every governed change trip this gate
# TWICE (the decision-core code path already requires the trailer) and made routine oracle/task edits a
# governance event — a per-change tax the 15-day history shows produced reflex trailers, not review. Spec
# CONTENT correctness is enforced by `specvalidate` (shape, traceability, ratify, lockstep); the law that
# spec/ carries lives in the decision-core code paths and docs named here, which stay protected.
# WIDENED TO ALL scripts/lint-*.sh (TG-310). It enumerated exactly two of them — lint-forbidden and
# lint-protected-paths — so lint-image-pins.sh, lint-eval-evidence.sh and lint-ci-shell.sh could be
# weakened or deleted with no owner trailer and no attribution. AGENTS.md and CLAUDE.md both describe the
# protected surface as `scripts/lint-*.sh`, so the documented rule and the enforced rule disagreed, in the
# direction that matters: the gates NOT covered included the supply-chain gate that had just been found
# passing vacuously. A governance surface whose own description overstates it is worse than a smaller one
# stated honestly, because everyone downstream reasons from the description.
#
# The pattern is a glob-equivalent rather than a longer list for the same reason the image-pin gate scans a
# directory: a lint added tomorrow is protected the day it lands, with nobody having to remember this line.
# THE GATE SCRIPTS. This list said `scripts/lint-[a-z-]+\.sh` and nothing else, while this gate's own
# refusal text promises to protect "the constitution / safety core / ADRs / spec / CI gates". Those are not
# the same set: a CI gate here is named lint-*, check-*, verify-*, *-gate or *-witness, and only the first
# was covered. MEASURED 2026-08-23 by weakening two of them on a branch with no trailer —
# scripts/check-actuation-guard-coverage.sh (the ACTUATION guard's coverage check) and
# scripts/deployed-sha-witness.sh (the deploy witness) — and asking this gate: it answered
# "ok (no protected law-surface path changed)", PASS. So the safety gates a reviewer would most want
# attributed were exactly the ones a change could weaken silently.
#
# Widening brings the implementation up to the scope the gate already claims; it is not a new policy.
# `_test.sh` DRILLS ARE IN (TG-538, decided 2026-08-25 under the graduation plan): a gate is falsifiable
# only through its drill — the drill is what proves the gate can go red — so a weakened drill disarms its
# gate exactly as effectively as weakening the gate, and did so unattributed while drills sat outside this
# pattern (the original `scripts/lint-[a-z-]+\.sh` excluded lint-forbidden_test.sh by the underscore
# alone, an accident of the character class, not a decision).
protected_re='^(docs/CONSTITUTION\.md|core/safety/|core/actuate/|core/policy/|core/risk/|core/predict/|core/verify/|core/breaker/|core/territory/|docs/adr/|CODEOWNERS|\.gitlab-ci\.yml|scripts/lint-[a-z_-]+\.sh|scripts/check-[a-z_-]+\.sh|scripts/verify-[a-z_-]+\.sh|scripts/[a-z-]+-gate(_test)?\.sh|scripts/[a-z-]+-witness(_test)?\.sh|AGENTS\.md|CLAUDE\.md|docs/SDD-WORKFLOW\.md|docs/GOVERNED-BEHAVIORS\.md)'

# Resolve the base to diff against. This gate needs a base to know what changed. In an MR pipeline GitLab
# provides CI_MERGE_REQUEST_DIFF_BASE_SHA; on a BRANCH pipeline (a push to main) that variable is unset, so we
# fall back to the pushed range via CI_COMMIT_BEFORE_SHA. An operator can force one with PROTECTED_PATHS_BASE.
base="${PROTECTED_PATHS_BASE:-${CI_MERGE_REQUEST_DIFF_BASE_SHA:-}}"
if [ -z "$base" ] && [ -n "${CI_COMMIT_BEFORE_SHA:-}" ]; then
  # GitLab sends an all-zero sentinel for the first push of a new branch (no predecessor commit to diff).
  case "$CI_COMMIT_BEFORE_SHA" in
    *[!0]*) base="$CI_COMMIT_BEFORE_SHA" ;;
  esac
fi

# Changed files (test hook wins; else the diff base..HEAD when a base is known).
if [ -n "${PROTECTED_PATHS_FILES+x}" ]; then
  changed="$PROTECTED_PATHS_FILES"
elif [ -n "$base" ] && git rev-parse --verify -q "$base" >/dev/null 2>&1; then
  changed="$(git diff --name-only "$base"...HEAD 2>/dev/null)"
elif [ "${CI_PIPELINE_SOURCE:-}" = "schedule" ] || [ "${CI_PIPELINE_SOURCE:-}" = "web" ] || [ "${CI_PIPELINE_SOURCE:-}" = "api" ] || [ "${CI_PIPELINE_SOURCE:-}" = "trigger" ]; then
  # A SCHEDULED or MANUALLY-TRIGGERED (web/api/trigger) pipeline introduces no change, so this change-gate has nothing to examine: GitLab sends the
  # all-zero CI_COMMIT_BEFORE_SHA and there is no MR base. Skipping here is semantic, not a bypass — the commit
  # under a schedule was already gated by the MR/push pipeline that landed it. Without this arm, the nightly
  # eval-gate schedule documented at .gitlab-ci.yml:245 would turn this job red for no change at all, and the
  # obvious "fix" would be to weaken the gate.
  echo "== protected-path gate =="
  echo "  skipped — ${CI_PIPELINE_SOURCE:-manual} pipeline introduces no change to gate (already enforced when the commit landed)."
  echo "protected-path gate: PASS"
  exit 0
elif [ -n "${CI:-}${GITLAB_CI:-}" ]; then
  # CI IS the enforcement point. Before 2026-07-26 this branch printed "skipped … PASS" and exited 0, so every
  # push pipeline to main (where CI_MERGE_REQUEST_DIFF_BASE_SHA is never set) passed the law-surface gate
  # VACUOUSLY — a green tick on a main pipeline proved nothing about that commit. A gate that cannot fail is
  # worse than no gate, so an unresolvable base in CI is now a HARD FAILURE, never a silent pass.
  echo "== protected-path gate =="
  echo "  FORBIDDEN: running in CI but no diff base could be resolved."
  echo "  Tried: PROTECTED_PATHS_BASE, CI_MERGE_REQUEST_DIFF_BASE_SHA, CI_COMMIT_BEFORE_SHA."
  echo "  The gate cannot verify what changed, so it fails closed. Ensure GIT_DEPTH: 0 so the base is fetchable."
  echo "protected-path gate: FAIL"
  exit 1
elif base="$(git merge-base HEAD origin/main 2>/dev/null)" && [ -n "$base" ]; then
  # LOCAL with a reachable origin/main: evaluate against the merge-base — the same range the MR pipeline
  # will judge. Before 2026-07-30 this branch SKIPPED, so `make all` was green locally and the same push
  # went red in CI (a trap a session only discovered after pushing, 135 times). A stacked branch includes
  # its parent's commits in this range, but an approved parent carries its trailer in the same range, so
  # already-approved history still passes; set PROTECTED_PATHS_BASE to narrow the range explicitly.
  changed="$(git diff --name-only "$base"...HEAD 2>/dev/null)"
else
  # LOCAL with no origin/main (fresh clone of a fork, offline): no reliable base exists. Skipping here is
  # not a bypass — the MR/push pipeline above is the enforcement point and fails closed.
  echo "== protected-path gate =="
  echo "  skipped — local run with no origin/main (set PROTECTED_PATHS_BASE to check a range). CI enforces."
  echo "protected-path gate: PASS"
  exit 0
fi

protected_hits="$(printf '%s\n' "$changed" | grep -E "$protected_re" || true)"

echo "== protected-path gate (base=$base) =="
if [ -z "${protected_hits//[$'\n ']/}" ]; then
  # ★ A CLEAN PASS HERE MEANS "NOTHING COMMITTED TOUCHES THE LAW SURFACE" — NOT "YOUR WORKING TREE IS FINE".
  #
  # This gate diffs $base..HEAD, so a protected file that is edited and even `git add`-ed but NOT YET
  # COMMITTED is invisible to it. Run it before committing and it prints PASS in the exact situation it
  # exists to catch. That is not hypothetical: it produced a false green twice in one session on
  # 2026-08-03, the second time immediately after the first had been noticed and understood — the local
  # PASS was quoted as evidence, the change was pushed without a trailer, and CI failed, which on this
  # project means an email to the owner.
  #
  # So when the working tree carries uncommitted law-surface edits, say so. Still PASS (nothing committed
  # has broken the rule yet), but never silently: the operator is one `git commit` away from a red pipeline.
  if [ -z "${EVAL_EVIDENCE_FILES+x}" ] && [ -z "${PROTECTED_PATHS_FILES+x}" ]; then
    pending="$( { git diff --name-only 2>/dev/null; git diff --cached --name-only 2>/dev/null; } \
                 | sort -u | grep -E "$protected_re" || true)"
    if [ -n "${pending//[$'\n ']/}" ]; then
      echo "  NOTE — uncommitted law-surface edits are present and this gate CANNOT see them:"
      printf '%s\n' "$pending" | sed 's/^/      /'
      echo "      They need a  Law-Change-Approved-By: @<codeowner>  trailer on the commit that lands them."
      echo "      Re-run this gate AFTER committing; a PASS before the commit proves nothing about them."
    fi
  fi
  echo "  ok (no protected law-surface path changed)"
  echo "protected-path gate: PASS"
  exit 0
fi

# A protected path changed — require an approval trailer whose handle is a CODEOWNERS owner.
# Owners come ONLY from ownership lines, never comments (a comment example like "@handle" must not count).
owners="$(grep -vE '^[[:space:]]*#' CODEOWNERS 2>/dev/null | grep -oE '@[A-Za-z0-9_/-]+' | sort -u)"
if [ -n "${PROTECTED_PATHS_TRAILERS+x}" ]; then
  trailers="$PROTECTED_PATHS_TRAILERS"
else
  trailers="$(git log "$base"..HEAD --pretty=format:'%(trailers:key=Law-Change-Approved-By,valueonly)' 2>/dev/null)"
fi

approved=0
approver=""
while IFS= read -r t; do
  h="$(printf '%s' "$t" | grep -oE '@[A-Za-z0-9_/-]+' | head -1)"
  [ -z "$h" ] && continue
  if printf '%s\n' "$owners" | grep -qxF "$h"; then approved=1; approver="$h"; break; fi
done <<< "$trailers"

echo "  protected paths changed:"
printf '%s\n' "$protected_hits" | sed 's/^/    /'
if [ "$approved" = 1 ]; then
  echo "  ok — approved by CODEOWNERS handle $approver via a Law-Change-Approved-By trailer"
  echo "protected-path gate: PASS"
  exit 0
fi

echo "  FORBIDDEN: a law-surface path changed WITHOUT an owner approval marker."
echo "  A change to the constitution / safety core / ADRs / spec / CI gates must be deliberate and attributed."
echo "  Add a trailer to a commit in this branch:  Law-Change-Approved-By: @<codeowner-handle>"
echo "  (the handle must be a declared owner in CODEOWNERS). See docs/CONSTITUTION.md §6."
echo "protected-path gate: FAIL"
exit 1
