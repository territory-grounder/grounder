// Package governance wires Territory Grounder's two self-monitoring controls as Temporal Schedules:
// the governance-metrics worker (auto-demote repeat offenders) and the judge-liveness monitor. These
// replace the predecessor Cronicle jobs — run-history, retries, and dead-man detection are
// Temporal-native. The pure decision logic lives in core/governance; this file is the schedule +
// activity wiring only.
//
// Provenance: [F] spec/004 (BEH-4) · [R] paradigm-rule 7 (Temporal Schedules replace Cronicle),
// EXECUTION-PLAN P1-9 · [O] INV-19 (decisions land on the audit spine).
package governance

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	coregov "github.com/territory-grounder/grounder/core/governance"
	tg "github.com/territory-grounder/grounder/temporal"
)

// Schedule ids and cadences for the governance schedules.
const (
	GovernanceMetricsScheduleID  = "tg/sched/governance-metrics"
	JudgeLivenessScheduleID      = "tg/sched/judge-liveness"
	FrontierCrossCheckScheduleID = "tg/sched/frontier-crosscheck"
)

// Cadences. The judge-liveness monitor runs hourly (it is a cheap two-table read); the frontier
// cross-check runs every six hours because each run spends independent frontier-model calls, and the
// failure it catches is measured in days, never minutes.
var (
	GovernanceMetricsInterval  = 24 * time.Hour
	JudgeLivenessInterval      = time.Hour
	FrontierCrossCheckInterval = 6 * time.Hour
)

// Activities holds the governance activity implementations, closing over the injected core-decision
// collaborators so a worker (and the test env) can register real behavior. A nil collaborator means that
// monitor is NOT wired, and scheduleSpecs then refuses to register its schedule — see there for why an
// unwired schedule is worse than an absent one.
type Activities struct {
	Demoter    *coregov.Demoter
	Monitor    *coregov.JudgeLivenessMonitor
	CrossCheck *coregov.FrontierCrossCheckMonitor
}

// GovernanceMetricsResult is the serializable outcome of a demote run.
type GovernanceMetricsResult struct {
	Candidates int
	Demoted    int
}

// GovernanceMetricsActivity groups recent close-out incidents by tuple and demotes genuine repeat
// offenders (spec/004 REQ-301..304). incidents are supplied by the caller (read from the reconciler's
// close-out rows in production).
func (a *Activities) GovernanceMetricsActivity(ctx context.Context, incidents []coregov.Incident, now time.Time) (GovernanceMetricsResult, error) {
	counts := coregov.CountByTuple(incidents, now)
	candidates := 0
	for _, c := range counts {
		if coregov.IsDemoteCandidate(c) {
			candidates++
		}
	}
	rows, err := a.Demoter.Evaluate(ctx, counts, now)
	if err != nil {
		return GovernanceMetricsResult{}, err
	}
	return GovernanceMetricsResult{Candidates: candidates, Demoted: len(rows)}, nil
}

// JudgeLivenessActivity runs the judge-liveness monitor (spec/004 REQ-305/306) and, on a judge-death
// finding, forces the judged-accrual halt through the monitor's injected Halter (REQ-308).
func (a *Activities) JudgeLivenessActivity(ctx context.Context, now time.Time) (coregov.LivenessResult, error) {
	return a.Monitor.Run(ctx, now)
}

// FrontierCrossCheckActivity runs the model-INDEPENDENT anchor (spec/004 REQ-307): an independent
// re-judgment over the same rubric catches both a drifting judge (which liveness reads as healthy) and a
// confirmed-dead one (sessions the frontier proves were judgeable and the local judge scored not at all).
// A confirmed death halts judged accrual through the monitor's Halter.
func (a *Activities) FrontierCrossCheckActivity(ctx context.Context) (coregov.CrossCheckResult, error) {
	return a.CrossCheck.Run(ctx)
}

// ScheduleSpec is one governance schedule: its id, the BARE name of the workflow function that runs it, and
// its cadence. Kept as data so the boot wiring, the oracle, and the CI dead-man all read the same list
// rather than three drifting copies.
type ScheduleSpec struct {
	ID       string
	Workflow string
	Every    time.Duration
}

// scheduleSpecs returns the schedules whose collaborators are ACTUALLY wired.
//
// This gate is the lesson of the finding that produced this file's repair (PORT-FIDELITY-AUDIT #15): a
// schedule pointing at a workflow that does not exist, or at an activity whose collaborator is nil, is worse
// than no schedule at all — it manufactures a permanently-failing run history, and an alarm that is always
// red trains an operator to stop reading governance alarms, which is how the real one gets missed.
//
// GovernanceMetricsWorkflow is deliberately ABSENT: the demote worker (finding #5 / TG-219) has no live
// incident source, so its schedule would fire into a guaranteed failure. It rejoins this list in the same
// change that gives it one — and the CI dead-man (eval/ci/check-governance-schedules.sh) proves the two
// schedules that ARE listed here stay wired at boot.
func (a *Activities) scheduleSpecs() []ScheduleSpec {
	var out []ScheduleSpec
	if a != nil && a.Monitor != nil {
		out = append(out, ScheduleSpec{JudgeLivenessScheduleID, "JudgeLivenessWorkflow", JudgeLivenessInterval})
	}
	if a != nil && a.CrossCheck != nil {
		out = append(out, ScheduleSpec{FrontierCrossCheckScheduleID, "FrontierCrossCheckWorkflow", FrontierCrossCheckInterval})
	}
	return out
}

// Specs exposes the wired schedule set so the worker can log it and an oracle can assert it.
func (a *Activities) Specs() []ScheduleSpec { return a.scheduleSpecs() }

// CreateSchedules registers the wired governance Temporal Schedules and is genuinely IDEMPOTENT: an
// already-existing schedule (ErrScheduleAlreadyRunning) is skipped, not treated as fatal, so a
// call-on-every-startup reconcile never crash-loops AND never aborts before ensuring the LATER schedule. The
// naive "return on first error" shape silently dropped the judge-liveness dead-man schedule whenever an
// earlier schedule already existed (a partial create, or an operator deleting one).
func CreateSchedules(ctx context.Context, sc client.ScheduleClient, specs []ScheduleSpec) error {
	return createSchedules(specs, func(opts client.ScheduleOptions) error {
		_, err := sc.Create(ctx, opts)
		return err
	})
}

// createSchedules is the pure idempotent loop over the create seam (tested without a full ScheduleClient).
//
// The action's TaskQueue is the RUNNER queue, not tg.TaskQueueSchedule: nothing in this system polls
// tg.schedule — the sole worker.New in the tree listens on tg.runner — so a schedule pointing there would
// enqueue workflow tasks no worker ever takes. That is a dead schedule wearing the appearance of a live one,
// which is the same defect class this whole repair closes, one layer down.
func createSchedules(specs []ScheduleSpec, create func(client.ScheduleOptions) error) error {
	for _, s := range specs {
		err := create(client.ScheduleOptions{
			ID:   s.ID,
			Spec: client.ScheduleSpec{Intervals: []client.ScheduleIntervalSpec{{Every: s.Every}}},
			Action: &client.ScheduleWorkflowAction{
				ID:        s.ID + "-wf",
				Workflow:  s.Workflow,
				TaskQueue: tg.TaskQueueRunner,
			},
		})
		// An already-registered schedule is not an error — reconcile continues to the NEXT schedule so a
		// partial state (only the first exists) still (re)creates the remaining dead-man monitors.
		if err != nil && !errors.Is(err, temporal.ErrScheduleAlreadyRunning) {
			return err
		}
	}
	return nil
}
