#!/usr/bin/env bash
# The ORIENTATION-BUDGET gate (TG-428): the resume path must fit in one cold-start sitting.
#
# Definition-of-done v1.1 criterion 3 (docs/BOARD.md § Definition of done) gives a cold session a
# 10,000-TOKEN reading budget to reach oriented-and-working: exactly CLAUDE.md -> AGENTS.md ->
# docs/BOARD.md, nothing else mandatory. Tokens are model-dependent, so this gate enforces BYTES at a
# STATED, DELIBERATELY CONSERVATIVE 4 bytes/token: 10k tokens x 4 B/token = 40,000 bytes
# (RESUME_BUDGET_MAX_BYTES). English prose runs ~4-5 B/token, so a surface that fits 40,000 bytes fits
# the 10k-token budget with margin; the conversion is written here, beside the number, so the budget
# cannot silently drift from its justification.
#
# Why this exists: on 2026-08-10 docs/BOARD.md alone was 103KB — a month of journal appended to the
# queue — and the "two mandatory files" claim in CLAUDE.md was true only in prose. The board split
# (TG-428, docs/history/BOARD-JOURNAL-2026-08.md) fixed the instance; this gate makes the CLASS fail:
# the next journal-shaped accretion reds the pipeline instead of compounding for a month.
#
# SECOND PROBE — the coherence ratchet. The resume files PLUS CONTRIBUTING.md must contain ZERO
# occurrences of the two recurring stale claims this repo has already retired twice:
#     'Multi-tenant by default'            (retired by ADR-0010: single-org, external_ref, no tenant_id)
#     'mutation stays globally disabled'   (false since the Phase-2 flip, 2026-07-20)
# docs/history/ and docs/adr/ are deliberately NOT scanned — history is allowed to say what was true
# then; the ratchet polices what a cold session is TOLD IS TRUE NOW.
#
# EXIT CODES — absence is not thinness:
#   0  under budget AND zero ratchet hits
#   1  over budget, or a retired claim is back on the steering surface
#   2  tooling error (a required tool is missing, or a readable file could not be measured)
#   3  a listed file is ABSENT or EMPTY — the resume path is BROKEN, not thin. A gate that lets a
#      missing file shrink the total toward a pass rewards deleting orientation; refuse instead.
#
# Test hooks (used by scripts/lint-resume-budget_test.sh; defaults are the real steering surface):
#   RESUME_BUDGET_FILES         whitespace-separated resume-path files
#   RESUME_BUDGET_CONTRIBUTING  the culture file added to the ratchet scan
#   RESUME_BUDGET_MAX_BYTES     the byte budget
set -u

command -v wc >/dev/null 2>&1 && command -v grep >/dev/null 2>&1 && command -v awk >/dev/null 2>&1 \
  && command -v sort >/dev/null 2>&1 || {
  echo "resume-budget gate: TOOLING ERROR — needs wc, grep, awk and sort on PATH"
  exit 2
}

MAX="${RESUME_BUDGET_MAX_BYTES:-40000}"
FILES="${RESUME_BUDGET_FILES:-CLAUDE.md AGENTS.md docs/BOARD.md}"
CONTRIB="${RESUME_BUDGET_CONTRIBUTING:-CONTRIBUTING.md}"

echo "== resume-budget gate (orientation budget: ${MAX} bytes ~= $(awk -v m="$MAX" 'BEGIN{printf "%g", m/4000}')k tokens at 4 B/token) =="

# ── probe 1: the byte budget ────────────────────────────────────────────────────────────────────────
# Absence and emptiness are checked BEFORE any byte is added to the total, and each refuses with its
# own exit code. "Sum whatever exists" would let a deleted BOARD.md read as the thinnest possible
# resume path — the exact inversion of what this gate asserts.
n_listed=0
n_read=0
total=0
sizes=""
for f in $FILES; do
  n_listed=$((n_listed + 1))
  if [ ! -f "$f" ]; then
    echo "RESUME FILE ABSENT: $f — the resume path is BROKEN, not thin; absence is not a 0-byte pass"
    exit 3
  fi
  bytes=$(wc -c < "$f") || {
    echo "resume-budget gate: TOOLING ERROR — could not measure $f"
    exit 2
  }
  if [ "$bytes" -eq 0 ]; then
    echo "RESUME FILE EMPTY (0 bytes): $f — present but says nothing; an empty orientation file is a broken one, not a cheap one"
    exit 3
  fi
  printf '  %7d bytes  %s\n' "$bytes" "$f"
  total=$((total + bytes))
  n_read=$((n_read + 1))
  sizes="${sizes}${bytes}	${f}
"
done

tok=$(awk -v b="$total" 'BEGIN{printf "%.1f", b/4000}')
tokmax=$(awk -v m="$MAX" 'BEGIN{printf "%g", m/4000}')
budget_fail=0
if [ "$total" -le "$MAX" ]; then
  echo "resume surface: $total of $MAX bytes (~${tok}k of ${tokmax}k tokens) across $n_read of $n_listed files — PASS"
else
  largest=$(printf '%s' "$sizes" | sort -rn | head -1)
  largest_bytes=${largest%%	*}
  largest_file=${largest#*	}
  echo "resume surface: $total of $MAX bytes (~${tok}k of ${tokmax}k tokens) across $n_read of $n_listed files — FAIL"
  echo "  OVER BUDGET by $((total - MAX)) bytes. Largest file: $largest_file ($largest_bytes bytes) — move"
  echo "  narrative to docs/history/ (journal entries belong in BOARD-JOURNAL-<month>.md, not the queue);"
  echo "  do NOT raise the budget to fit the prose."
  budget_fail=1
fi

# ── probe 2: the coherence ratchet ──────────────────────────────────────────────────────────────────
# The ratchet's scan set is the resume files + CONTRIBUTING.md. A missing or empty member refuses with
# exit 3 for the same reason as above: "0 hits in a file that is not there" is a claim about nothing.
if [ ! -f "$CONTRIB" ]; then
  echo "RATCHET FILE ABSENT: $CONTRIB — cannot assert 0 stale claims over a file that is not there; absence is not a 0-hit pass"
  exit 3
fi
if [ ! -s "$CONTRIB" ]; then
  echo "RATCHET FILE EMPTY (0 bytes): $CONTRIB — present but says nothing; the ratchet would be vacuous over it"
  exit 3
fi

n_scanned=$((n_listed + 1))
hits=$(grep -nHF -e 'Multi-tenant by default' -e 'mutation stays globally disabled' $FILES "$CONTRIB" 2>/dev/null)
ratchet_fail=0
if [ -z "$hits" ]; then
  echo "coherence ratchet: 0 hits in $n_scanned files scanned — PASS"
else
  n_hits=$(printf '%s\n' "$hits" | grep -c .)
  echo "coherence ratchet: $n_hits hit(s) in $n_scanned files scanned — FAIL: a RETIRED claim is back on the steering surface:"
  printf '%s\n' "$hits" | sed 's/^/    /'
  echo "  'Multi-tenant by default' died with ADR-0010; 'mutation stays globally disabled' died with the"
  echo "  Phase-2 flip (2026-07-20). Fix the claim, not the ratchet — history lives in docs/history/."
  ratchet_fail=1
fi

if [ "$budget_fail" -ne 0 ] || [ "$ratchet_fail" -ne 0 ]; then
  echo "resume-budget gate: FAIL"
  exit 1
fi
echo "resume-budget gate: PASS — the resume path fits the orientation budget and carries no retired claims"
exit 0
