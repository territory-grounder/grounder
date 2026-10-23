package governance

// The workflow bodies the governance schedules trigger (spec/004 REQ-307/REQ-308, TG-222).
//
// Before this file the two schedule specs named "GovernanceMetricsWorkflow" and "JudgeLivenessWorkflow" as
// STRING LITERALS and no Go function of either name existed anywhere in the tree — the schedules could not
// have run even if something had created them, which nothing did. That is the finding verbatim: "no
// constructor, no caller, and schedule workflows defined nowhere".
//
// Each is DISTINCTLY NAMED because Temporal registers a workflow by its BARE function name (the collision
// guard lives in temporal/skilltrial/finalizer_names_test.go): a second exported `Workflow` boot-loops the
// worker.

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	coregov "github.com/territory-grounder/grounder/core/governance"
)

// JudgeLivenessWorkflow is the hourly judge-liveness body: one activity, workflow-time-stamped
// (workflow.Now — deterministic on replay). Two attempts, so a transient database read retries once and the
// next hour's run drains anything left.
//
// The activity's error is NOT swallowed: a monitor that cannot read its own denominator has not proved the
// judge is alive, and a green run for an unmeasurable judge is the precise falsehood the three-week outage
// was made of. A failed run is visible in the Temporal UI as a failed schedule action.
func JudgeLivenessWorkflow(ctx workflow.Context) (coregov.LivenessResult, error) {
	opts := workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 2},
	}
	var out coregov.LivenessResult
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts),
		new(Activities).JudgeLivenessActivity, workflow.Now(ctx).UTC()).Get(ctx, &out)
	return out, err
}

// FrontierCrossCheckWorkflow is the six-hourly independent-anchor body. Its timeout is sized for a sample of
// frontier-model re-judgments (each is a reasoning-model call, and the sampler batches several), matching
// the session judge's own generous activity budget rather than the two-minute database-read budget above.
// One attempt only: a re-judgment sample is expensive and the next scheduled run is six hours away, so a
// retry storm against a struggling model plane buys nothing the next run does not.
func FrontierCrossCheckWorkflow(ctx workflow.Context) (coregov.CrossCheckResult, error) {
	opts := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
	var out coregov.CrossCheckResult
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts),
		new(Activities).FrontierCrossCheckActivity).Get(ctx, &out)
	return out, err
}
