#!/usr/bin/env bash
# MIGRATION NUMBERS COLLIDE ACROSS UNMERGED BRANCHES (TG-374).
#
# core/db/migration_numbers_unique_test.go refuses two migrations sharing a number ON ONE TREE. That catches
# the collision the moment two branches meet on main — which is one merge too late, because by then somebody
# has to renumber an approved MR, and `schema_migrations` keys on the FILENAME so a renumber re-applies the
# migration on every database where the old name already ran.
#
# THE MISTAKE IS NOT CARELESSNESS, IT IS THAT `main` IS THE WRONG PLACE TO LOOK. On 2026-08-06 two open MRs
# each added an 0061 (0061_estate_snapshot_plane, 0061_graduation_credit_requires_execution). Both authors
# checked main, where 0060 was the highest, and both were right about main. Thirteen unmerged branches
# touched core/db/migrations at the time; the next free number was not discoverable from the branch point.
#
# And it had already happened: 0058_exec_class_decision and 0058_triage_decision_latency are BOTH applied in
# production. That pair merged, twice, unnoticed.
#
# WHAT THIS CHECKS. For every migration this branch ADDS relative to main, no other remote branch may hold a
# DIFFERENT file with the same number. Purely local — it reads refs the runner already fetched (GIT_DEPTH: 0),
# with no API call and no token, so it cannot fail because GitLab is slow.
#
# Env: MIGRATION_BASE (default origin/main) · MIGRATION_REFS (default the remote branches)
set -euo pipefail

BASE="${MIGRATION_BASE:-origin/main}"
DIR="core/db/migrations"

echo "== migration-collision gate (base=$BASE) =="

if ! git rev-parse --verify --quiet "$BASE" >/dev/null; then
  echo "  SKIP — $BASE is not present in this clone; nothing to compare against"
  exit 0
fi

# Migrations this branch adds that the base does not have.
added="$(git diff --name-only --diff-filter=A "$BASE"...HEAD -- "$DIR" | grep '\.up\.sql$' || true)"
if [ -z "$added" ]; then
  echo "  ok — this branch adds no migration"
  exit 0
fi

refs="${MIGRATION_REFS:-$(git for-each-ref --format='%(refname)' refs/remotes/origin | grep -v '/HEAD$' || true)}"
if [ -z "$refs" ]; then
  echo "  SKIP — no remote branches in this clone to compare against (a shallow fetch?)"
  exit 0
fi

fail=0
for path in $added; do
  file="$(basename "$path")"
  num="${file%%_*}"
  echo "  this branch adds $file (number $num)"
  for ref in $refs; do
    # Every migration on that ref sharing the number, EXCLUDING this exact filename.
    others="$(git ls-tree -r --name-only "$ref" -- "$DIR" 2>/dev/null \
              | grep "/${num}_" | grep '\.up\.sql$' | grep -v "/${file}$" || true)"
    for other in $others; do
      echo "    COLLISION: ${ref#refs/remotes/} already has $(basename "$other")"
      fail=1
    done
  done
done

if [ "$fail" -ne 0 ]; then
  cat <<'MSG'

  Two migrations sharing a number is not a lost migration — the runner keys schema_migrations on the full
  FILENAME and sorts lexically, so both apply in a defined order. What it costs is a schema whose current
  version cannot be stated, a directory no reader can order, and a rename trap: renumbering later makes a
  NEW version that re-applies wherever the old name already ran.

  Fix it HERE, before merge, where renumbering is free:
    1. pick the next number no branch claims (this gate just listed the ones that do)
    2. git mv both the .up.sql and the .down.sql
    3. make the migration idempotent (DROP ... IF EXISTS before CREATE) — a renumbered file re-applies on
       any database that already ran the old name, including the local test fixture

MSG
  exit 1
fi
echo "  ok — no other branch claims these numbers"
