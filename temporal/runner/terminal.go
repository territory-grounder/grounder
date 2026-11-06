package runner

// The synthetic terminal frame (TG-81 borrow 1; clean-room from h-network's "always emit a terminal
// frame" runner rule, attribution: SOURCE-BENCHMARK-CATALOG). Before this wrapper, RunnerWorkflow had
// ~15 error returns that left NO session_triage row — the session existed only in Temporal history,
// invisible to the judge spine, the console and the eval denominator. A session that DIES now still
// leaves a durable terminal record saying it died and WHY (typed, two-tier: session-fatal classes vs
// the call-scoped failures the loop already absorbs and retries internally).

import (
	"errors"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/ingest"
)

// RunnerWorkflow runs one triage session (runSession) and guarantees a terminal triage frame: when the
// session exits by ERROR without having recorded its own row, a synthetic frame is written on a
// DISCONNECTED context (so a cancelled session can still record) before the error propagates. Clean
// no-record returns (suppressed, cluster-collapsed) are deliberate designs, not silent losses — the
// synthetic frame fires only on the error exits, where the loss was real. Version-guarded
// ("synthetic-terminal") so pre-feature histories replay byte-identically.
func RunnerWorkflow(ctx workflow.Context, env ingest.IncidentEnvelope) (RunnerResult, error) {
	res, err := runSession(ctx, env)
	if err != nil && !res.TriageRecorded &&
		workflow.GetVersion(ctx, "synthetic-terminal", workflow.DefaultVersion, 1) >= 1 {
		var a *Activities // nil receiver — activity-name resolution only, the runSession idiom
		// The session's own ctx may be cancelled or erroring — exactly the states this frame exists
		// for — so the record rides a disconnected child with the standard bounded retry options.
		dctx, cancel := workflow.NewDisconnectedContext(workflow.WithActivityOptions(ctx, runnerActivityOptions()))
		defer cancel()
		s := res
		s.Outcome = "error:" + terminalErrorClass(err)
		s.StopReason = "synthetic-terminal"
		recordTriage(dctx, a, env, s, "", "", nil)
	}
	return res, err
}

// terminalErrorClass types a session-ending error for the synthetic frame — the two-tier error model's
// session-fatal half. The call-scoped tier (a tool read failing, a retryable activity attempt) never
// reaches this function: the loop and the bounded retry policies absorb those, and only an error that
// actually KILLED the session is classified here. The vocabulary is closed and the fallback is honest:
// an unrecognized error is "session-fatal", not a guessed subclass.
func terminalErrorClass(err error) string {
	var (
		canceled *temporal.CanceledError
		timeout  *temporal.TimeoutError
		panicked *temporal.PanicError
		actErr   *temporal.ActivityError
	)
	switch {
	case errors.As(err, &canceled):
		return "session-fatal:cancelled"
	case errors.As(err, &timeout):
		return "session-fatal:timeout"
	case errors.As(err, &panicked):
		return "session-fatal:panic"
	case errors.As(err, &actErr):
		return "session-fatal:activity"
	default:
		return "session-fatal"
	}
}
