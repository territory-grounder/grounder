#!/usr/bin/env bash
# DRILL for scripts/claim-worktree.sh (TG-81 borrow (a)). Fully hermetic: TG_CLAIMS_DIR points the tool
# at a throwaway directory, TG_SESSION_ID names each simulated session, and TG_CLAIM_NOW advances the
# clock without sleeping, so every arm is deterministic. Same convention as
# scripts/check-merge-gate-setting_test.sh: every verdict arm asserts BOTH the exit code and a message.
#
# KILLING MUTATIONS (executed 2026-08-14):
#   * make acquire() proceed when the branch is already held — replace the `refuse "$branch" "$claim"`
#     call with `:` (or `return 0`) — and arms (2), (7) and (9) go RED: a SECOND session would be told
#     to proceed onto a branch a live sibling owns, which is exactly the shared-tree clobber this tool
#     exists to prevent. Restore ⇒ green.
#   * make is_stale() always return 1 (never stale) and arms (6) and (8) go RED: a crashed session's
#     claim would wedge its branch forever, because nothing could ever reclaim it.
#   * make is_stale() always return 0 (always stale) and arms (2), (7), (9) go RED: every live claim
#     would be reclaimable, so no refusal would ever hold.
set -uo pipefail
cd "$(dirname "$0")/.."
CW=scripts/claim-worktree.sh
fail=0
ran=0
out=""
rc=0

check() {  # check <name> <want-rc> [<expected-substring> ...]  — reads globals out/rc set by the caller
  local name="$1" want="$2"; shift 2
  local m
  ran=$((ran + 1))
  if [ "$rc" -ne "$want" ]; then
    echo "  FAIL: $name — want rc=$want got rc=$rc"
    printf '%s\n' "$out" | sed 's/^/      /'
    fail=1
    return
  fi
  for m in "$@"; do
    if ! printf '%s' "$out" | grep -qF "$m"; then
      echo "  FAIL: $name — rc was right but the output never said '$m'"
      printf '%s\n' "$out" | sed 's/^/      /'
      fail=1
      return
    fi
  done
  echo "  ok: $name (rc=$rc)"
}

echo "== claim-worktree drill =="

# Hermetic surface: a throwaway claims dir, an EXISTING worktree path, and a path that does NOT exist.
D="$(mktemp -d)"; WT="$(mktemp -d)"; GONE="$D/never-created-worktree"
export TG_CLAIMS_DIR="$D"
trap 'rm -rf "$D" "$WT"' EXIT
# Defaults made explicit so the arithmetic in the stale arms is legible here, not just in the tool.
export TG_CLAIM_TTL=28800 TG_CLAIM_GRACE=900

# ── the ordinary lifecycle: claim → a sibling refuses → release → the branch is free again ──────────
# (1) first claim succeeds.
out="$(TG_SESSION_ID=alice bash "$CW" feat/demo "$WT" 2>&1)"; rc=$?
check "first claim by alice succeeds" 0 "claimed 'feat/demo'"

# (2) a DIFFERENT session claiming the SAME branch is REFUSED and told who holds it — the headline case.
out="$(TG_SESSION_ID=bob bash "$CW" feat/demo "$WT" 2>&1)"; rc=$?
check "second caller (bob) is REFUSED, names the holder" 1 "REFUSED" "alice"

# (3) the SAME session re-claiming its own branch is idempotent, not a refusal.
out="$(TG_SESSION_ID=alice bash "$CW" feat/demo "$WT" 2>&1)"; rc=$?
check "re-claim by the holder is idempotent" 0 "already held by THIS session"

# (4) release drops it.
out="$(TG_SESSION_ID=alice bash "$CW" --release feat/demo 2>&1)"; rc=$?
check "release frees the claim" 0 "released the claim on 'feat/demo'"

# (5) after release the branch is claimable by anyone — proves release actually freed it.
out="$(TG_SESSION_ID=bob bash "$CW" feat/demo "$WT" 2>&1)"; rc=$?
check "post-release, bob can now claim" 0 "claimed 'feat/demo'"

# ── stale reclaim: a crashed/finished session must not wedge a branch ────────────────────────────────
# (6) a claim older than the TTL is reclaimable by another session.
out="$(TG_CLAIM_NOW=1000 TG_SESSION_ID=alice bash "$CW" feat/stale-ttl "$WT" 2>&1)"; rc=$?
check "seed an old claim (age 0 at t=1000)" 0 "claimed 'feat/stale-ttl'"
out="$(TG_CLAIM_NOW=$((1000 + 28800 + 1)) TG_SESSION_ID=bob bash "$CW" feat/stale-ttl "$WT" 2>&1)"; rc=$?
check "TTL-expired claim is reclaimed" 0 "reclaimed a STALE claim" "TTL"

# (7) DISCRIMINATOR: a claim WITHIN the TTL is NOT stale — a live claim must still refuse.
out="$(TG_CLAIM_NOW=1000 TG_SESSION_ID=alice bash "$CW" feat/fresh "$WT" 2>&1)"; rc=$?
check "seed a fresh claim (t=1000)" 0 "claimed 'feat/fresh'"
out="$(TG_CLAIM_NOW=1005 TG_SESSION_ID=bob bash "$CW" feat/fresh "$WT" 2>&1)"; rc=$?
check "a still-fresh claim is NOT reclaimed (refused)" 1 "REFUSED"

# (8) a claim whose worktree path is GONE and older than the grace is reclaimable.
out="$(TG_CLAIM_NOW=1000 TG_SESSION_ID=alice bash "$CW" feat/gone "$GONE" 2>&1)"; rc=$?
check "seed a claim pointing at a soon-absent worktree" 0 "claimed 'feat/gone'"
out="$(TG_CLAIM_NOW=$((1000 + 900 + 1)) TG_SESSION_ID=bob bash "$CW" feat/gone "$WT" 2>&1)"; rc=$?
check "gone-worktree claim past grace is reclaimed" 0 "reclaimed a STALE claim" "gone"

# (9) DISCRIMINATOR: gone worktree but WITHIN grace still refuses — protects the claim-before-add window.
out="$(TG_CLAIM_NOW=1000 TG_SESSION_ID=alice bash "$CW" feat/gone2 "$GONE" 2>&1)"; rc=$?
check "seed a just-claimed, not-yet-created worktree" 0 "claimed 'feat/gone2'"
out="$(TG_CLAIM_NOW=1010 TG_SESSION_ID=bob bash "$CW" feat/gone2 "$WT" 2>&1)"; rc=$?
check "within grace, an absent worktree is NOT yet reclaimable" 1 "REFUSED"

# ── argument handling + observability ────────────────────────────────────────────────────────────────
# (10) no arguments is a usage error, distinct from a refusal.
out="$(bash "$CW" 2>&1)"; rc=$?
check "no args → usage error (rc 2)" 2 "usage:"

# (11) an unknown flag is a usage error.
out="$(bash "$CW" --nope 2>&1)"; rc=$?
check "unknown flag → usage error (rc 2)" 2 "unknown flag"

# (12) --list surfaces live claims (the 'check before you collide' affordance).
out="$(bash "$CW" --list 2>&1)"; rc=$?
check "--list shows a live claim" 0 "live worktree claims" "feat/demo"

# (13) releasing a branch with no claim is a harmless no-op, not an error.
out="$(TG_SESSION_ID=alice bash "$CW" --release feat/never-claimed 2>&1)"; rc=$?
check "release of an unheld branch is a no-op" 0 "nothing to release"

# The drill's own vacuity floor: if the arms stopped running, the drill is not proving anything.
if [ "$ran" -lt 13 ]; then
  echo "claim-worktree drill: FAIL — only $ran assertion(s) ran; the drill itself is vacuous"
  exit 1
fi

if [ "$fail" -eq 0 ]; then
  echo "claim-worktree drill: PASS ($ran assertions)"
else
  echo "claim-worktree drill: FAIL"
fi
exit "$fail"
