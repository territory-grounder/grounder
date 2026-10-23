package runner

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/observe"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// TG-380 slice 5 — the gate stage's acted is `out.Executed` and eligible is `!out.RateLimited`, both read from
// the interceptor Outcome at the UNPROTECTED caller (ExecuteActivity). The generic producer-scan guard only
// asserts offered!=0 + the subset invariant, so this pins the acted classification directly: a still-faulted
// host is healed (executes → acted=1); a host that healed before execute is refused at the necessity gate (not
// executed → acted=0). Both are eligible (neither is a rate-limit short-circuit).
func TestExecuteActivityBooksGateStage(t *testing.T) {
	// gateExercise drives ONE ExecuteActivity against a fully-grounded sealed restart with the given clear-check
	// wiring (which decides execute-vs-refuse) and returns the gate tally snapshot.
	gateExercise := func(t *testing.T, clear func(context.Context, string, string) ([]verify.ObservedAlert, bool)) (offered, eligible, acted int64) {
		t.Helper()
		ctx := context.Background()
		choke := safety.NewActuatingChokepoint() // mutation ON (test-only)
		m := unitManifest(t)
		sink := &fakeManifestSink{}
		if err := sink.Seal(ctx, m); err != nil {
			t.Fatalf("seal: %v", err)
		}
		tally := observe.NewStageTally()
		deps := Deps{
			Stages:           tally,
			Interceptor:      withPermissivePolicy(actuate.NewInterceptor(choke, &recordingActuator{}, audit.NewLedger())),
			Manifests:        sink,
			Mutation:         choke,
			PostStateObserve: func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return []verify.ObservedAlert{}, true },
			ClearObserve:     clear,
		}
		if _, err := NewActivities(deps).ExecuteActivity(ctx, ExecuteInput{
			ActionID: m.ActionID, ExternalRef: "tg380-gate", PlanHash: "plan#g", TargetHost: "web01", Site: "nl",
			Band:        safety.BandAuto,
			EvidenceIDs: []string{"tr-1"},
			ToolResults: []agent.ToolResult{{ID: "tr-1", Target: "web01", Output: "web01 nginx is failed", Success: true}},
		}); err != nil {
			t.Fatalf("ExecuteActivity must be a recorded refusal or an execution, never an error: %v", err)
		}
		return tally.Snapshot("gate")
	}

	t.Run("executed is offered eligible and acted", func(t *testing.T) {
		// The host is STILL faulted at execute ⇒ the necessity gate passes and the action executes.
		off, elig, acted := gateExercise(t, func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{{Host: "web01", Rule: "NginxDown", Site: "nl"}}, true
		})
		if off != 1 || elig != 1 || acted != 1 {
			t.Errorf("executed path offered/eligible/acted = %d/%d/%d, want 1/1/1", off, elig, acted)
		}
	})

	t.Run("refused is offered and eligible but not acted", func(t *testing.T) {
		// The host healed before execute ⇒ the necessity gate refuses; offered + eligible, but not acted. This is
		// the classification that would silently invert if acted were booked as anything but out.Executed.
		off, elig, acted := gateExercise(t, func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			return nil, true
		})
		if off != 1 || elig != 1 || acted != 0 {
			t.Errorf("refused path offered/eligible/acted = %d/%d/%d, want 1/1/0", off, elig, acted)
		}
	})
}
