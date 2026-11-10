#!/usr/bin/env bash
# SELF-TEST for lint-migration-collision.sh (TG-374).
#
# A gate that cannot be shown to FIRE is a gate nobody should trust. This drives the script over a scratch
# repository with a known collision and a known-clean case, because the real repository is (by design) always
# in the clean state and would prove only that the script exits 0.
set -euo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/lint-migration-collision.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"

git init -q .
git config user.email t@t; git config user.name t
mkdir -p core/db/migrations
echo "-- base" > core/db/migrations/0001_base.up.sql
git add -A && git commit -qm base
git branch -q -M main
# A second "remote" branch that claims 0002 with ITS OWN filename.
git checkout -q -b other
echo "-- other" > core/db/migrations/0002_other_thing.up.sql
git add -A && git commit -qm other
git checkout -q main

# Fake the remote refs the gate reads, so no network is involved.
git update-ref refs/remotes/origin/main    refs/heads/main
git update-ref refs/remotes/origin/other   refs/heads/other

fails=0

# --- CASE 1: a branch adding a DIFFERENT 0002 must FAIL --------------------------------------------------
git checkout -q -b mine
echo "-- mine" > core/db/migrations/0002_my_thing.up.sql
git add -A && git commit -qm mine
if MIGRATION_BASE=refs/remotes/origin/main bash "$SCRIPT" >/dev/null 2>&1; then
  echo "FAIL: the gate PASSED a branch adding 0002_my_thing while origin/other holds 0002_other_thing"
  fails=1
else
  echo "ok: a cross-branch collision FAILS the gate"
fi

# --- CASE 2: the same branch renumbered must PASS ---------------------------------------------------------
git mv core/db/migrations/0002_my_thing.up.sql core/db/migrations/0003_my_thing.up.sql
git commit -qam renumber
if MIGRATION_BASE=refs/remotes/origin/main bash "$SCRIPT" >/dev/null 2>&1; then
  echo "ok: renumbering to a free slot PASSES"
else
  echo "FAIL: the gate rejected 0003, which no branch claims — it is refusing correct work"
  fails=1
fi

# --- CASE 3: a branch adding NO migration must PASS (the common case) --------------------------------------
git checkout -q -b docs-only main
echo hi > README.md
git add -A && git commit -qm docs
if MIGRATION_BASE=refs/remotes/origin/main bash "$SCRIPT" >/dev/null 2>&1; then
  echo "ok: a branch with no migration PASSES"
else
  echo "FAIL: the gate fired on a branch that adds no migration at all"
  fails=1
fi

# --- CASE 4: THE SAME filename on another branch is NOT a collision ----------------------------------------
# A stacked branch carries its parent's migration; that is the same file, not a second claim on the number.
git checkout -q -b stacked other
echo "-- stacked extra" > core/db/migrations/0004_stacked.up.sql
git add -A && git commit -qm stacked
git update-ref refs/remotes/origin/stacked refs/heads/stacked
git checkout -q other
if MIGRATION_BASE=refs/remotes/origin/main bash "$SCRIPT" >/dev/null 2>&1; then
  echo "ok: a stacked branch carrying the SAME file is not reported as a collision"
else
  echo "FAIL: the gate treats a stacked branch's copy of its own parent's migration as a collision — it "
  echo "      would fire on every stacked MR, which is how a gate gets switched off"
  fails=1
fi

exit "$fails"
