package runner

// TG-665 (G4A F7), the commit-confirm chain with the REAL mutation breaker (pre-TG-665 drills stubbed it): a
// forward whose post-state DEVIATES trips the breaker at the interceptor, the breaker opens DURABLY and the
// owner-set mode stands, the armed window consults `deviation`, fires the compensating inverse through the FULL
// chain — which gate 4e admits exactly once as compensation-under-trip — and the inverse EXECUTES and verifies,
// so the window resolves `reverted`. Before TG-665 the trip force-Shadowed the mode and the inverse was refused
// at 4e/4f/the leaf: the compensation was lost on the one case it exists for.
//
// KILLING MUTATION (executed 2026-11-07): in safety.MutationBreaker.TripFor, delete the deviation branch (every
// cause forces Shadow — the pre-TG-665 shape). RED: TestDeviationFiresTheInverseThroughTheRealBreaker (the window
// resolves revert_failed: "chain refused the inverse"). Restored from a copy, green.

import (
	"context"
	"sync"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// strayAfterFirstExec is the post-state observer: quiet until the first effect executes, then an alert on a host
// the prediction never named (a real cascade — the DEVIATION trigger). The inverse's own baseline captures that
// alert as pre-existing, so the inverse verifies `match`.
func strayAfterFirstExec(act *recordingActuator) func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
	var mu sync.Mutex
	return func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		mu.Lock()
		defer mu.Unlock()
		if act.execs == 0 {
			return nil, true
		}
		return []verify.ObservedAlert{{Host: "stray01", Rule: "HostDown", Site: "dc1"}}, true
	}
}

func TestDeviationFiresTheInverseThroughTheRealBreaker(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	gate := safety.NewActuatingChokepoint()
	store := breaker.NewMemStore()
	mb, err := safety.NewMutationBreaker(gate, store, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	act := &recordingActuator{}
	sink := &fakeManifestSink{}
	cap := &ccT0292CaptureSink{}
	deps := testDeps(ccT0292ProposeScript()...)
	deps.Mutation = gate
	deps.Interceptor = withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger().WithSink(cap))).WithMutationBreaker(mb)
	deps.Manifests = sink
	deps.ManifestSink = sink
	deps.PostStateObserve = strayAfterFirstExec(act)
	deps.ClearObserve = faultedUntilHealed("web01", "NginxDown", act)
	fake := newCCT0292Fake()
	deps.CommitConfirm = fake
	execs := newCCT0293FakeExecs()
	execs.set(db.ForwardExecution{Verdict: "deviation", TargetHost: "web01", Site: "dc1"}) // the terminus: executed, verified, DEVIATION
	// The row exists only once the forward ran (TG-660 refuses an action whose row already exists as a spent
	// authorization), so the reader answers only after the actuator's first execution.
	execs.after = func() bool { return act.execs > 0 }
	deps.Executions = execs
	deps.BreakerTrip = func(ctx context.Context, reason string) error {
		_, err := mb.TripFor(ctx, reason, "", safety.CauseRevertFailed)
		return err
	}
	acts := NewActivities(deps)
	registerAll(env, acts)
	env.RegisterActivity(acts.BackfillManifestActivity)
	env.RegisterActivity(acts.ObserveClearedActivity)
	env.RegisterActivity(acts.RecoveredSinceActivity)
	env.RegisterActivity(acts.ReconcileActivity)
	env.RegisterActivity(acts.GraduationActivity)

	env.ExecuteWorkflow(RunnerWorkflow, ccT0292Envelope())
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete: %v", env.GetWorkflowError())
	}
	if act.execs != 2 {
		t.Fatalf("the forward AND its compensating inverse must both execute (execs=%d) — before TG-665 the inverse was refused under the trip", act.execs)
	}
	if !mb.Tripped(context.Background()) {
		t.Fatal("the deviation must have opened the breaker durably")
	}
	if !gate.MayActuate() {
		t.Fatal("TG-665: the deviation trip must leave the owner-set mode standing (no forced Shadow)")
	}
	_, resolves := fake.snapshot()
	if len(resolves) != 1 || resolves[0].State != db.CommitConfirmReverted {
		t.Fatalf("the window must resolve REVERTED (the inverse executed and verified), got %+v", resolves)
	}
	var sawCompensation bool
	for _, d := range cap.decisions() {
		if d == "actuate:compensation-under-trip" {
			sawCompensation = true
		}
	}
	if !sawCompensation {
		t.Fatalf("the inverse must have been admitted as compensation-under-trip, ledger=%v", cap.decisions())
	}
	rec, _ := mb.TripRecord(context.Background())
	if rec.Cause != safety.CauseDeviation || rec.TrippedBy == "" || rec.CompensationActionID == "" {
		t.Fatalf("the breaker row must carry the deviating action and the admitted compensation, got %+v", rec)
	}
}
