#!/usr/bin/env bash
# CLAIM-BEFORE-TOUCH worktree hygiene (TG-81 borrow (a), the h-conductor pattern).
#
# THE RACE THIS ENDS: multiple autonomous sessions share ONE git working tree (the AFK multi-session
# build — see the [[parallel-session-shared-tree-race]] lesson). Two of them can `git worktree add` the
# SAME branch, or edit the same paths, ~seconds apart and clobber each other: duplicate branches, a
# `git add` sweeping a sibling's untracked file into the wrong commit, HEAD moving under you.
#
# THE CONVENTION (advisory, NOT a distributed lock): before you create a worktree, CLAIM the branch.
#   1. `scripts/claim-worktree.sh <branch> <worktree-path>`   → take the claim, then `git worktree add`.
#   2. A second session running the same claim sees a LIVE claim and REFUSES (exit 1) — it yields.
#   3. `scripts/claim-worktree.sh --release <branch>`         → drop the claim when the worktree is done.
#   4. `scripts/claim-worktree.sh --list`                     → show every live claim (who holds what).
#
# ATOMICITY: the claim is a DIRECTORY created with `mkdir`, which fails-if-exists as ONE syscall — the
# admission decision and the write are the same operation, so there is no test-then-write window two
# racers can both pass. The holder's identity is written into <claim>/meta immediately after.
#
# CRASH SAFETY (a dead session must not wedge a branch forever): a claim is RECLAIMABLE when it is
# older than TG_CLAIM_TTL (default 8h), OR its recorded worktree path no longer exists AND it is older
# than TG_CLAIM_GRACE (default 15m — long enough that a just-claimed, not-yet-created worktree is still
# respected). A reclaim removes the stale claim and takes it, announcing what it displaced.
#
# WHERE CLAIMS LIVE: <main-worktree-root>/.claude/worktrees/.claims/ — resolved from
# `git rev-parse --git-common-dir` so every linked worktree agrees on the one shared directory. That
# path is under .claude/worktrees/, which .gitignore excludes wholesale, so a claim can NEVER be
# committed. Run this from inside the repo (any worktree of it).
#
# EXIT CODES (a caller reads these: 0 = proceed, non-zero = do NOT touch the branch):
#   0  claim acquired, reclaimed-from-stale, already-held-by-this-session, released, or listed
#   1  REFUSED — the branch is claimed by another LIVE session; yield
#   2  usage error (missing/'--' malformed args, or a branch/path with control characters)
#   3  environment error (not in a git repo and TG_CLAIMS_DIR unset; the claims dir is unusable)
#
# TEST HOOKS (deterministic; used by scripts/claim-worktree_test.sh — defaults are the real surface):
#   TG_CLAIMS_DIR   override the claims directory (hermetic tests never touch the real one)
#   TG_SESSION_ID   the claiming session's identity (default: $CLAUDE_CODE_SESSION_ID, then user@host)
#   TG_CLAIM_NOW    the "current" epoch seconds (advance the clock without sleeping)
#   TG_CLAIM_TTL    seconds after which any claim is stale (default 28800)
#   TG_CLAIM_GRACE  seconds after which a claim whose worktree is gone is stale (default 900)
set -euo pipefail

SESSION="${TG_SESSION_ID:-${CLAUDE_CODE_SESSION_ID:-$(id -un 2>/dev/null || echo user)@$(hostname -s 2>/dev/null || echo host)}}"
TTL="${TG_CLAIM_TTL:-28800}"
GRACE="${TG_CLAIM_GRACE:-900}"
STALE_REASON=""

usage() {
  cat >&2 <<'USAGE'
usage:
  claim-worktree.sh <branch> <worktree-path>   claim <branch> before `git worktree add` (yields if held)
  claim-worktree.sh --release <branch>          drop the claim on <branch>
  claim-worktree.sh --list                      show every live claim
  claim-worktree.sh --help                      this text
exit: 0 acquired/reclaimed/released/listed · 1 REFUSED (held elsewhere) · 2 usage · 3 environment
USAGE
}

now() { printf '%s' "${TG_CLAIM_NOW:-$(date +%s)}"; }

# Filesystem-safe claim key. Branch names commonly contain '/', which would spawn nested directories,
# so it is the one character remapped; control characters are rejected before we ever get here.
encode() { printf '%s' "${1//\//%2F}"; }

# Read one single-line value from a claim's meta file. Keys are a fixed internal vocabulary, so there
# is no injection surface; an absent key or file yields the empty string.
meta_get() { sed -n "s/^$1=//p" "$2" 2>/dev/null | head -n1; }

# Epoch a claim was taken: the recorded value if present and numeric, else the claim dir's mtime.
claim_epoch() {
  local e
  e="$(meta_get epoch "$1/meta")"
  if [[ -n "$e" && "$e" =~ ^[0-9]+$ ]]; then
    printf '%s' "$e"
    return 0
  fi
  stat -c %Y "$1" 2>/dev/null || printf ''
}

# Is this claim reclaimable? Sets STALE_REASON when yes. Age wins; a gone-worktree past the grace is
# the faster signal for a session that finished/crashed without releasing.
is_stale() {
  local claim="$1" ep age path now_s
  now_s="$(now)"
  ep="$(claim_epoch "$claim")"
  [[ "$ep" =~ ^[0-9]+$ ]] || ep=0
  age=$(( now_s - ep ))
  (( age < 0 )) && age=0
  if (( age > TTL )); then
    STALE_REASON="age ${age}s > TTL ${TTL}s"
    return 0
  fi
  path="$(meta_get path "$claim/meta")"
  if [[ -n "$path" && ! -e "$path" ]] && (( age > GRACE )); then
    STALE_REASON="worktree '$path' is gone and age ${age}s > grace ${GRACE}s"
    return 0
  fi
  return 1
}

write_meta() {
  local claim="$1" branch="$2" path="$3" now_s iso tmp
  now_s="$(now)"
  iso="$(date -u -d "@$now_s" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)"
  tmp="$claim/.meta.$$"
  {
    printf 'session=%s\n' "$SESSION"
    printf 'epoch=%s\n' "$now_s"
    printf 'iso=%s\n' "$iso"
    printf 'branch=%s\n' "$branch"
    printf 'path=%s\n' "$path"
  } >"$tmp"
  mv -f "$tmp" "$claim/meta"
}

refuse() {
  local branch="$1" claim="$2" holder path iso ep age now_s
  holder="$(meta_get session "$claim/meta")"
  path="$(meta_get path "$claim/meta")"
  iso="$(meta_get iso "$claim/meta")"
  ep="$(claim_epoch "$claim")"; now_s="$(now)"
  if [[ "$ep" =~ ^[0-9]+$ ]]; then age="$(( now_s - ep ))s"; else age="?"; fi
  echo "REFUSED: branch '$branch' is already claimed by another session — yield, do not clobber it."
  echo "  held by session : ${holder:-unknown}"
  echo "  worktree        : ${path:-unknown}"
  echo "  claimed at      : ${iso:-unknown} (age $age)"
  echo "  claim file      : $claim/meta"
  echo "  If that session is gone the claim self-expires after ${TTL}s; or free it deliberately:"
  echo "    $0 --release $branch"
  exit 1
}

acquire() {
  local branch="$1" path="$2" claim reclaimed=0 holder
  claim="$CLAIMS_DIR/$(encode "$branch").claim"
  if ! mkdir "$claim" 2>/dev/null; then
    # The claim dir already exists — held-by-us, stale, or held by a live sibling.
    holder="$(meta_get session "$claim/meta")"
    if [[ -n "$holder" && "$holder" == "$SESSION" ]]; then
      echo "claim on '$branch' is already held by THIS session ($SESSION) — proceeding."
      echo "  claim file: $claim/meta"
      return 0
    fi
    if is_stale "$claim"; then
      case "$claim" in
        *.claim) : ;;
        *) echo "internal error: refusing to remove non-claim path '$claim'" >&2; exit 3 ;;
      esac
      rm -rf "$claim"
      mkdir "$claim" 2>/dev/null || { echo "lost a reclaim race on '$branch' — re-run" >&2; exit 3; }
      reclaimed=1
      echo "reclaimed a STALE claim on '$branch' (was: ${holder:-unknown}; $STALE_REASON)"
    else
      refuse "$branch" "$claim"
    fi
  fi
  write_meta "$claim" "$branch" "$path"
  if (( reclaimed == 0 )); then
    echo "claimed '$branch' for worktree '$path' (session $SESSION)"
  fi
  echo "  claim file: $claim/meta"
  echo "  release when done:  $0 --release $branch"
}

release() {
  local branch="$1" claim holder
  claim="$CLAIMS_DIR/$(encode "$branch").claim"
  if [[ ! -d "$claim" ]]; then
    echo "no live claim on '$branch' — nothing to release (looked in $CLAIMS_DIR)"
    return 0
  fi
  holder="$(meta_get session "$claim/meta")"
  case "$claim" in
    *.claim) : ;;
    *) echo "internal error: refusing to remove non-claim path '$claim'" >&2; exit 3 ;;
  esac
  rm -rf "$claim"
  echo "released the claim on '$branch' (was held by ${holder:-unknown})"
}

list_claims() {
  local any=0 d branch holder path ep age now_s tag
  now_s="$(now)"
  echo "== live worktree claims ($CLAIMS_DIR) =="
  shopt -s nullglob
  for d in "$CLAIMS_DIR"/*.claim; do
    any=1
    branch="$(meta_get branch "$d/meta")"
    holder="$(meta_get session "$d/meta")"
    path="$(meta_get path "$d/meta")"
    ep="$(claim_epoch "$d")"
    if [[ "$ep" =~ ^[0-9]+$ ]]; then age="$(( now_s - ep ))s"; else age="?"; fi
    tag=""
    is_stale "$d" && tag="  [STALE: $STALE_REASON]"
    printf '  %s\n      session=%s  path=%s  age=%s%s\n' "${branch:-?}" "${holder:-?}" "${path:-?}" "$age" "$tag"
  done
  shopt -u nullglob
  (( any == 1 )) || echo "  (none)"
}

# Validate a branch or path argument: present, and free of control characters (which would corrupt the
# single-line meta format and the claim key).
validate_ref() {
  local v="$1" what="$2"
  if [[ -z "$v" ]]; then echo "ERROR: empty $what" >&2; usage; exit 2; fi
  if [[ "$v" =~ [[:cntrl:]] ]]; then echo "ERROR: $what must not contain control characters" >&2; exit 2; fi
}

resolve_claims_dir() {
  if [[ -n "${TG_CLAIMS_DIR:-}" ]]; then printf '%s' "$TG_CLAIMS_DIR"; return 0; fi
  local common
  if ! common="$(git rev-parse --git-common-dir 2>/dev/null)"; then
    echo "ERROR: not inside a git repository and TG_CLAIMS_DIR is unset — cannot locate the claims dir." >&2
    return 1
  fi
  if ! common="$(cd "$common" 2>/dev/null && pwd)"; then
    echo "ERROR: could not resolve the git common dir to an absolute path." >&2
    return 1
  fi
  printf '%s' "$(dirname "$common")/.claude/worktrees/.claims"
}

main() {
  local cmd="${1:-}"
  case "$cmd" in
    --help|-h) usage; exit 0 ;;
    "") usage; exit 2 ;;
  esac

  CLAIMS_DIR="$(resolve_claims_dir)" || exit 3
  mkdir -p "$CLAIMS_DIR" 2>/dev/null || { echo "ERROR: cannot create claims dir '$CLAIMS_DIR'" >&2; exit 3; }

  case "$cmd" in
    --list)
      list_claims
      ;;
    --release)
      local branch="${2:-}"
      validate_ref "$branch" "branch"
      release "$branch"
      ;;
    --*)
      echo "ERROR: unknown flag '$cmd'" >&2; usage; exit 2
      ;;
    *)
      local branch="$cmd" path="${2:-}"
      validate_ref "$branch" "branch"
      validate_ref "$path" "worktree-path"
      acquire "$branch" "$path"
      ;;
  esac
}

main "$@"
