#!/usr/bin/env bash
# deadcode-gate_test.sh — the dead-code gate's own drill (TG-4).
#
# TG-283's lesson is that a GATE can be broken while every pipeline stays green, so a gate without an oracle
# that makes it FAIL is an unverified claim. This gate shipped without one — and then failed twice in ways a
# drill would have caught immediately: a locale-dependent `sort` (green locally, RED in CI on identical
# content) and a root set of 9 of the module's 26 mains (43 baseline entries were live code).
#
# The RATCHET DECISION is what is drilled here — new entry / unratcheted shrink / clean / missing baseline /
# broken root set — using DEADCODE_GATE_CURRENT to supply a precomputed "current" list instead of building a
# fixture Go module. The seam cannot make a REAL run pass: unset, the gate always runs the analyzer.
set -uo pipefail
cd "$(dirname "$0")/.."
G="$PWD/scripts/deadcode-gate.sh"

fails=0
check() { # check <name> <want-rc> <baseline-file> <current-file>
  local name="$1" want="$2" base="$3" cur="$4" rc
  DEADCODE_GATE_BASELINE="$base" DEADCODE_GATE_CURRENT="$cur" bash "$G" check >/dev/null 2>&1
  rc=$?
  if [ "$rc" = "$want" ]; then
    echo "  ok: $name (rc=$rc)"
  else
    echo "  FAIL: $name — want rc=$want got rc=$rc" >&2
    fails=$((fails + 1))
  fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

printf 'a/x.go: Alpha\nb/y.go: Beta\n' > "$tmp/baseline.txt"

echo "== dead-code gate drill =="

# 1. The tree matches the baseline exactly — the only state that may pass.
cp "$tmp/baseline.txt" "$tmp/same.txt"
check "a tree matching the baseline PASSES" 0 "$tmp/baseline.txt" "$tmp/same.txt"

# 2. NEW dead code that is not in the baseline must FAIL — the gate's primary duty. A merge that orphans a
#    function must wire it or delete it; it may never ship silently dark.
printf 'a/x.go: Alpha\nb/y.go: Beta\nc/z.go: Gamma\n' > "$tmp/new.txt"
check "NEW dead code not in the baseline FAILS" 1 "$tmp/baseline.txt" "$tmp/new.txt"

# 3. A baseline entry that is no longer dead must ALSO fail (unratcheted): the baseline can only go DOWN, so a
#    fix that is not committed back would let the list rot into a grandfather-everything file.
printf 'a/x.go: Alpha\n' > "$tmp/shrunk.txt"
check "a FIXED entry with no ratchet-down FAILS" 1 "$tmp/baseline.txt" "$tmp/shrunk.txt"

# 4. Both directions at once still fails (a swap must not cancel out into a false pass).
printf 'a/x.go: Alpha\nc/z.go: Gamma\n' > "$tmp/swap.txt"
check "a simultaneous add+remove FAILS (they must not cancel)" 1 "$tmp/baseline.txt" "$tmp/swap.txt"

# 5. No baseline at all is a TOOLING error (2), never a pass — an absent baseline must not read as "nothing
#    is dead".
check "a MISSING baseline is a tooling error, not a pass" 2 "$tmp/nonexistent.txt" "$tmp/same.txt"

# 6. An EMPTY analyzer result against a non-empty baseline reads as a massive unratcheted shrink and fails.
#    This is the vacuity floor in the direction that matters: a silently broken analyzer must never pass.
: > "$tmp/empty.txt"
check "an EMPTY result against a real baseline FAILS (vacuity)" 1 "$tmp/baseline.txt" "$tmp/empty.txt"

# 7. The ROOT-SET floor: a `go list` that returns almost nothing would shrink the reachable set and
#    manufacture a wall of fake "new dead code". Fewer than ten roots must fail as a broken enumeration.
#    Exercised against the REAL gate (no current override), with the enumeration stubbed to one package.
stub="$tmp/stubbed-gate.sh"
sed 's|mains="$(go list .*|mains="only/one/main"|' "$G" > "$stub"
bash "$stub" check >/dev/null 2>&1
rc=$?
if [ "$rc" = "2" ]; then
  echo "  ok: a BROKEN root enumeration fails as tooling, not as dead code (rc=2)"
else
  echo "  FAIL: a BROKEN root enumeration — want rc=2 got rc=$rc" >&2
  fails=$((fails + 1))
fi

# 8. LOCALE INDEPENDENCE (the defect that took this gate red in CI while green locally): the same content
#    sorted under a different collation must still pass, because the gate pins LC_ALL=C internally.
printf 'core/db/agent_step_evidence.go: NewMemStore\ncore/Falsify/mem.go: Seed\nz/z.go: Zeta\n' > "$tmp/loc_cur.txt"
LC_ALL=C sort -u "$tmp/loc_cur.txt" > "$tmp/loc_base.txt"
LC_ALL=en_US.UTF-8 check "content sorted in ANOTHER locale still PASSES (LC_ALL=C is pinned)" 0 "$tmp/loc_base.txt" "$tmp/loc_cur.txt"

if [ "$fails" -gt 0 ]; then
  echo "dead-code gate drill: FAIL ($fails)" >&2
  exit 1
fi
echo "dead-code gate drill: PASS"
