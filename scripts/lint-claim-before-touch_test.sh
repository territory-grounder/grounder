#!/usr/bin/env bash
# lint-claim-before-touch_test.sh — deterministic drills for the claim gate. Builds a REAL git
# repo + worktree fixture in a tempdir; every skip/refusal arm is executed, including the
# empty-input drill (no claim at all) per TG-365.
set -u
pass=0; fail=0
say() { if [ "$1" -eq "$2" ]; then echo "  ok: $3 (rc=$1)"; pass=$((pass+1)); else echo "  FAIL: $3 — want rc $2 got $1"; fail=$((fail+1)); fi; }
LINT="$(cd "$(dirname "$0")" && pwd)/lint-claim-before-touch.sh"
ROOT=$(mktemp -d)
trap 'rm -rf "$ROOT"' EXIT
(
  cd "$ROOT"
  git init -q main-repo
  cd main-repo
  git -c user.email=t@t -c user.name=t commit -q --allow-empty -m init
  mkdir -p .claude/worktrees/.claims
  git worktree add -q .claude/worktrees/wt1 -b feat/drill-branch >/dev/null 2>&1
) || { echo "fixture build failed"; exit 1; }
WT="$ROOT/main-repo/.claude/worktrees/wt1"

echo "== claim-before-touch drills =="
out=$(cd "$WT" && CI= GITLAB_CI= bash "$LINT" 2>&1); say $? 1 "unclaimed worktree branch is RED"
case "$out" in *repair*) echo "  ok: the refusal names its repair"; pass=$((pass+1));; *) echo "  FAIL: no repair named: $out"; fail=$((fail+1));; esac

mkdir -p "$ROOT/main-repo/.claude/worktrees/.claims/feat%2Fdrill-branch.claim"
printf 'session=drill-session\n' > "$ROOT/main-repo/.claude/worktrees/.claims/feat%2Fdrill-branch.claim/meta"
out=$(cd "$WT" && CI= GITLAB_CI= bash "$LINT" 2>&1); say $? 0 "claimed branch PASSES"
case "$out" in *drill-session*) echo "  ok: the pass names the claiming session"; pass=$((pass+1));; *) echo "  FAIL: owner not named: $out"; fail=$((fail+1));; esac

out=$(cd "$ROOT/main-repo" && CI= GITLAB_CI= bash "$LINT" 2>&1); say $? 0 "main checkout SKIPS"
case "$out" in *"main checkout"*) echo "  ok: the skip is named"; pass=$((pass+1));; *) echo "  FAIL: silent skip: $out"; fail=$((fail+1));; esac

out=$(cd "$WT" && CI=1 bash "$LINT" 2>&1); say $? 0 "CI env SKIPS with its reason"

(cd "$WT" && git checkout -q --detach HEAD)
out=$(cd "$WT" && CI= GITLAB_CI= bash "$LINT" 2>&1); say $? 0 "detached HEAD SKIPS"

out=$(cd /tmp && CI= GITLAB_CI= bash "$LINT" 2>&1); say $? 2 "outside any git tree is a tooling error"

echo "claim-before-touch drills: $pass ok, $fail failed"
[ "$fail" -eq 0 ]
