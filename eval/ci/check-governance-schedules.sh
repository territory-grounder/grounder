#!/usr/bin/env bash
# Dead-man for the DEAD-MAN: the governance monitors' own absence must be detectable (TG-222, spec/004
# REQ-308). Same shape as eval/ci/check-baseline-freshness.sh — a CI gate that fails CLOSED, not a log line.
#
# PORT-FIDELITY-AUDIT #15 is the incident this makes impossible to repeat. Judge-liveness and the frontier
# cross-check were code-complete, oracle-tested, lockstep-clean — and DEAD: no constructor, no caller, and
# schedule workflows named only as string literals no Go function matched. Every green signal the project had
# stayed green while the control did nothing, because "it compiles and its unit tests pass" and "it runs in
# production" are different claims and only the first one was being checked.
#
# The judge-liveness monitor watches the judge. The frontier cross-check watches the judge independently.
# NOTHING watched whether either of them was wired. This does. It asserts, from the source of truth in the
# tree, that (1) each schedule spec names a workflow function that EXISTS, (2) the worker's boot path CALLS
# the arming function, (3) the arming registers each workflow, and (4) the judged-accrual halt is actually
# consulted at the graduation choke point. Delete any one of those and this gate goes red.
#
# It is a SOURCE gate, not a runtime probe, deliberately: the failure being guarded is "the wiring left the
# tree", which is a property of the tree. Runtime health is the monitors' own job — this guards the monitors.
#
# Fails CLOSED on a missing file. Test hook: GOV_ROOT overrides the repo root.
set -uo pipefail
cd "$(dirname "$0")/../.."
root="${GOV_ROOT:-.}"

schedule_go="$root/temporal/governance/schedule.go"
workflows_go="$root/temporal/governance/workflows.go"
wiring_go="$root/cmd/worker/governance.go"
main_go="$root/cmd/worker/main.go"
finalizer_go="$root/temporal/skilltrial/finalizer.go"

fail=0
say_fail() { echo "  FAIL: $*"; fail=1; }

echo "== governance-schedule dead-man =="

for f in "$schedule_go" "$workflows_go" "$wiring_go" "$main_go" "$finalizer_go"; do
  if [ ! -f "$f" ]; then
    echo "  FAIL: $f does not exist — the governance monitors have no wiring at all."
    echo "governance-schedules: FAIL"
    exit 1
  fi
done

# (1) Every workflow NAME a schedule spec registers must be a real function in the same package. This is the
#     literal finding: two schedules named workflows that were defined nowhere.
names="$(sed -n 's/.*ScheduleSpec{[^,]*, *"\([A-Za-z0-9_]*\)".*/\1/p' "$schedule_go" | sort -u)"
if [ -z "$names" ]; then
  say_fail "no schedule spec is declared in $schedule_go — no governance monitor is scheduled, so a judge that"
  echo "        dies mid-campaign silently invalidates the comparison window."
fi
for n in $names; do
  if ! grep -q "^func ${n}(" "$workflows_go"; then
    say_fail "schedule spec names workflow ${n}, but no 'func ${n}(' exists in $workflows_go — the schedule"
    echo "        would fire into a workflow task nothing can execute."
  fi
  if ! grep -q "RegisterWorkflow(tggov\.${n})" "$wiring_go"; then
    say_fail "workflow ${n} is scheduled but never registered on the worker in $wiring_go — an unregistered"
    echo "        workflow name is a schedule that can never run."
  fi
done

# (1b) The arming must actually CREATE the schedules. Registering the workflows and never creating the
#      schedule is the same dead capability with a more convincing surface.
if ! grep -q "CreateSchedules(" "$wiring_go"; then
  say_fail "the arming in $wiring_go registers workflows but never creates their schedules — the monitors"
  echo "        would be registered and never triggered."
fi

# (2) The boot path must CALL the arming. A wiring function nothing invokes is the same dead capability one
#     layer up, and it is the shape this finding took the first time.
if ! grep -q "armGovernanceSchedules(" "$main_go"; then
  say_fail "cmd/worker/main.go never calls armGovernanceSchedules — the monitors have a constructor and no caller."
fi
if ! grep -q "JudgeLivenessMonitor{" "$main_go"; then
  say_fail "cmd/worker/main.go never constructs a JudgeLivenessMonitor — judge death is unmeasured in production."
fi

# (3) The judged-accrual HALT must be armed and consulted. Detection that stops nothing is a warning nobody
#     reads, which is exactly what let a judge stay dead for three weeks.
if ! grep -q "NewJudgeDeadMan(" "$main_go"; then
  say_fail "cmd/worker/main.go never arms the judge-death dead-man — a confirmed dead judge would halt nothing."
fi
if ! grep -q "JudgeHealth" "$finalizer_go"; then
  say_fail "the skill-trial finalizer does not consult the judged-accrual halt — skills would graduate on the"
  echo "        scores of a judge already proven dead."
fi

if [ "$fail" != 0 ]; then
  echo "  Do NOT silence this by deleting the check: an unwatched governance monitor is how a dead judge"
  echo "  invalidated three weeks of measurement without anyone noticing. Restore the wiring instead."
  echo "governance-schedules: FAIL"
  exit 1
fi
echo "  wired: $(echo "$names" | tr '\n' ' ')— each defined, registered, armed at boot, and halting accrual."
echo "governance-schedules: PASS"
