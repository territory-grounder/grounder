#!/usr/bin/env bash
# DRILL for scripts/check-merge-gate-setting.sh (TG-428). Same convention as scripts/lint-image-pins_test.sh:
# every verdict arm proven against the deterministic MERGE_GATE_JSON hook, rc AND message both asserted.
#
# KILLING MUTATION (executed 2026-08-10): make the gate treat a fetch failure as a pass — mutate blind()
# to announce 'merge gate: ON' and exit 0 — and the BLIND arms below go RED (want rc=3 got rc=0). That
# fail-open is the exact defect this witness exists to kill: a latch reported ON because nobody could
# look at it. Restore ⇒ green.
set -uo pipefail
cd "$(dirname "$0")/.."
G=scripts/check-merge-gate-setting.sh
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

echo "== merge-gate setting witness drill =="

# (1) The latch is ON ⇒ rc 0, and the verdict says it was checked LIVE (a cached or assumed ON is the lie
#     this witness exists to end).
MERGE_GATE_JSON='{"id":1,"path_with_namespace":"products/territory-grounder/grounder","only_allow_merge_if_pipeline_succeeds":true}' \
  check "setting true → merge gate ON" 0 "merge gate: ON" "checked live"

# (2) The latch is OFF ⇒ rc 1, and the message names BOTH the consequence and the fix.
MERGE_GATE_JSON='{"id":1,"path_with_namespace":"products/territory-grounder/grounder","only_allow_merge_if_pipeline_succeeds":false}' \
  check "setting false → MERGE GATE OFF" 1 "MERGE GATE OFF" "re-enable only_allow_merge_if_pipeline_succeeds"

# (3) THE VACUITY ARM (killing-mutation target): the fetch answered NOTHING. Unknown is not ON and it is
#     not OFF — it is rc 3 BLIND, its own exit code, distinct from a real OFF (rc 1).
MERGE_GATE_JSON='' \
  check "EMPTY project JSON → BLIND, never a verdict" 3 "MERGE GATE WITNESS BLIND" "cannot certify the setting"

# (4) A body without the field (a 200 from the wrong endpoint, a token seeing a redacted view) is just as
#     blind as no body — the absence of `false` must never be read as `true`.
MERGE_GATE_JSON='{"id":1,"path_with_namespace":"products/territory-grounder/grounder"}' \
  check "JSON missing the setting field → BLIND" 3 "MERGE GATE WITNESS BLIND"

# (5) No hook, no token of any kind ⇒ BLIND before any network call is attempted.
out5=$(env -u MERGE_GATE_JSON -u GITLAB_API_TOKEN -u TG_READONLY_API_TOKEN -u CI_JOB_TOKEN \
  bash "$G" 2>&1); rc5=$?
ran=$((ran + 1))
if [ "$rc5" -eq 3 ] && printf '%s' "$out5" | grep -qF "no API token"; then
  echo "  ok: no token at all → BLIND 'no API token' (rc=3)"
else
  echo "  FAIL: no token at all — want rc=3 + 'no API token', got rc=$rc5"
  printf '%s\n' "$out5" | sed 's/^/      /'
  fail=1
fi

# The drill's own vacuity floor.
if [ "$ran" -lt 5 ]; then
  echo "merge-gate setting witness drill: FAIL — only $ran assertion(s) ran; the drill itself is vacuous"
  exit 1
fi

if [ "$fail" -eq 0 ]; then
  echo "merge-gate setting witness drill: PASS ($ran assertions)"
else
  echo "merge-gate setting witness drill: FAIL"
fi
exit "$fail"
