#!/usr/bin/env bash
# lint-claim-before-touch.sh — the parallel-session claim gate (TG-81a mechanized; TG-488 protocol).
#
# Two autonomous sessions work this repo concurrently (standing mode since 2026-08-14). The claim
# convention keeps them off each other's branches: BEFORE working a branch, a session writes
#   .claude/worktrees/.claims/<url-encoded-branch>.claim/meta   (session=<id>, epoch, iso, branch, path)
# and releases it on merge. This gate turns the convention into a refusal: inside a worktree under
# .claude/worktrees/, `make all` fails when the CURRENT branch holds no claim — so a session that
# forgot to claim (fresh boot, post-compaction) is stopped at its FIRST local gate, not at a
# duplicated MR.
#
# SKIP is a NAMED state, never a silent green (TG-365): CI clones have no machine-local claims dir;
# the main checkout is not a work surface (worktree discipline covers it); detached HEAD is a
# CI-style checkout. Each skip prints its reason.
#
# Exit: 0 PASS or named SKIP · 1 unclaimed branch (repair printed) · 2 tooling error.
set -euo pipefail

if [ -n "${CI:-}${GITLAB_CI:-}" ]; then
  echo "claim gate: SKIP — CI clone carries no machine-local claims (the gate enforces on session boxes)"
  exit 0
fi
top=$(git rev-parse --show-toplevel 2>/dev/null) || {
  echo "claim gate: TOOLING ERROR — not inside a git tree" >&2
  exit 2
}
case "$top" in
  */.claude/worktrees/*) ;;
  *)
    echo "claim gate: SKIP — main checkout (claims apply to branches under .claude/worktrees/)"
    exit 0
    ;;
esac
branch=$(git -C "$top" rev-parse --abbrev-ref HEAD)
if [ "$branch" = "HEAD" ]; then
  echo "claim gate: SKIP — detached HEAD (a CI-style checkout, not a session branch)"
  exit 0
fi
main_root=${top%/.claude/worktrees/*}
enc=$(printf '%s' "$branch" | sed 's|/|%2F|g')
claim="$main_root/.claude/worktrees/.claims/${enc}.claim/meta"
if [ ! -f "$claim" ]; then
  echo "claim gate: FAIL — branch '$branch' carries NO claim (parallel-session protocol, TG-488/TG-81a)." >&2
  echo "  repair: mkdir -p '$main_root/.claude/worktrees/.claims/${enc}.claim' && write its meta" >&2
  echo "  (session=<id> / epoch / iso / branch / path) BEFORE working the branch; release on merge." >&2
  exit 1
fi
owner=$(sed -n 's/^session=//p' "$claim" | head -1)
echo "claim gate: PASS — '$branch' claimed by session ${owner:-<unrecorded>}"
