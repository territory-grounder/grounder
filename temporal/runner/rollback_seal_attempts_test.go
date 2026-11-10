package runner

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/sdk/temporal"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/temporal/wfauth"
	"github.com/territory-grounder/grounder/temporal/wfauth/wfauthtest"
)

// TG-660 review S-A: the rollback seal claims the start envelope's one-time nonce, so a retried seal could only be
// refused as a replay — a transient failure would burn a legitimate rollback, and a forgery would be ledgered once
// per attempt. The seal therefore gets EXACTLY ONE attempt, like every admin-write activity.
// KILLING MUTATION: return runnerActivityOptions() from rollbackSealOptions — RED here.
func TestTheRollbackSealGetsExactlyOneAttempt(t *testing.T) {
	o := rollbackSealOptions()
	if o.RetryPolicy == nil || o.RetryPolicy.MaximumAttempts != 1 {
		t.Fatalf("the nonce-claiming seal must run exactly once, got %+v", o.RetryPolicy)
	}
}

// A workflow-auth refusal is deterministic, so it is NON-RETRYABLE whatever the caller's policy, and it still
// unwraps to the *wfauth.Refusal the callers and tests read. KILLING MUTATION: return the bare refusal — RED here.
func TestARollbackSealRefusalIsNonRetryable(t *testing.T) {
	_, v := wfauthtest.Pair(t)
	d := permissive(Deps{ManifestSink: &captureSink{}, Ledger: audit.NewLedger().WithSink(&captureLedger{})})
	d.WorkflowAuth = v
	_, err := (&Activities{D: d}).SealRollbackActivity(context.Background(), RollbackInput{
		ForwardActionID: "forward-s-a", ForwardOpClass: "start-service", ForwardOp: "start", ForwardTarget: "app01",
		ForwardParams: map[string]string{"unit": "nginx"}, ForwardReversible: true, RollbackExternalRef: "rollback:s-a", Operator: "op",
	})
	var app *temporal.ApplicationError
	if !errors.As(err, &app) || !app.NonRetryable() {
		t.Fatalf("an unsigned seal must fail NON-retryable, got %v", err)
	}
	var ref *wfauth.Refusal
	if !errors.As(err, &ref) || ref.Class != "unsigned" {
		t.Fatalf("the refusal must still unwrap to *wfauth.Refusal, got %v", err)
	}
}
