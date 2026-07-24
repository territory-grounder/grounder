package runner

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// budget.go — the Runner session's two runaway bounds (design-wisdom #8 · Gulli ch12 · Anthropic 6.4):
// a BOUNDED activity RetryPolicy so a failing activity can never retry forever, and a workflow-level
// wall-clock BUDGET so a session can never park past a hard total-time ceiling. Both stop the session to
// a clean terminal outcome (a failed session a human reconciles / an orphaned-poll hand-off), never a
// crash, and neither actuates the estate — mutation stays OFF.

// runnerNonRetryable names DETERMINISTIC activity error TYPES — a retry can never turn them green (a
// malformed/unparseable payload, a schema/validation reject). Listing them short-circuits the bounded
// retry so a poison activity surfaces on the FIRST failure instead of burning every attempt (a bounded
// retry must not turn a poison activity into an N-attempt loop). The base read-only pipeline activities
// today return plain transient errors (none of these types), so this changes nothing for them today — it
// is the forward seam: an activity that returns one of these as a typed ApplicationError is never retried.
var runnerNonRetryable = []string{"InvalidInput", "ValidationError"}

// BaseActivityMaxAttempts bounds the base read-only pipeline activities — a failing activity is attempted
// at most this many times, then the failure surfaces (never Temporal's unbounded default). Exported so
// the acceptance oracle asserts the bound rather than a hard-coded literal.
const BaseActivityMaxAttempts = 4

// InvestigateMaxAttempts bounds the read-only investigate activity — ONE retry over a transient blip.
const InvestigateMaxAttempts = 2

// runnerActivityOptions is the BASE ActivityOptions for the Runner's ordinary read-only pipeline
// activities (suppress / classify / gate / notify / record-pending / resolve-pending / record-triage /
// reconcile / verify). Temporal's DEFAULT activity RetryPolicy is UNBOUNDED (MaximumAttempts 0 ⇒ retry
// forever with exponential backoff); the Runner base options previously set only a StartToCloseTimeout,
// so every one of these activities inherited that unbounded default. A persistently-failing activity
// would then retry forever and either pin the session open indefinitely (a `.Get` that never returns) or,
// for the best-effort record/reconcile activities whose error is discarded, hang the session on a `.Get`
// that never resolves. This bounds it: a few attempts with capped exponential backoff, then the failure
// SURFACES and the workflow ends deterministically (a failed session a human reconciles) or the discarded
// best-effort `.Get` returns and the session proceeds — never an unbounded loop. These base activities are
// idempotent reads/records (never an estate mutation), so a bounded retry over a transient blip is safe.
//
// The two HAZARDOUS activity classes keep their OWN tighter policy at their call site and are NOT governed
// by this base policy (see workflow.go): the human-vote RECORD and the estate EXECUTE each run at
// MaximumAttempts=1 (at-least-once delivery + one human approval must never double-append the ledger nor
// execute the estate twice); the read-only INVESTIGATE keeps its longer StartToCloseTimeout with a single
// bounded retry. So poison never loops in any class: base ≤ 4, investigate ≤ 2, record/execute = 1.
func runnerActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        time.Second,
			BackoffCoefficient:     2.0,
			MaximumInterval:        30 * time.Second,
			MaximumAttempts:        BaseActivityMaxAttempts,
			NonRetryableErrorTypes: runnerNonRetryable,
		},
	}
}

// investigateRetryPolicy bounds the read-only agent loop activity: ONE retry over a transient model/tool
// blip (the loop is read-only — INV-21 — so a single re-run is safe, unlike execute), capped backoff, and
// the shared deterministic-error short-circuit so a poison input is not retried at all. It keeps the loop
// activity's long StartToCloseTimeout (set at the call site) so a legitimate multi-cycle investigation is
// not truncated.
func investigateRetryPolicy() *temporal.RetryPolicy {
	return &temporal.RetryPolicy{
		InitialInterval:        time.Second,
		BackoffCoefficient:     2.0,
		MaximumInterval:        30 * time.Second,
		MaximumAttempts:        InvestigateMaxAttempts,
		NonRetryableErrorTypes: runnerNonRetryable,
	}
}

// WorkflowWallClockBudget is the cumulative wall-clock CEILING for a single Runner session, measured from
// workflow start (workflow.Now deltas). It is a defense-in-depth BACKSTOP against a runaway session
// burning unbounded time: the bounded activity retries above cap COMPUTE time, and this budget caps the
// one place a session parks for a long span — the durable human-vote wait (VoteWait). The Runner races a
// budget deadline against that wait; WHEN the session's total wall-clock budget is exhausted before a
// decision arrives, the workflow STOPS to the SAME terminal orphaned-poll hand-off a timed-out poll uses
// (stand down fail-closed, record, hand the incident to the escalation re-check lane), never a crash,
// mutation stays OFF.
//
// It is set to VoteWait (24h) + a compute headroom (2h) so a legitimately slow human decision WITHIN the
// 24h poll window is NEVER cut short — the budget bites only a session whose TOTAL wall-clock exceeds a
// full day plus headroom, which the bounded compute + one vote wait cannot legitimately reach (so in
// production it fires only if the compute bound is somehow defeated — a future added loop or a
// misconfigured timeout — which is exactly what a backstop is for).
//
// It is a package var (not a const) SOLELY so the acceptance oracle can inject a short ceiling and drive
// the budget-exceeded path deterministically in the Temporal in-process env, whose mock clock only
// advances on a timer the workflow awaits — the vote wait is that lever. Production never rebinds it.
var WorkflowWallClockBudget = 26 * time.Hour

// budgetRemaining is the wall-clock left before the session budget is exhausted, floored at zero — the
// duration of the budget-deadline timer the Runner races against the human-vote wait. It is computed from
// workflow.Now (deterministic on replay), so the timer duration is deterministic. A zero remaining means
// the session is already over budget and the deadline fires immediately.
func budgetRemaining(ctx workflow.Context, start time.Time) time.Duration {
	rem := WorkflowWallClockBudget - workflow.Now(ctx).Sub(start)
	if rem < 0 {
		return 0
	}
	return rem
}
