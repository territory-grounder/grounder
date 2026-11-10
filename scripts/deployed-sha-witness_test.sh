#!/usr/bin/env bash
# DRILL for scripts/deployed-sha-witness.sh (TG-428). Same convention as scripts/lint-image-pins_test.sh:
# every verdict arm is proven to both PASS and REFUSE against deterministic env hooks (no AWX, no git
# state), and the message is part of the contract — a witness that fails without naming BOTH shas gets
# "fixed" by deleting it.
#
# KILLING MUTATION (executed 2026-08-10): delete the no-marker/empty-tags vacuity floor from
# scripts/deployed-sha-witness.sh and the "EMPTY deployed tags" arm below goes GREEN-WRONGLY — the gate
# certifies "in sync" (rc 0) over ZERO observed containers, exactly the vacuous PASS the floor exists to
# forbid — and this drill goes RED (want rc=3 got rc=0). Restore ⇒ green.
set -uo pipefail
cd "$(dirname "$0")/.."
G=scripts/deployed-sha-witness.sh
fail=0
ran=0

# check <name> <want-rc> [<expected-substring> ...]  — hook env is set by the caller on the call line
check() {
  local name="$1" want="$2"; shift 2
  local out rc m
  out="$(bash "$G" 2>&1)"; rc=$?
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

echo "== deployed-sha witness drill =="

# Every arm pins WITNESS_TIP_MESSAGE: without it the script reads the REAL repo tip's subject, and on any
# morning after the nightly [skip ci] baseline commit these arms would silently test the resolution path
# instead of their own (TG-539 — the drill convention is deterministic hooks, no git state).

# (1) All three containers on main's sha ⇒ in sync, and the verdict carries its denominator.
WITNESS_TIP_MESSAGE='feat: x' WITNESS_DEPLOYED_TAGS='12ab34cd 12ab34cd 12ab34cd' WITNESS_MAIN_SHA=12ab34cd WITNESS_TIP_AGE_S=7200 \
  check "three matching containers → in sync" 0 "in sync" "deployed=12ab34cd main=12ab34cd" "3/3"

# (2) Mismatch with a tip well beyond the grace window ⇒ rc 1, and the output names BOTH shas — the
#     operator's first question is "stale at WHAT, against WHAT" and a one-sided message cannot answer it.
WITNESS_TIP_MESSAGE='feat: x' WITNESS_DEPLOYED_TAGS='deadbeef deadbeef deadbeef' WITNESS_MAIN_SHA=12ab34cd WITNESS_TIP_AGE_S=36000 \
  check "mismatch with a 10h-old tip → stale estate" 1 "deadbeef" "12ab34cd" "estate runs stale code"

# (3) THE VACUITY ARM (killing-mutation target): the probe answered NOTHING. Zero observed containers is
#     a blind witness, not a synchronized estate — must be rc 3 BLIND, never PASS and never plain FAIL.
WITNESS_TIP_MESSAGE='feat: x' WITNESS_DEPLOYED_TAGS='' WITNESS_MAIN_SHA=12ab34cd WITNESS_TIP_AGE_S=600 \
  check "EMPTY deployed tags → BLIND, not a vacuous verdict" 3 "WITNESS BLIND" "refusing to certify"

# (4) Mismatch minutes after the tip moved ⇒ a deploy in flight, not an incident (rc 0, says so).
WITNESS_TIP_MESSAGE='feat: x' WITNESS_DEPLOYED_TAGS='deadbeef' WITNESS_MAIN_SHA=12ab34cd WITNESS_TIP_AGE_S=600 \
  check "mismatch within the 120m grace → deploy may be in flight" 0 "in flight"

# (5) ONE stale container among synced ones is still drift — a partial rollout that stuck (rc 1).
WITNESS_TIP_MESSAGE='feat: x' WITNESS_DEPLOYED_TAGS='12ab34cd deadbeef 12ab34cd' WITNESS_MAIN_SHA=12ab34cd WITNESS_TIP_AGE_S=36000 \
  check "a single stale container among three → still stale" 1 "estate runs stale code" "1/3"

# (7) A [skip ci] tip builds no image: deployed == the last BUILDABLE ancestor is IN SYNC (rc 0), and the
#     verdict names both shas. The 2026-08-24 red (job 477837) was exactly this state judged wrongly.
#     KILLING MUTATION (executed 2026-08-25): delete the resolution block and this arm goes rc 1
#     "stale" against a tip that builds no image — the false-red TG-539 names. Restore ⇒ green.
WITNESS_TIP_MESSAGE='chore(eval): nightly trend-watch [skip ci]' WITNESS_BUILDABLE_SHA=28608210 \
WITNESS_DEPLOYED_TAGS='28608210 28608210 28608210' WITNESS_MAIN_SHA=4fb3df98 WITNESS_TIP_AGE_S=36000 \
  check "[skip ci] tip, deployed == last buildable → in sync" 0 "in sync" "4fb3df98" "last buildable 28608210"

# (8) A [skip ci] tip does NOT excuse genuine drift: deployed off the BUILDABLE ancestor, old tip ⇒ rc 1.
WITNESS_TIP_MESSAGE='chore(eval): nightly trend-watch [skip ci]' WITNESS_BUILDABLE_SHA=28608210 \
WITNESS_DEPLOYED_TAGS='deadbeef deadbeef deadbeef' WITNESS_MAIN_SHA=4fb3df98 WITNESS_TIP_AGE_S=36000 \
  check "[skip ci] tip, deployed off the buildable → still stale" 1 "estate runs stale code" "deadbeef" "28608210"

# (9) EMPTY-INPUT MUTATION ARM: the buildable resolution answered NOTHING. A skip-tip with no resolvable
#     ancestor must be BLIND (rc 3) — never "in sync" against the unbuildable tip, never a bare FAIL.
WITNESS_TIP_MESSAGE='docs: baseline [skip ci]' WITNESS_BUILDABLE_SHA= \
WITNESS_DEPLOYED_TAGS='28608210' WITNESS_MAIN_SHA=4fb3df98 WITNESS_TIP_AGE_S=600 \
  check "[skip ci] tip, unresolvable buildable → BLIND" 3 "WITNESS BLIND" "could not be resolved"

# (10) The other marker spelling and casing ([CI SKIP]) classifies the same way.
WITNESS_TIP_MESSAGE='chore: mirror sync [CI SKIP]' WITNESS_BUILDABLE_SHA=28608210 \
WITNESS_DEPLOYED_TAGS='28608210' WITNESS_MAIN_SHA=4fb3df98 WITNESS_TIP_AGE_S=36000 \
  check "[CI SKIP] casing variant → same resolution" 0 "in sync" "last buildable 28608210"

# (6) No MAIN_SHA and no git ⇒ the OTHER side of blindness. Run from a non-repo dir with every main-sha
#     source removed; the witness must refuse, not compare against an empty string.
tmpd=$(mktemp -d)
out6=$(cd "$tmpd" && env -u MAIN_SHA -u WITNESS_MAIN_SHA \
  WITNESS_DEPLOYED_TAGS='deadbeef' WITNESS_TIP_AGE_S=600 \
  bash "$OLDPWD/$G" 2>&1); rc6=$?
rm -rf "$tmpd"
ran=$((ran + 1))
if [ "$rc6" -eq 3 ] && printf '%s' "$out6" | grep -qF "no main tip"; then
  echo "  ok: no MAIN_SHA and no git → BLIND 'no main tip' (rc=3)"
else
  echo "  FAIL: no MAIN_SHA and no git — want rc=3 + 'no main tip', got rc=$rc6"
  printf '%s\n' "$out6" | sed 's/^/      /'
  fail=1
fi

# (11) The age fallback (TG-539 defect a): git unreadable, WITNESS_TIP_AGE_S unset, but the job env
#      carries CI_COMMIT_TIMESTAMP — an ancient timestamp must yield a readable age and a STALE verdict
#      (rc 1), where the pre-fix witness went BLIND "tip age is unreadable" (the 2026-08-24 red).
tmpd=$(mktemp -d)
out11=$(cd "$tmpd" && env -u WITNESS_TIP_AGE_S \
  MAIN_SHA=12ab34cd WITNESS_TIP_MESSAGE='feat: x' WITNESS_DEPLOYED_TAGS='deadbeef' \
  CI_COMMIT_TIMESTAMP='2020-01-01T00:00:00+00:00' \
  bash "$OLDPWD/$G" 2>&1); rc11=$?
rm -rf "$tmpd"
ran=$((ran + 1))
if [ "$rc11" -eq 1 ] && printf '%s' "$out11" | grep -qF "estate runs stale code"; then
  echo "  ok: git dead + CI_COMMIT_TIMESTAMP → age readable, stale verdict (rc=1)"
else
  echo "  FAIL: age fallback — want rc=1 + 'estate runs stale code', got rc=$rc11"
  printf '%s\n' "$out11" | sed 's/^/      /'
  fail=1
fi

# (11b) The JQ age path, offset form (the 2026-08-25 played run: the scheduled image has BusyBox date —
#      no GNU -d — AND no git, so the GNU-date fallback died exactly where it was needed). The hook skips
#      GNU date; the timestamp carries a REAL +02:00 offset (GitLab's server form): jq must land within
#      seconds of the true instant — an offset-sign or unapplied-offset bug is a 2h error, a full grace
#      width, and this arm's fresh-timestamp construction makes that read STALE instead of in-flight.
tmpd=$(mktemp -d)
now_utc=$(date -u +%s)
# A timestamp 30 min ago, rendered in +02:00 local form (portable arithmetic, no date -d).
ago=$(( now_utc - 1800 ))
iso_plus2=$(jq -rn --argjson e "$ago" '($e + 7200) | strftime("%Y-%m-%dT%H:%M:%S") + "+02:00"')
out11b=$(cd "$tmpd" && env -u WITNESS_TIP_AGE_S \
  MAIN_SHA=12ab34cd WITNESS_TIP_MESSAGE='feat: x' WITNESS_DEPLOYED_TAGS='deadbeef' \
  WITNESS_NO_GNU_DATE=1 CI_COMMIT_TIMESTAMP="$iso_plus2" \
  bash "$OLDPWD/$G" 2>&1); rc11b=$?
rm -rf "$tmpd"
ran=$((ran + 1))
# The assertion is the PRINTED AGE, not just the verdict: a wrong sign or an unapplied offset lands the
# parse ±2h away — in the FUTURE that is a NEGATIVE age which still reads "in flight" rc 0, so a
# verdict-only assertion is half-vacuous (measured: the sign-flip mutation survived it). ~30m must print
# as 29-31m; anything else — including "-90m old" — is the offset bug this arm exists to kill.
if [ "$rc11b" -eq 0 ] && printf '%s' "$out11b" | grep -qE "tip is (29|30|31)m old"; then
  echo "  ok: jq age path parses a +02:00-offset timestamp to the true instant (rc=0, ~30m old)"
else
  echo "  FAIL: jq offset age path — want rc=0 + 'tip is ~30m old', got rc=$rc11b"
  printf '%s\n' "$out11b" | sed 's/^/      /'
  fail=1
fi

# (12)+(13) THE REAL RESOLUTION PATH (no WITNESS_BUILDABLE_SHA hook): a throwaway git repo proves the
#     git-side ancestor walk itself — the reviewer found every skip-ci arm above hooks past it. Three
#     commits: A buildable → B carrying the marker in the BODY only (GitLab skips on the FULL message, so
#     the walk must too — a subject-only scan keeps B and produces a false expectation) → C the skip tip.
#     Expected: tip classified from C's real message, B rejected by its body, A resolved; deployed=A ⇒ in
#     sync. Then (13): history of ONLY marked commits resolves nothing ⇒ BLIND, never a verdict.
marker="[skip$(printf ' ')ci]"
tmpr=$(mktemp -d)
# LOUD construction (2026-08-25, scheduled run 50473): the first draft piped this to /dev/null, so in the
# delivery-witnesses job — whose ci-deploy-tools image cannot host a git fixture — the repo silently never
# built and both arms tested a DIFFERENT blindness ("no main tip"), failing on message not substance: the
# drill's own vacuous-fixture defect. Errors are captured; an environment that cannot git-init SKIPS both
# arms with the reason printed (they still run fully in MR CI's golang image and in every local make all),
# and a construction failure in a git-capable environment is a hard FAIL, never a silent shape-shift.
# INDEPENDENT capability probe FIRST (review finding 2026-08-25): deciding "environment cannot host a
# git fixture" by re-probing the FAILED construction conflated "no git" with "git init broke for any
# reason" — a disk-full/permission failure at init in a capable runner would have been absorbed as an
# accepted SKIP. The probe is a separate throwaway init+commit; only its failure may SKIP. Any fixture
# failure AFTER a passing probe is a hard FAIL.
probe=$(mktemp -d)
probe_err=$( (cd "$probe" && git init -q . && git config user.email d@r && git config user.name drill \
  && echo p > q && git add q && git commit -qm probe) 2>&1 )
probe_rc=$?
rm -rf "$probe"
git_fixture_ok=0
if [ "$probe_rc" -ne 0 ]; then
  echo "  SKIP: real-git arms (12)+(13) — this environment cannot host a git fixture (probe): ${probe_err:-git unusable}"
  ran=$((ran + 2))
else
  fixture_err=$( (
    cd "$tmpr" && git init -q . && git config user.email d@r && git config user.name drill
    echo a > f && git add f && git commit -qm "feat: base build"
    echo b >> f && git commit -aqm "chore: annotate

body says $marker only"
    echo c >> f && git commit -aqm "docs: baseline $marker"
  ) 2>&1 )
  fixture_rc=$?
  if [ "$fixture_rc" -eq 0 ] && [ "$(cd "$tmpr" && git rev-list --count HEAD 2>/dev/null)" = "3" ]; then
    git_fixture_ok=1
  else
    echo "  FAIL: real-git fixture did not build in a git-capable environment (probe passed): $fixture_err"
    ran=$((ran + 2))
    fail=1
  fi
fi
if [ "$git_fixture_ok" = 1 ]; then
sha_a=$(cd "$tmpr" && git rev-list --max-count=1 HEAD~2 | cut -c1-8)
out12=$(cd "$tmpr" && env -u MAIN_SHA -u WITNESS_MAIN_SHA -u WITNESS_TIP_MESSAGE -u WITNESS_BUILDABLE_SHA \
  WITNESS_DEPLOYED_TAGS="$sha_a" WITNESS_TIP_AGE_S=36000 bash "$OLDPWD/$G" 2>&1); rc12=$?
ran=$((ran + 1))
if [ "$rc12" -eq 0 ] && printf '%s' "$out12" | grep -qF "last buildable $sha_a"; then
  echo "  ok: real git walk skips a BODY-marked commit and resolves the buildable ancestor (rc=0)"
else
  echo "  FAIL: real-git resolution — want rc=0 + 'last buildable $sha_a', got rc=$rc12"
  printf '%s\n' "$out12" | sed 's/^/      /'
  fail=1
fi
orphan_err=$( (
  cd "$tmpr" && git checkout -q --orphan only-marked && { git rm -qrf . || true; }
  echo x > g && git add g && git commit -qm "chore: one $marker"
  echo y >> g && git commit -aqm "chore: two $marker"
) 2>&1 )
if [ $? -ne 0 ]; then
  echo "  FAIL: all-marked fixture did not build: $orphan_err"
  ran=$((ran + 1))
  fail=1
else
out13=$(cd "$tmpr" && env -u MAIN_SHA -u WITNESS_MAIN_SHA -u WITNESS_TIP_MESSAGE -u WITNESS_BUILDABLE_SHA \
  -u CI_API_V4_URL WITNESS_DEPLOYED_TAGS='deadbeef' WITNESS_TIP_AGE_S=600 bash "$OLDPWD/$G" 2>&1); rc13=$?
ran=$((ran + 1))
if [ "$rc13" -eq 3 ] && printf '%s' "$out13" | grep -qF "could not be resolved"; then
  echo "  ok: a history of only marked commits resolves nothing → BLIND (rc=3)"
else
  echo "  FAIL: all-marked history — want rc=3 + 'could not be resolved', got rc=$rc13"
  printf '%s\n' "$out13" | sed 's/^/      /'
  fail=1
fi
fi
fi
rm -rf "$tmpr"

# The drill's own vacuity floor: if the hooks stopped being honored (a renamed env var), every check
# above could silently test the same path and this script would still exit 0 with a cheerful banner.
if [ "$ran" -lt 14 ]; then
  echo "deployed-sha witness drill: FAIL — only $ran assertion(s) ran; the drill itself is vacuous"
  exit 1
fi

if [ "$fail" -eq 0 ]; then
  echo "deployed-sha witness drill: PASS ($ran assertions)"
else
  echo "deployed-sha witness drill: FAIL"
fi
exit "$fail"
