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

// Schedule ids and cadences for the two governance schedules.
const (
	GovernanceMetricsScheduleID = "tg/sched/governance-metrics"
	JudgeLivenessScheduleID     = "tg/sched/judge-liveness"
)

// Cadences. The demote worker runs daily; the judge-liveness monitor hourly.
var (
	GovernanceMetricsInterval = 24 * time.Hour
	JudgeLivenessInterval     = time.Hour
)

// Activities holds the governance activity implementations, closing over the injected core-decision
// collaborators so a worker (and the test env) can register real behavior.
type Activities struct {
	Demoter *coregov.Demoter
	Monitor *coregov.JudgeLivenessMonitor
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

// JudgeLivenessActivity runs the judge-liveness monitor (spec/004 REQ-305/306).
func (a *Activities) JudgeLivenessActivity(ctx context.Context, now time.Time) (coregov.LivenessResult, error) {
	return a.Monitor.Run(ctx, now)
}

// CreateSchedules registers the two governance Temporal Schedules and is genuinely IDEMPOTENT: an
// already-existing schedule (ErrScheduleAlreadyRunning) is skipped, not treated as fatal, so a
// call-on-every-startup reconcile never crash-loops AND never aborts before ensuring the LATER schedule. The
// naive "return on first error" shape silently dropped the judge-liveness dead-man schedule whenever
// governance-metrics already existed (a partial create, or an operator deleting one). The workflows the
// schedules trigger run the activities above on the schedule task queue.
func CreateSchedules(ctx context.Context, sc client.ScheduleClient) error {
	return createSchedules(func(opts client.ScheduleOptions) error {
		_, err := sc.Create(ctx, opts)
		return err
	})
}

// createSchedules is the pure idempotent loop over the create seam (tested without a full ScheduleClient).
func createSchedules(create func(client.ScheduleOptions) error) error {
	specs := []struct {
		id       string
		workflow string
		every    time.Duration
	}{
		{GovernanceMetricsScheduleID, "GovernanceMetricsWorkflow", GovernanceMetricsInterval},
		{JudgeLivenessScheduleID, "JudgeLivenessWorkflow", JudgeLivenessInterval},
	}
	for _, s := range specs {
		err := create(client.ScheduleOptions{
			ID:   s.id,
			Spec: client.ScheduleSpec{Intervals: []client.ScheduleIntervalSpec{{Every: s.every}}},
			Action: &client.ScheduleWorkflowAction{
				ID:        s.id + "-wf",
				Workflow:  s.workflow,
				TaskQueue: tg.TaskQueueSchedule,
			},
		})
		// An already-registered schedule is not an error — reconcile continues to the NEXT schedule so a
		// partial state (only the first exists) still (re)creates the judge-liveness dead-man monitor.
		if err != nil && !errors.Is(err, temporal.ErrScheduleAlreadyRunning) {
			return err
		}
	}
	return nil
}
