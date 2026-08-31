#!/usr/bin/env bash
# release-gate_test.sh — the §5 release gate's own drill (TG-5).
#
# The gate's whole value is the RED / BLOCKED / GREEN distinction and the exit code that follows from it:
# a release must be refused on a failure and must NOT be certified on missing evidence. A gate whose
# decision logic is untested is the "check that cannot report nothing to check" shape it exists to prevent.
# Every case is hermetic (a fixture root under $TMPDIR); RELEASE_GATE_NO_GO skips only the two Go-module
# limbs, and they report BLOCKED — so the flag can never manufacture the PASS a case is asserting.
set -uo pipefail
cd "$(dirname "$0")/.."
G="$PWD/scripts/release-gate.sh"

fails=0
check() { # check <name> <want-rc> <root>
  local name="$1" want="$2" root="$3" rc
  RELEASE_GATE_ROOT="$root" RELEASE_GATE_NO_GO=1 bash "$G" >/dev/null 2>&1
  rc=$?
  if [ "$rc" = "$want" ]; then
    echo "  ok: $name (rc=$rc)"
  else
    echo "  FAIL: $name — want rc=$want got rc=$rc" >&2
    fails=$((fails + 1))
  fi
}

# grep the gate's report for a line, in a fixture root. The output is CAPTURED first, never piped straight
# into grep: this script runs under `pipefail`, and the gate exits 3 by design when it cannot certify — piping
# would make the pipeline inherit that 3 and report every assertion as a failure regardless of the match.
saysline() { # saysline <name> <pattern> <root>
  local name="$1" pat="$2" root="$3" out
  out="$(RELEASE_GATE_ROOT="$root" RELEASE_GATE_NO_GO=1 bash "$G" 2>&1 || true)"
  if printf '%s\n' "$out" | grep -q "$pat"; then
    echo "  ok: $name"
  else
    echo "  FAIL: $name — report has no line matching: $pat" >&2
    fails=$((fails + 1))
  fi
}

fixture() { # fixture <verdict-json-or-empty> -> prints the root
  local body="$1"
  local root
  root="$(mktemp -d)"
  mkdir -p "$root/docs/contracts"
  # Contract fields present, so §5.5 is not the thing under test in the verdict cases.
  for f in openapi.yaml asyncapi.yaml; do
    printf 'x-tg:\n    generated_at: "2026-01-01T00:00:00Z"\n    source_hash: "%064d"\n    coverage_scope: "routes=1;entities=1"\n' 0 > "$root/docs/contracts/$f"
  done
  if [ -n "$body" ]; then
    mkdir -p "$root/eval/history/2026-01-01-change-deadbeef"
    printf '%s' "$body" > "$root/eval/history/2026-01-01-change-deadbeef/verdict.json"
  fi
  printf '%s' "$root"
}

echo "== §5 release gate drill =="

# A FAILING regression record REFUSES the release (exit 1) — the one thing a release gate must never miss.
red_root="$(fixture '{"outcome":"fail","overall_pass":false,"overall_candidate":3.1}')"
check "a FAILING regression record REFUSES (exit 1)" 1 "$red_root"
saysline "  ...and names it RED" '\[RED' "$red_root"

# A PASSING record does NOT certify on its own: the other conditions are still unevidenced, so the gate
# reports CANNOT CERTIFY (exit 3) rather than a green release. This is the anti-fail-safe case.
pass_root="$(fixture '{"outcome":"pass","overall_pass":true,"overall_candidate":4.4,"overall_delta":0.1}')"
check "a PASSING record alone does NOT certify (exit 3, not 0)" 3 "$pass_root"
saysline "  ...and the regression limb reads GREEN" 'GREEN.*§5.1 regression corpus' "$pass_root"

# INCONCLUSIVE is accepted for a MERGE (TG-500 under-powered rule) but must never certify a RELEASE.
inc_root="$(fixture '{"outcome":"inconclusive","overall_pass":true,"overall_candidate":4.0}')"
check "an INCONCLUSIVE record does not certify (exit 3)" 3 "$inc_root"
saysline "  ...and is BLOCKED, not GREEN" 'BLOCKED.*§5.1 regression corpus' "$inc_root"

# No record at all is BLOCKED, never a pass — absence of evidence is not evidence of a green release.
empty_root="$(fixture '')"
check "no regression record at all is BLOCKED (exit 3)" 3 "$empty_root"
saysline "  ...and says nothing was readable" 'no committed change-gate verdict' "$empty_root"

# A CORRUPT record is RED, not silently skipped.
bad_root="$(fixture 'not json at all')"
check "an unreadable record REFUSES (exit 1)" 1 "$bad_root"

# Missing provenance on a generated contract is RED (§5.5) — a hand-edited artifact must not ship.
prov_root="$(fixture '{"outcome":"pass","overall_pass":true}')"
printf 'x-tg:\n    source_hash: "abc"\n' > "$prov_root/docs/contracts/openapi.yaml"
check "a contract missing generated_at/coverage_scope REFUSES (exit 1)" 1 "$prov_root"

# A holdout dir with no regression/holdout pair is BLOCKED with a DIFFERENT reason than "no record at all" —
# a half-written record must not read as an absent one.
half_root="$(fixture '{"outcome":"pass","overall_pass":true}')"
mkdir -p "$half_root/eval/history/2026-01-02-holdout-cafe"
check "a holdout dir with no scorecard pair is BLOCKED (exit 3)" 3 "$half_root"
saysline "  ...and says the pair is missing" 'carries no regression.json' "$half_root"

# A complete holdout record is recognised (the gap check itself is delegated to tools/evalgate, skipped here).
full_root="$(fixture '{"outcome":"pass","overall_pass":true}')"
mkdir -p "$full_root/eval/history/2026-01-02-holdout-cafe"
echo '{}' > "$full_root/eval/history/2026-01-02-holdout-cafe/regression.json"
echo '{}' > "$full_root/eval/history/2026-01-02-holdout-cafe/holdout.json"
saysline "a complete holdout record is recognised, not reported absent" 'gap check skipped' "$full_root"

# The canary is ADVISORY: it appears in the report but is never counted as a certifying condition.
saysline "the canary is advisory, never a condition" 'ADVISORY.*§5.6' "$pass_root"

# ---- §5.4 judge-calibration arms (TG-5, 2026-08-25): the gate reads the newest COMMITTED judgecal record.
cal_fixture() { # cal_fixture <calibration-json-or-empty> -> prints a root with a judgecal dir
  local body="$1" root
  root="$(fixture '{"outcome":"pass","overall_pass":true}')"
  if [ -n "$body" ]; then
    mkdir -p "$root/eval/history/2026-08-25-judgecal"
    printf '%s' "$body" > "$root/eval/history/2026-08-25-judgecal/calibration.json"
  fi
  printf '%s' "$root"
}
now_iso="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# Floors met at the lower bound ⇒ GREEN (the other legs stay blocked, so rc stays 3 — GREEN is asserted by line).
meets_root="$(cal_fixture '{"generated_at":"'"$now_iso"'","bar":0.70,"confusion":{"tp":40,"fp":3,"tn":38,"fn":4},"tpr":{"Value":0.909,"Lo":0.801,"Hi":0.96,"N":44,"Defined":true},"tnr":{"Value":0.927,"Lo":0.805,"Hi":0.97,"N":41,"Defined":true},"meets_bar":true}')"
check "a floors-meeting calibration record does not alone certify (rc 3)" 3 "$meets_root"
saysline "  ...and the calibration limb reads GREEN" 'GREEN.*§5.4' "$meets_root"

# Floors failing ⇒ RED (exit 1): a judge below the bar refuses the release.
fail_root="$(cal_fixture '{"generated_at":"'"$now_iso"'","bar":0.70,"confusion":{"tp":10,"fp":30,"tn":5,"fn":20},"tpr":{"Value":0.33,"Lo":0.2,"Hi":0.5,"N":30,"Defined":true},"tnr":{"Value":0.14,"Lo":0.06,"Hi":0.3,"N":35,"Defined":true},"meets_bar":false,"bar_reason":"TPR lower bound 0.20 < 0.70"}')"
check "calibration floors failing REFUSES (exit 1)" 1 "$fail_root"

# n=0 is UNCALIBRATED — its own BLOCKED state, never a pass and never a plain fail.
uncal_root="$(cal_fixture '{"generated_at":"'"$now_iso"'","bar":0.70,"confusion":{"tp":0,"fp":0,"tn":0,"fn":0},"tpr":{},"tnr":{},"meets_bar":false,"bar_reason":"no labelled items"}')"
check "an n=0 record is BLOCKED, not RED (exit 3)" 3 "$uncal_root"
saysline "  ...and says UNCALIBRATED" 'UNCALIBRATED' "$uncal_root"

# A stale record is BLOCKED with the re-run instruction — old evidence must not certify today's judge.
stale_root="$(cal_fixture '{"generated_at":"2026-01-01T00:00:00Z","bar":0.70,"confusion":{"tp":40,"fp":3,"tn":38,"fn":4},"tpr":{"Value":0.9,"Lo":0.8,"Hi":0.96,"N":44,"Defined":true},"tnr":{"Value":0.9,"Lo":0.8,"Hi":0.97,"N":41,"Defined":true},"meets_bar":true}')"
check "a STALE meets-bar record is BLOCKED (exit 3)" 3 "$stale_root"
saysline "  ...and says to re-run judgecal" 're-run judgecal' "$stale_root"

# EMPTY-INPUT MUTATION ARM: an unreadable record REFUSES (exit 1), never certifies.
badcal_root="$(cal_fixture 'not json')"
check "an unreadable calibration record REFUSES (exit 1)" 1 "$badcal_root"

# DECOY ARM (the review's demonstrated false-GREEN): a -judgecal-suffixed decoy dir sorting after the
# canonical one must NOT be selected — the anchored glob reads the real record (which fails) and REFUSES.
decoy_root="$(cal_fixture '{"generated_at":"'"$now_iso"'","bar":0.70,"confusion":{"tp":10,"fp":30,"tn":5,"fn":20},"tpr":{"Value":0.33,"Lo":0.2,"Hi":0.5,"N":30,"Defined":true},"tnr":{"Value":0.14,"Lo":0.06,"Hi":0.3,"N":35,"Defined":true},"meets_bar":false,"bar_reason":"floors not met"}')"
mkdir -p "$decoy_root/eval/history/2026-08-25-judgecal-zzz-scratch"
printf '%s' '{"generated_at":"'"$now_iso"'","bar":0.70,"confusion":{"tp":40,"fp":3,"tn":38,"fn":4},"tpr":{"Value":0.9,"Lo":0.8,"Hi":0.96,"N":44,"Defined":true},"tnr":{"Value":0.9,"Lo":0.8,"Hi":0.97,"N":41,"Defined":true},"meets_bar":true}' \
  > "$decoy_root/eval/history/2026-08-25-judgecal-zzz-scratch/calibration.json"
check "a passing DECOY dir cannot mask the real failing record (exit 1)" 1 "$decoy_root"

# BOUNDARY ARM: a record 14d+2h old is past the 14d bound — the seconds-based compare must call it STALE
# (integer-floor days read this as 14 and passed it).
if date -u -d '-14 days -2 hours' +%Y-%m-%dT%H:%M:%SZ >/dev/null 2>&1; then
  edge_iso="$(date -u -d '-14 days -2 hours' +%Y-%m-%dT%H:%M:%SZ)"
  edge_root="$(cal_fixture '{"generated_at":"'"$edge_iso"'","bar":0.70,"confusion":{"tp":40,"fp":3,"tn":38,"fn":4},"tpr":{"Value":0.9,"Lo":0.8,"Hi":0.96,"N":44,"Defined":true},"tnr":{"Value":0.9,"Lo":0.8,"Hi":0.97,"N":41,"Defined":true},"meets_bar":true}')"
  check "a 14d+2h record is STALE (boundary, exit 3)" 3 "$edge_root"
  saysline "  ...and names the stale age" 'STALE\|re-run judgecal' "$edge_root"
fi

# Absence keeps its own BLOCKED reason, distinct from unreadable and from stale.
saysline "no judgecal record is BLOCKED with the produce instruction" 'no committed judgecal record' "$pass_root"

# §5.2 stays visibly the owner-deferred ruling, never silently dropped.
saysline "the VISR deferral is stated, not hidden" 'OWNER-DEFERRED' "$pass_root"

if [ "$fails" -gt 0 ]; then
  echo "§5 release gate drill: FAIL ($fails)" >&2
  exit 1
fi
echo "§5 release gate drill: PASS"
