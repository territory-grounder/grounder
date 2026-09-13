package runner

import (
	"errors"
	"testing"

	"go.temporal.io/sdk/temporal"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/breaker"
)

// TG-400. Temporal matches RetryPolicy.NonRetryableErrorTypes on the ApplicationError TYPE, and derives
// that type from the Go type name of the returned error. Every model failure — rate_limit,
// provider_error and a breaker refusal alike — was a *ModelError, so all three serialised as "ModelError"
// and no list could single one out.
//
// Measured across all 159 session_triage rows for 2026-08-06, every Temporal history retrieved: 135 of 159
// investigations ran attempt 2; for 78 of them attempt 1 was breaker_open and attempt 2 was breaker_open
// again. With TG_MODEL_BREAKER_COOLDOWN=60s and a 1s initial interval, attempt 2 lands inside the open
// window with certainty. 134 of 135 ended RETRY_STATE_MAXIMUM_ATTEMPTS_REACHED.
//
// THE ORACLE USES THE SDK'S OWN CONVERTER rather than asserting a name I read out of the SDK source. TG
// pins sdk v1.34 and the derivation lives in an internal package; a test that hard-codes what I believe
// the rule to be would keep passing across an upgrade that changed it, which is precisely the class of
// silent drift this ticket is about.

// temporalErrorType asks the SDK what type name it would put on the wire for err.
func temporalErrorType(t *testing.T, err error) string {
	t.Helper()
	f := temporal.GetDefaultFailureConverter().ErrorToFailure(err)
	if f == nil {
		t.Fatalf("the SDK produced no failure for %T", err)
	}
	if ai := f.GetApplicationFailureInfo(); ai != nil {
		return ai.GetType()
	}
	t.Fatalf("the SDK did not classify %T as an application failure: %+v", err, f)
	return ""
}

// TestARefusalSerialisesUnderItsOwnType is the defect. If this name equals the one a provider failure
// gets, no NonRetryableErrorTypes entry can distinguish them.
func TestARefusalSerialisesUnderItsOwnType(t *testing.T) {
	refusal := model.NewBreakerRefusalForTest("model-primary", "primary")
	provider := model.NewModelErrorForTest("provider_error", "upstream 503")

	rt := temporalErrorType(t, refusal)
	pt := temporalErrorType(t, provider)

	if rt == pt {
		t.Fatalf("a deliberate breaker REFUSAL and a provider FAILURE both serialise as %q. Temporal "+
			"matches non-retryable on the type, so listing that name would make every model failure "+
			"non-retryable, and omitting it retries the refusal — which is what happened to 78 sessions.", rt)
	}
	if rt == "" {
		t.Fatal("the refusal carries no application-failure type at all, so it can never appear in a " +
			"NonRetryableErrorTypes list")
	}
}

// TestTheRefusalTypeIsListedNonRetryable closes the loop: having a distinct type is useless unless the
// policy names it. This reads the SDK's name and the deployed list, so a rename of either side fails here
// rather than silently restoring the retry.
func TestTheRefusalTypeIsListedNonRetryable(t *testing.T) {
	name := temporalErrorType(t, model.NewBreakerRefusalForTest("model-primary", "primary"))

	var listed bool
	for _, n := range runnerNonRetryable {
		if n == name {
			listed = true
		}
	}
	if !listed {
		t.Errorf("the SDK will serialise a breaker refusal as type %q, and runnerNonRetryable is %v — the "+
			"refusal is therefore RETRYABLE. Attempt 2 lands inside the breaker's cooldown window with "+
			"certainty, so the retry can only reproduce the same refusal.", name, runnerNonRetryable)
	}
}

// TestTheInvestigatePolicyCarriesTheList. The list is inert unless the policy that governs the retrying
// activity actually uses it — this repo's recurring shape is a correct value nothing consults.
func TestTheInvestigatePolicyCarriesTheList(t *testing.T) {
	p := investigateRetryPolicy()
	if p == nil {
		t.Fatal("investigateRetryPolicy returned nil")
	}
	if len(p.NonRetryableErrorTypes) != len(runnerNonRetryable) {
		t.Errorf("the investigate policy carries %v but runnerNonRetryable is %v — the list and the policy "+
			"have diverged, so the entries are documentation rather than behaviour",
			p.NonRetryableErrorTypes, runnerNonRetryable)
	}
}

// TestExistingCallersStillSeeAModelError guards the compatibility this change depends on. BreakerRefusal
// EMBEDS *ModelError so the metrics layer (which classifies on .Class) and the breaker sentinel check keep
// working; a replacement type rather than an embedding would silently reclassify every refusal as
// outcome="other" — the exact defect TG-369 was filed for.
func TestExistingCallersStillSeeAModelError(t *testing.T) {
	err := error(model.NewBreakerRefusalForTest("model-primary", "primary"))

	var me *model.ModelError
	if !errors.As(err, &me) {
		t.Fatal("errors.As no longer recovers the *ModelError from a refusal — the observability layer " +
			"classifies on ModelError.Class, so every refusal would fall back to the catch-all outcome")
	}
	if me.Class != model.ClassBreakerOpen {
		t.Errorf("recovered class %q, want %q", me.Class, model.ClassBreakerOpen)
	}
	if !errors.Is(err, breaker.ErrOpen) {
		t.Error("errors.Is(err, breaker.ErrOpen) no longer holds — callers that distinguish \"TG refused " +
			"to call\" from \"the provider failed\" would stop being able to")
	}
}
