package main

// The BOOT WIRING for the governance monitors (spec/004 REQ-307/REQ-308, TG-222).
//
// PORT-FIDELITY-AUDIT finding #15: judge-liveness and frontier cross-check were code-complete with no
// constructor, no caller, and schedule workflows defined nowhere — so the three-week dead-judge class the
// predecessor lived through was undetectable here. This file is the constructor and the caller.
//
// It is deliberately a SEPARATE, SMALL FILE with the arming logic behind one function, because an oracle has
// to be able to prove the arming happened. Wiring buried inline in a 4,000-line main() is provable only by
// reading it, and "someone read it" is the assurance level this whole audit exists to replace.

import (
	"context"
	"fmt"
	"log"

	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/core/audit"
	tggov "github.com/territory-grounder/grounder/temporal/governance"
)

// scheduleCreator is the slice of client.ScheduleClient the arming needs, so an oracle drives the REAL
// arming function with a recording fake instead of a live Temporal server.
type scheduleCreator interface {
	Create(ctx context.Context, options client.ScheduleOptions) (client.ScheduleHandle, error)
}

// workflowRegistrar is the slice of worker.Worker the arming needs (same reason).
type workflowRegistrar interface {
	RegisterWorkflow(w any)
	RegisterActivity(a any)
}

// armGovernanceSchedules registers the governance workflows + activities on the worker and creates their
// Temporal Schedules. It is the SINGLE place both happen, so the two can never drift into the state the
// finding describes — a schedule with no workflow, or a workflow with no schedule.
//
// It is NON-FATAL by design, exactly like the other cron arms in main(): a Temporal hiccup at boot must not
// stop a worker whose primary job is triage. What makes that safe is that the ABSENCE is detectable — the CI
// dead-man eval/ci/check-governance-schedules.sh fails when this wiring is removed from the tree, and a
// schedule that fails to create is logged at boot with what stops working as a result.
func armGovernanceSchedules(ctx context.Context, sc scheduleCreator, w workflowRegistrar, acts *tggov.Activities, logf func(string, ...any)) error {
	if acts == nil {
		return fmt.Errorf("governance schedules: nil activities — nothing to arm")
	}
	specs := acts.Specs()
	if len(specs) == 0 {
		return fmt.Errorf("governance schedules: no monitor is wired — judge death would be UNDETECTABLE")
	}
	// Register BEFORE creating: a schedule that fires against an unregistered workflow name produces a task
	// nothing can execute, so the registration must already stand when the first action lands.
	if acts.Monitor != nil {
		w.RegisterWorkflow(tggov.JudgeLivenessWorkflow)
		w.RegisterActivity(acts.JudgeLivenessActivity)
	}
	if acts.CrossCheck != nil {
		w.RegisterWorkflow(tggov.FrontierCrossCheckWorkflow)
		w.RegisterActivity(acts.FrontierCrossCheckActivity)
	}
	if err := tggov.CreateSchedules(ctx, scheduleClientFunc(sc.Create), specs); err != nil {
		return err
	}
	for _, s := range specs {
		logf("governance schedule armed: %s → %s every %s", s.ID, s.Workflow, s.Every)
	}
	return nil
}

// scheduleClientFunc adapts the narrow creator to the SDK's ScheduleClient shape that CreateSchedules takes.
// Only Create is ever called; the remaining methods exist to satisfy the interface and panic loudly rather
// than silently doing nothing if a future caller reaches for one.
type scheduleClientFunc func(ctx context.Context, options client.ScheduleOptions) (client.ScheduleHandle, error)

func (f scheduleClientFunc) Create(ctx context.Context, options client.ScheduleOptions) (client.ScheduleHandle, error) {
	return f(ctx, options)
}
func (f scheduleClientFunc) GetHandle(context.Context, string) client.ScheduleHandle {
	panic("governance schedules: GetHandle is not part of the arming path")
}
func (f scheduleClientFunc) List(context.Context, client.ScheduleListOptions) (client.ScheduleListIterator, error) {
	panic("governance schedules: List is not part of the arming path")
}

// govEscalator routes a judge-death / judge-drift warning to a HUMAN through the two surfaces the worker
// already owns: the durable escalation queue (so the finding survives a restart and appears in the operator
// queue) and the notifier (so it pages now). It satisfies core/governance.Escalator.
//
// It fails OPEN on the notifier and CLOSED on the queue: a page that cannot be delivered must not discard
// the durable record, but losing BOTH surfaces is returned as an error so the monitor's run goes red rather
// than reporting a warning it never actually raised.
type govEscalator struct {
	enqueue func(ctx context.Context, ref, reason string) error
	notify  func(ctx context.Context, n notifier.Notice) error
}

func (g govEscalator) Warn(ctx context.Context, kind, detail string) error {
	ref := "governance/" + kind
	var queueErr error
	if g.enqueue != nil {
		queueErr = g.enqueue(ctx, ref, detail)
	}
	if g.notify != nil {
		if nerr := g.notify(ctx, notifier.Notice{
			DecisionID: ref,
			Body: "GOVERNANCE " + kind + ": " + detail +
				"\n\nJudged accrual is HALTED: no skill graduates on this judge's scores until the judge is " +
				"proven alive and an operator re-arms the judge-death dead-man.",
		}); nerr != nil && queueErr == nil {
			log.Printf("governance: %s notice delivery failed (%v) — the durable escalation row still stands", kind, nerr)
			return nil
		} else if nerr == nil {
			return nil
		}
	}
	return queueErr
}

// govHaltRecorder binds a judged-accrual halt to the governance ledger, so a stop is hash-chained like every
// other governance decision (INV-19) instead of living only in a log line nobody re-reads. It mirrors
// ledgerTripRecorder, the mutation breaker's equivalent.
type govHaltRecorder struct{ l *audit.Ledger }

func (r govHaltRecorder) RecordJudgeHalt(reason string) {
	if r.l == nil {
		return
	}
	if _, err := r.l.Append(audit.GovDecision{
		Decision: "governance:judge-death-halt",
		Reason:   reason,
		ActionID: "judge-death-halt",
		Withheld: true, // autonomy withheld — no graduation accrues on an unverified judge
	}); err != nil {
		log.Printf("governance: judged-accrual halt applied but ledger append failed: %v", err)
	}
}
