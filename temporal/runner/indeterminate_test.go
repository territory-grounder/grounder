package runner

// TG-663 (G4A F4) drills — the Runner and the armed window on an INDETERMINATE execute. Executed red→green; the
// killing mutations below were EXECUTED on 2026-11-07 (restored from a copy, never `git checkout`):
//
//   M1 — workflow.go: drop `&& !exec.Indeterminate` from the abort condition (the pre-TG-663 shape). RED:
//        TestRunnerKeepsTheWindowArmedOnIndeterminateExecute (the window resolved `aborted`).
//   M2 — commit_confirm.go: in commitConfirmAct, resolve ConsultIndeterminate at elapse as `confirmed` when healed.
//        RED: TestCommitConfirmChildIndeterminateDecidesAtElapse/healed_stands_down_without_credit (a credit-
//        feeding `confirmed` for an unproven execution).
//   M3 — commit_confirm.go: in the consult's indeterminate re-read, require only the object (drop the quiet-target
//        conjunct). RED: TestConsultCommitConfirmReadsAnIndeterminateRun/healthy_object_but_target_alerting.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// indeterminateActuator fires and loses its acknowledgement (the effect may have landed).
type indeterminateActuator struct{ execs int }

func (a *indeterminateActuator) Capability() string { return "test" }
func (a *indeterminateActuator) ReadOnly() bool     { return false }
func (a *indeterminateActuator) Exec(_ context.Context, _ []string, _ []byte) (actuation.Result, error) {
	a.execs++
	return actuation.Result{}, actuation.MarkIndeterminate(errors.New("ssh: remote exec on web01:22 lost its acknowledgement after launch: EOF"))
}

func tg663SealedManifest(t *testing.T) (*fakeManifestSink, string) {
	t.Helper()
	sink := &fakeManifestSink{}
	m, err := manifest.New(manifest.Action{Op: "start", OpClass: "start-service", Target: "web01",
		Params: map[string]string{"unit": "nginx"}, Reversible: true}, safety.BandAuto, "ph-663", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Seal(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return sink, m.ActionID
}

// The consult reads an indeterminate row as its own disposition and re-reads the OBJECT: healed only when the
// object positively reads healthy AND the target is observed quiet.
func TestConsultCommitConfirmReadsAnIndeterminateRun(t *testing.T) {
	mk := func(obj ObjectStateReading, alerts []verify.ObservedAlert, obsOK bool) (*Activities, string) {
		sink, id := tg663SealedManifest(t)
		execs := newCCT0293FakeExecs()
		execs.set(db.ForwardExecution{Verdict: db.VerdictIndeterminate, TargetHost: "web01", Site: "dc1"})
		return NewActivities(Deps{Executions: execs, Manifests: sink, Ledger: audit.NewLedger(), TriageFacts: settledQuietFacts,
			ObjectState:      func(context.Context, string, string, map[string]string) ObjectStateReading { return obj },
			PostStateObserve: func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return alerts, obsOK },
		}), id
	}
	healthy := ObjectStateReading{Healthy: true, Observed: true, Detail: "nginx.service is active"}
	cases := []struct {
		name   string
		obj    ObjectStateReading
		alerts []verify.ObservedAlert
		obsOK  bool
		healed bool
	}{
		{"healthy object and quiet target", healthy, nil, true, true},
		{"healthy object but target alerting", healthy, []verify.ObservedAlert{{Host: "web01", Rule: "NginxDown"}}, true, false},
		{"healthy object but alert surface unreadable", healthy, nil, false, false},
		{"unhealthy object", ObjectStateReading{Observed: true, Detail: "inactive"}, nil, true, false},
		{"unobserved object", ObjectStateReading{Healthy: true, Observed: false}, nil, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, id := mk(tc.obj, tc.alerts, tc.obsOK)
			got, err := a.ConsultCommitConfirmActivity(context.Background(), ConsultCommitConfirmInput{ActionID: id, ExternalRef: "r"})
			if err != nil {
				t.Fatal(err)
			}
			if got.Disposition != ConsultIndeterminate {
				t.Fatalf("an indeterminate row must read ConsultIndeterminate (never confirmed/aborted/deviation), got %q", got.Disposition)
			}
			if got.IndeterminateHealed != tc.healed {
				t.Fatalf("healed=%v, want %v (detail %q)", got.IndeterminateHealed, tc.healed, got.Detail)
			}
		})
	}
}

// The child window: at elapse a healed indeterminate run stands down WITHOUT credit (elapsed_unconfirmed), an
// unhealed one HOLDS + pages; a nudge before elapse keeps the window armed.
func TestCommitConfirmChildIndeterminateDecidesAtElapse(t *testing.T) {
	run := func(t *testing.T, obj ObjectStateReading) (*ccT0293ChildDeps, []ccT0292Resolve) {
		sink, id := tg663SealedManifest(t)
		env, d := ccT0293ChildEnv(t, func(deps *Deps) {
			deps.Manifests = sink
			deps.ObjectState = func(context.Context, string, string, map[string]string) ObjectStateReading { return obj }
			deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
		})
		d.execs.set(db.ForwardExecution{Verdict: db.VerdictIndeterminate, TargetHost: "web01", Site: "dc1"})
		if err := d.fake.ArmCommitConfirm(context.Background(), db.CommitConfirmRow{
			ActionID: id, ExternalRef: "TG-cc-1", OpClass: "start-service", TargetHost: "web01", WindowSeconds: 600}); err != nil {
			t.Fatal(err)
		}
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(CommitConfirmSignalName, CommitConfirmResolve{Kind: "confirm-input"})
		}, 1*60*1e9) // a nudge at 1 minute must NOT decide an indeterminate run
		env.ExecuteWorkflow(CommitConfirmWorkflow, CommitConfirmInput{ActionID: id, ExternalRef: "TG-cc-1", WindowSeconds: 600})
		if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
			t.Fatalf("child: %v", env.GetWorkflowError())
		}
		_, resolves := d.fake.snapshot()
		return d, resolves
	}
	t.Run("healed stands down without credit", func(t *testing.T) {
		d, resolves := run(t, ObjectStateReading{Healthy: true, Observed: true, Detail: "nginx.service is active"})
		if len(resolves) != 1 || resolves[0].State != db.CommitConfirmElapsedUnconfirmed {
			t.Fatalf("a healed indeterminate run must resolve elapsed_unconfirmed (never confirmed — unproven execution), got %+v", resolves)
		}
		if !strings.Contains(resolves[0].Detail, "without credit") {
			t.Fatalf("the resolution must say no credit was granted, got %q", resolves[0].Detail)
		}
		if len(*d.trips) != 0 {
			t.Fatalf("a stand-down must not trip the breaker, got %v", *d.trips)
		}
		for _, dec := range d.sink.decisions() {
			if dec == "commit-confirm:confirmed" || dec == "commit-confirm:aborted" {
				t.Fatalf("unexpected ledger decision %q", dec)
			}
		}
	})
	t.Run("unhealed holds and pages", func(t *testing.T) {
		d, resolves := run(t, ObjectStateReading{Observed: true, Detail: "nginx.service is inactive"})
		if len(resolves) != 1 || resolves[0].State != db.CommitConfirmHeldUnverifiable {
			t.Fatalf("an indeterminate run whose object does not read healthy must HOLD (operator-owned), got %+v", resolves)
		}
		if len(*d.trips) != 0 {
			t.Fatalf("a hold must not trip the breaker, got %v", *d.trips)
		}
	})
}

// The Runner, end to end: an indeterminate execute does NOT abort the armed window (it stays armed for its own
// elapse consult), records the session `indeterminate:ack-lost` with a reconciliation read, mutates nothing in
// the record and feeds the ladder nothing. POSITIVE CONTROL: TestRunnerAbortsWindowWhenChainRefuses (a refusal
// still aborts).
func TestRunnerKeepsTheWindowArmedOnIndeterminateExecute(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	gate := safety.NewActuatingChokepoint()
	act := &indeterminateActuator{}
	sink := &fakeManifestSink{}
	deps := testDeps(ccT0292ProposeScript()...)
	deps.Mutation = gate
	deps.Interceptor = withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger()))
	deps.Manifests = sink
	deps.ManifestSink = sink
	deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
	// The necessity gate must see the fault (the effect is attempted); the clear loop is never entered on an
	// indeterminate terminal, so a static alerting reader is the honest estate here.
	deps.ClearObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return []verify.ObservedAlert{{Host: "web01", Rule: "NginxDown"}}, true
	}
	fake := newCCT0292Fake()
	deps.CommitConfirm = fake
	execs := newCCT0293FakeExecs()
	execs.set(db.ForwardExecution{Verdict: db.VerdictIndeterminate, TargetHost: "web01", Site: "dc1"})
	// The row exists only once the effect was attempted (TG-660: ExecuteActivity refuses an action whose row already
	// exists, as a spent authorization) — so the reader answers only after the actuator ran.
	execs.after = func() bool { return act.execs > 0 }
	deps.Executions = execs
	g := &gradSpy{}
	deps.RecordGraduation = g.record
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
	if act.execs != 1 {
		t.Fatalf("precondition: the effect must have been attempted once (execs=%d)", act.execs)
	}
	var res RunnerResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Mutated {
		t.Fatal("an indeterminate execute proves no mutation — Mutated must stay false")
	}
	if !strings.HasPrefix(res.Outcome, "indeterminate:ack-lost") || !strings.Contains(res.Outcome, "reconciliation read") {
		t.Fatalf("the session must record the indeterminate terminal with its reconciliation read, got %q", res.Outcome)
	}
	if g.calls != 0 {
		t.Fatalf("an unproven execution must feed the ladder nothing, got %d feed(s)", g.calls)
	}
	arms, resolves := fake.snapshot()
	if len(arms) != 1 {
		t.Fatalf("the window must have been armed once, got %d", len(arms))
	}
	for _, r := range resolves {
		if r.State == db.CommitConfirmAborted || r.State == db.CommitConfirmConfirmed {
			t.Fatalf("TG-663: an indeterminate execute must NOT abort the armed window (the effect may have landed) and must never confirm it (unproven), got %+v", resolves)
		}
	}
	// The test environment runs the abandoned child to its elapse: the window either is still ARMED or has
	// decided on its own re-read — here the healed stand-down (elapsed_unconfirmed, no credit). Both are the
	// designed outcomes; `aborted` and `confirmed` are the two it must never reach.
	switch arms[0].State {
	case db.CommitConfirmArmed:
	case db.CommitConfirmElapsedUnconfirmed:
		if len(resolves) != 1 || !strings.Contains(resolves[0].Detail, "without credit") {
			t.Fatalf("the elapse decision must be the healed stand-down without credit, got %+v", resolves)
		}
	default:
		t.Fatalf("the window must be armed or stood down without credit, got %q", arms[0].State)
	}
}
