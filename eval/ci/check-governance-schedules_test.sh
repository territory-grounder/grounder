#!/usr/bin/env bash
# Tests for the governance-schedule dead-man (style: eval/ci/check-baseline-freshness_test.sh). Drives the
# gate through its GOV_ROOT hook against synthetic trees, so no real source file is touched — and, crucially,
# proves the gate can FAIL. A dead-man that only ever passes is the defect it exists to catch.
set -uo pipefail
cd "$(dirname "$0")/../.."
gate=eval/ci/check-governance-schedules.sh
fail=0
check() { # desc want-rc
  local desc="$1" want="$2"
  bash "$gate" >/dev/null 2>&1; local rc=$?
  if [ "$rc" = "$want" ]; then echo "  ok: $desc (rc=$rc)"; else echo "  FAIL: $desc — want rc=$want got rc=$rc"; fail=1; fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# build_tree <dir> — a minimal, fully-wired synthetic tree the gate must PASS.
build_tree() {
  local d="$1"
  mkdir -p "$d/temporal/governance" "$d/cmd/worker" "$d/temporal/skilltrial"
  cat > "$d/temporal/governance/schedule.go" <<'EOF'
out = append(out, ScheduleSpec{JudgeLivenessScheduleID, "JudgeLivenessWorkflow", JudgeLivenessInterval})
out = append(out, ScheduleSpec{FrontierCrossCheckScheduleID, "FrontierCrossCheckWorkflow", FrontierCrossCheckInterval})
EOF
  cat > "$d/temporal/governance/workflows.go" <<'EOF'
func JudgeLivenessWorkflow(ctx workflow.Context) {}
func FrontierCrossCheckWorkflow(ctx workflow.Context) {}
EOF
  cat > "$d/cmd/worker/governance.go" <<'EOF'
w.RegisterWorkflow(tggov.JudgeLivenessWorkflow)
w.RegisterWorkflow(tggov.FrontierCrossCheckWorkflow)
tggov.CreateSchedules(ctx, scheduleClientFunc(sc.Create), specs)
EOF
  cat > "$d/cmd/worker/main.go" <<'EOF'
judgeDeadMan, _ := governance.NewJudgeDeadMan(breakerStore, rec)
govActs := &tggov.Activities{Monitor: &governance.JudgeLivenessMonitor{}}
armGovernanceSchedules(ctx, c.ScheduleClient(), w, govActs, log.Printf)
EOF
  cat > "$d/temporal/skilltrial/finalizer.go" <<'EOF'
JudgeHealth JudgeHealth
EOF
}

echo "== governance-schedule dead-man tests =="

build_tree "$tmp/good"
GOV_ROOT="$tmp/good" check "a fully-wired tree passes" 0

# THE FINDING ITSELF: a schedule naming a workflow that is defined nowhere.
build_tree "$tmp/no-workflow"
: > "$tmp/no-workflow/temporal/governance/workflows.go"
GOV_ROOT="$tmp/no-workflow" check "a schedule whose workflow is defined NOWHERE fails" 1

# The other half of the finding: a wiring function nothing calls.
build_tree "$tmp/no-caller"
sed -i '/armGovernanceSchedules/d' "$tmp/no-caller/cmd/worker/main.go"
GOV_ROOT="$tmp/no-caller" check "boot wiring that never CALLS the arming fails" 1

build_tree "$tmp/no-monitor"
sed -i '/JudgeLivenessMonitor{/d' "$tmp/no-monitor/cmd/worker/main.go"
GOV_ROOT="$tmp/no-monitor" check "a worker that never constructs the liveness monitor fails" 1

build_tree "$tmp/no-register"
: > "$tmp/no-register/cmd/worker/governance.go"
GOV_ROOT="$tmp/no-register" check "a scheduled workflow never registered on the worker fails" 1

build_tree "$tmp/registers-but-never-creates"
sed -i '/CreateSchedules(/d' "$tmp/registers-but-never-creates/cmd/worker/governance.go"
GOV_ROOT="$tmp/registers-but-never-creates" check "arming that registers workflows but creates no schedule fails" 1

build_tree "$tmp/no-deadman"
sed -i '/NewJudgeDeadMan(/d' "$tmp/no-deadman/cmd/worker/main.go"
GOV_ROOT="$tmp/no-deadman" check "detection with no ARMED halt fails (a warning nobody reads)" 1

build_tree "$tmp/no-accrual-gate"
: > "$tmp/no-accrual-gate/temporal/skilltrial/finalizer.go"
GOV_ROOT="$tmp/no-accrual-gate" check "a finalizer that never consults the halt fails" 1

build_tree "$tmp/no-specs"
: > "$tmp/no-specs/temporal/governance/schedule.go"
GOV_ROOT="$tmp/no-specs" check "a tree with NO schedule spec at all fails" 1

GOV_ROOT="$tmp/does-not-exist" check "a missing tree fails CLOSED" 1

# And the gate passes against the REAL tree it guards.
check "the real repository tree passes" 0

[ "$fail" = 0 ] && { echo "governance-schedule dead-man tests: PASS"; exit 0; } || { echo "governance-schedule dead-man tests: FAIL"; exit 1; }
