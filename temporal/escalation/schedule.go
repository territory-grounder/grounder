// Package escalation runs Territory Grounder's dropped-escalation requeue lane as a Temporal CRON
// workflow: on a fixed cadence it fires every DUE re-check in the escalation_queue through the
// authenticated re-entry signal (core/escalation.Controller.FireDue), so an enqueued escalation — an
// orphaned poll the reconciler requeued, or a judge-demotion escalation — actually fires / re-escalates /
// pages / stands down instead of sitting in the queue forever.
//
// This is the SCHEDULING half only: the fire/requeue/page/stand-down DECISION logic lives in
// core/escalation (spec/003, governed) and is driven UNCHANGED here. Same visible-scheduler-by-
// construction pattern as skilljudge/skilltrial — a Temporal cron shows last-run/next-run in the UI and a
// missed run is an alarmed workflow, not a quiet nothing.
//
// Provenance: [F] spec/003 (BEH-3, the reconcile requeue lane) wired into the worker · [R] Gulli ch12
// (recovery must be REACHABLE) · [O] INV-01/INV-12 (a re-check re-enters ONLY via the authenticated
// signal path, never a bare re-trigger). Nothing here actuates the estate — it pages humans and re-enters
// the gated pipeline; mutation stays OFF.
package escalation

import (
	"context"
	"log"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// CronSchedule is the FireDue cadence — every 5 minutes. An enqueued row fires ONCE (MarkFired transitions
// it out of pending), so the page rate is the ENQUEUE rate, not the cron rate — the cadence only bounds
// how long a due row waits, never a page storm.
const CronSchedule = "*/5 * * * *"

// WorkflowID is the singleton cron id.
const WorkflowID = "tg/escalation-firedue"

// Requeuer is the narrow slice of core/escalation.Controller this cron drives: fire every due re-check.
// It is an interface so the CI oracle drives the cron with an in-memory fake AND so this scheduling
// package never imports the governed core (no lockstep coupling). *escalation.Controller satisfies it.
type Requeuer interface {
	FireDue(ctx context.Context, now time.Time) (int, error)
}

// Deps are the worker-side collaborators. A nil Controller ⇒ the activity is a no-op (the cron is simply
// not armed in that configuration).
type Deps struct {
	Controller Requeuer
}

// Activities carries Deps for registration.
type Activities struct{ D Deps }

// Result is the serializable run summary — visible in the Temporal UI, so a persistently-erroring lane is
// observable rather than silent.
type Result struct {
	Fired   int    // due re-checks fired/re-escalated/paged/stood-down this run
	Errored bool   // FireDue reported a (per-row-isolated, non-fatal) error this run
	ErrText string // the joined error text when Errored (non-secret — external_refs + backend errors)
}

// FireDueActivity fires every due escalation re-check through the controller. It is FAIL-SAFE by
// construction: a FireDue error is CAPTURED into the Result and logged, NOT returned as an activity error
// — a stuck store or a paging outage must never fail the cron run, trigger a retry storm, or crash the
// worker. FireDue is itself internally BOUNDED (a failing MarkFired produces no page) and per-row ISOLATED
// (one poisoned incident never blocks the batch), and the cron cadence rate-caps throughput. Nothing here
// actuates the estate — it re-enters the gated pipeline via the authenticated signal and pages humans;
// mutation stays OFF.
func (a *Activities) FireDueActivity(ctx context.Context, now time.Time) (Result, error) {
	if a.D.Controller == nil {
		return Result{}, nil
	}
	fired, err := a.D.Controller.FireDue(ctx, now)
	res := Result{Fired: fired}
	if err != nil {
		res.Errored, res.ErrText = true, err.Error()
		log.Printf("escalation FireDue: %d fired, error: %v (best-effort — the worker is unaffected; retried at the next cron tick)", fired, err)
	} else if fired > 0 {
		log.Printf("escalation FireDue: %d due re-check(s) fired/re-escalated/paged (mutation OFF)", fired)
	}
	return res, nil
}

// FireDueWorkflow is the cron body — a DISTINCT registered name (Temporal registers by BARE function name,
// and a second exported `Workflow` boot-loops the worker; see skilltrial.FinalizerWorkflow). One
// workflow-time-stamped activity call (workflow.Now — deterministic on replay). The activity never returns
// an error, so the cron run always completes green and a genuine FireDue failure surfaces in the Result +
// the log, never as a crash. One attempt: the next cron tick drains whatever remained.
func FireDueWorkflow(ctx workflow.Context) (Result, error) {
	opts := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
	var res Result
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts),
		new(Activities).FireDueActivity, workflow.Now(ctx).UTC()).Get(ctx, &res)
	return res, err
}
