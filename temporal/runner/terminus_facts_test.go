package runner

// TG-662 (G4A F3/F8) drills — the terminus facts, the object read, and the window that reads them. Each claim
// executed red→green; the killing mutations below were EXECUTED on 2026-11-07 against this worktree and each
// turned the named drill red before the code was restored (restored from a copy, never `git checkout`):
//
//   M1 — workflow.go: objectHealthy() returns true unconditionally (the read's answer ignored; dropping the
//        conjunct itself does not compile — the closure becomes unused). RED:
//        TestClearConfirmRequiresAPositiveObjectRead/object_unhealthy_never_credits and /object_unobserved_never_credits.
//   M2 — commit_confirm.go: delete the applyTerminusFacts gate in ConsultCommitConfirmActivity (match confirms
//        alone, the pre-TG-662 shape). RED: TestConsultCommitConfirmRequiresTheTerminusFacts,
//        TestConsultCommitConfirmRoutesTheTerminus (the no-facts row), TestCommitConfirmChildAwaitsTheTerminusFacts
//        (facts_never_settle, settled_but_never_cleared, collateral_observed).
//   M3 — reconcile.go: drop `in.CollateralKnown &&` from the credit predicate. RED:
//        TestReconcileFeedsNotCleanWhenAnyPreconditionFails/collateral-unknown,
//        TestTerminusCreditRequiresKnownQuietCollateral/unknown_is_indeterminate,
//        TestTerminusCollateralDemotesTheCleanRun/an_unobservable_radius_is_indeterminate.
//   M4 — reconcile.go: re-add the pre-TG-662 early return for commit-confirmed classes. RED:
//        TestTerminusCreditsCommitConfirmedClassOnlyWhenWindowConfirmed (confirmed window never credited).
//   M5 — commit_confirm.go: make commitConfirmAct resolve ConsultPendingClear at elapse as confirmed. RED:
//        TestCommitConfirmChildAwaitsTheTerminusFacts/facts_never_settle (confirmed instead of elapsed_unconfirmed).
//   (M6 hostdiag and M7 core/db are recorded in their own test files.)

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// factsFake is the in-memory terminus-facts store (both seams over ONE map, exactly like the durable row).
type factsFake struct {
	mu sync.Mutex
	m  map[string]TerminusFacts
}

func newFactsFake() *factsFake { return &factsFake{m: map[string]TerminusFacts{}} }

func (f *factsFake) mark(_ context.Context, ref string, in TerminusFacts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	in.FactsAt = &now
	if in.ConfirmedClear {
		in.ConfirmedClearAt = &now
	}
	f.m[ref] = in
	return nil
}

func (f *factsFake) get(_ context.Context, ref string) (TerminusFacts, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[ref]
	return v, ok, nil
}

// settledQuietFacts is the SETTLED, CLEAR, COLLATERAL-QUIET reading — the one that lets a match confirm.
func settledQuietFacts(_ context.Context, _ string) (TerminusFacts, bool, error) {
	now := time.Now().UTC()
	return TerminusFacts{Mutated: true, ConfirmedClear: true, ConfirmedClearAt: &now, CollateralKnown: true, FactsAt: &now, Causality: CausalityTGCaused}, true, nil
}

// quietCausality is the all-KNOWN, nothing-seen evidence: no injector restore or injection, no recovery before
// the execution, no other actor — the reading that classifies tg-caused.
func quietCausality(context.Context, CausalityQuery) (CausalityEvidence, error) {
	return CausalityEvidence{InjectorKnown: true, RecoveryKnown: true, ActorsKnown: true, ActorCovered: true}, nil
}

func factsReader(f TerminusFacts, found bool) func(context.Context, string) (TerminusFacts, bool, error) {
	return func(context.Context, string) (TerminusFacts, bool, error) { return f, found, nil }
}

// An unobserved object can never read healthy, and a nil seam is unobserved.
func TestObserveObjectStateActivityNeverReportsHealthyWhenUnobserved(t *testing.T) {
	got, err := NewActivities(Deps{}).ObserveObjectStateActivity(context.Background(), ObjectStateInput{Host: "web01", OpClass: "start-service"})
	if err != nil || got.Observed || got.Healthy {
		t.Fatalf("a nil object-state seam must read unobserved and unhealthy, got %+v err=%v", got, err)
	}
	acts := NewActivities(Deps{ObjectState: func(context.Context, string, string, map[string]string) ObjectStateReading {
		return ObjectStateReading{Healthy: true, Observed: false, Detail: "a reader that claims healthy without observing"}
	}})
	got, err = acts.ObserveObjectStateActivity(context.Background(), ObjectStateInput{Host: "web01", OpClass: "start-service"})
	if err != nil || got.Healthy {
		t.Fatalf("Healthy must be forced false when Observed is false, got %+v err=%v", got, err)
	}
}

// The consult's facts gate: a terminus match is necessary, never sufficient.
func TestConsultCommitConfirmRequiresTheTerminusFacts(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name  string
		facts TerminusFacts
		found bool
		want  string
	}{
		{"no triage row", TerminusFacts{}, false, ConsultPendingClear},
		{"facts not settled", TerminusFacts{Mutated: true}, true, ConsultPendingClear},
		{"settled, never cleared", TerminusFacts{Mutated: true, CollateralKnown: true, FactsAt: &now}, true, ConsultUnconfirmed},
		{"settled, collateral unknown", TerminusFacts{Mutated: true, ConfirmedClear: true, ConfirmedClearAt: &now, FactsAt: &now}, true, ConsultUnconfirmed},
		{"settled, collateral observed", TerminusFacts{Mutated: true, ConfirmedClear: true, ConfirmedClearAt: &now, CollateralKnown: true, CollateralObserved: true, FactsAt: &now}, true, ConsultCollateral},
		{"settled, clear, quiet", TerminusFacts{Mutated: true, ConfirmedClear: true, ConfirmedClearAt: &now, CollateralKnown: true, FactsAt: &now}, true, ConsultConfirmed},
	}
	for _, tc := range cases {
		execs := newCCT0293FakeExecs()
		execs.set(db.ForwardExecution{Verdict: "match"})
		acts := NewActivities(Deps{Executions: execs, Ledger: audit.NewLedger(), TriageFacts: factsReader(tc.facts, tc.found)})
		got, err := acts.ConsultCommitConfirmActivity(context.Background(), ConsultCommitConfirmInput{ActionID: "a", ExternalRef: "r"})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Disposition != tc.want {
			t.Fatalf("%s: disposition %q, want %q (detail %q)", tc.name, got.Disposition, tc.want, got.Detail)
		}
	}
	// A non-match is never gated into a confirm by the facts: a deviation stays the inverse's case.
	execs := newCCT0293FakeExecs()
	execs.set(db.ForwardExecution{Verdict: "deviation"})
	acts := NewActivities(Deps{Executions: execs, Ledger: audit.NewLedger(), TriageFacts: settledQuietFacts})
	if got, err := acts.ConsultCommitConfirmActivity(context.Background(), ConsultCommitConfirmInput{ActionID: "a", ExternalRef: "r"}); err != nil || got.Disposition != ConsultDeviation {
		t.Fatalf("settled facts must never upgrade a deviation, got %+v err=%v", got, err)
	}
}

// The child window on the new readings: pending facts at elapse AWAIT (bounded) and confirm once they settle;
// facts that never settle resolve elapsed_unconfirmed; settled-unconfirmed resolves at once with no page and
// no breaker; collateral HOLDS + pages, and the inverse does not fire on it.
func TestCommitConfirmChildAwaitsTheTerminusFacts(t *testing.T) {
	t.Run("facts settle during the await and confirm", func(t *testing.T) {
		facts := newFactsFake()
		env, d := ccT0293ChildEnv(t, func(deps *Deps) { deps.TriageFacts = facts.get })
		d.execs.set(db.ForwardExecution{Verdict: "match"})
		// The parent settles the facts 11 minutes in: after the 600 s window elapsed, before the first
		// 3-minute await consult.
		env.RegisterDelayedCallback(func() {
			_ = facts.mark(context.Background(), "TG-cc-1", TerminusFacts{Mutated: true, ConfirmedClear: true, CollateralKnown: true})
		}, 11*time.Minute)
		env.ExecuteWorkflow(CommitConfirmWorkflow, ccT0292ChildInput())
		if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
			t.Fatalf("child: %v", env.GetWorkflowError())
		}
		_, resolves := d.fake.snapshot()
		if len(resolves) != 1 || resolves[0].State != db.CommitConfirmConfirmed {
			t.Fatalf("facts settling during the await must confirm exactly once, got %+v", resolves)
		}
		if len(*d.trips) != 0 {
			t.Fatalf("a confirm must not touch the breaker, got %v", *d.trips)
		}
	})
	t.Run("facts never settle resolves elapsed_unconfirmed with no page", func(t *testing.T) {
		env, d := ccT0293ChildEnv(t, func(deps *Deps) { deps.TriageFacts = newFactsFake().get })
		d.execs.set(db.ForwardExecution{Verdict: "match"})
		env.ExecuteWorkflow(CommitConfirmWorkflow, ccT0292ChildInput())
		if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
			t.Fatalf("child: %v", env.GetWorkflowError())
		}
		_, resolves := d.fake.snapshot()
		if len(resolves) != 1 || resolves[0].State != db.CommitConfirmElapsedUnconfirmed {
			t.Fatalf("facts never settling must resolve elapsed_unconfirmed (never confirmed, never held), got %+v", resolves)
		}
		if len(*d.trips) != 0 {
			t.Fatalf("an unconfirmed window must not trip the breaker, got %v", *d.trips)
		}
		for _, dec := range d.sink.decisions() {
			if dec == "commit-confirm:confirmed" || dec == "commit-confirm:held_unverifiable" {
				t.Fatalf("unexpected ledger decision %q", dec)
			}
		}
	})
	t.Run("settled but never cleared resolves at the nudge", func(t *testing.T) {
		now := time.Now().UTC()
		env, d := ccT0293ChildEnv(t, func(deps *Deps) {
			deps.TriageFacts = factsReader(TerminusFacts{Mutated: true, CollateralKnown: true, FactsAt: &now}, true)
		})
		d.execs.set(db.ForwardExecution{Verdict: "match"})
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(CommitConfirmSignalName, CommitConfirmResolve{Kind: "confirm-input"})
		}, time.Second)
		env.ExecuteWorkflow(CommitConfirmWorkflow, ccT0292ChildInput())
		if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
			t.Fatalf("child: %v", env.GetWorkflowError())
		}
		_, resolves := d.fake.snapshot()
		if len(resolves) != 1 || resolves[0].State != db.CommitConfirmElapsedUnconfirmed {
			t.Fatalf("a settled-unconfirmed terminus must resolve elapsed_unconfirmed, got %+v", resolves)
		}
	})
	t.Run("collateral observed holds and pages without firing", func(t *testing.T) {
		now := time.Now().UTC()
		env, d := ccT0293ChildEnv(t, func(deps *Deps) {
			deps.TriageFacts = factsReader(TerminusFacts{Mutated: true, ConfirmedClear: true, ConfirmedClearAt: &now,
				CollateralKnown: true, CollateralObserved: true, FactsAt: &now}, true)
			// The hold-watch's live observer reads QUIET, so the inverse must NOT fire out of the hold.
			deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
		})
		d.execs.set(db.ForwardExecution{Verdict: "match", TargetHost: "web01", Site: "dc1"})
		env.ExecuteWorkflow(CommitConfirmWorkflow, ccT0292ChildInput())
		if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
			t.Fatalf("child: %v", env.GetWorkflowError())
		}
		_, resolves := d.fake.snapshot()
		if len(resolves) != 1 || resolves[0].State != db.CommitConfirmHeldUnverifiable {
			t.Fatalf("observed collateral must HOLD (operator-owned), never confirm and never fire, got %+v", resolves)
		}
		if len(*d.trips) != 0 {
			t.Fatalf("a collateral hold must not trip the breaker, got %v", *d.trips)
		}
	})
}

// The full Runner: ConfirmedClear — and therefore the ladder credit — needs the healed OBJECT to read positively
// healthy on top of the host going quiet. An unhealthy or UNOBSERVED object never credits (indeterminate), a
// healthy one does. A non-commit-confirmed class so the window plays no part in the credit.
func TestClearConfirmRequiresAPositiveObjectRead(t *testing.T) {
	registerNonCCServiceClass(t)
	script := []string{
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
		`{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-cc-e2e","target":"web01","op_class":"tg648-start-unit","op":"start","params":{"unit":"nginx"},"reversible":true,"confidence":0.85,"evidence_ids":["tr-1"]}}`,
	}
	run := func(t *testing.T, object ObjectStateReading) (*gradSpy, int, []ObjectStateInput) {
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestWorkflowEnvironment()
		gate := safety.NewActuatingChokepoint()
		act := &recordingActuator{}
		sink := &fakeManifestSink{}
		deps := testDeps(script...)
		deps.Mutation = gate
		deps.Interceptor = withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger()))
		deps.Manifests = sink
		deps.ManifestSink = sink
		deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
		deps.ClearObserve = faultedUntilHealed("web01", "NginxDown", act)
		var reads []ObjectStateInput
		var mu sync.Mutex
		deps.ObjectState = func(_ context.Context, host, opClass string, params map[string]string) ObjectStateReading {
			mu.Lock()
			defer mu.Unlock()
			reads = append(reads, ObjectStateInput{Host: host, OpClass: opClass, Params: params})
			return object
		}
		// The collateral re-check surveys a quiet radius, so the object read is the only variable.
		deps.BlastMembers = func(string) []string { return []string{"web01", "db01"} }
		deps.CollateralOpenedSince = func(context.Context, []string, string, string, time.Time) ([]CollateralHit, error) { return nil, nil }
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
			t.Fatalf("precondition: the heal must execute once (execs=%d)", act.execs)
		}
		if g.calls != 1 {
			t.Fatalf("the terminus must feed the ladder exactly once, got %d", g.calls)
		}
		mu.Lock()
		defer mu.Unlock()
		return g, act.execs, append([]ObjectStateInput(nil), reads...)
	}
	t.Run("object healthy credits", func(t *testing.T) {
		g, _, reads := run(t, ObjectStateReading{Healthy: true, Observed: true})
		if !g.clean {
			t.Fatal("host quiet AND object healthy is the clean run")
		}
		if len(reads) == 0 || reads[0].Host != "web01" || reads[0].OpClass != nonCCSlug || reads[0].Params["unit"] != "nginx" {
			t.Fatalf("the object read must name the sealed target/class/params, got %+v", reads)
		}
	})
	t.Run("object unhealthy never credits", func(t *testing.T) {
		g, _, _ := run(t, ObjectStateReading{Healthy: false, Observed: true, Detail: "nginx.service is inactive"})
		if g.clean {
			t.Fatal("a quiet host with the unit still down must NOT credit — the object read is load-bearing (TG-662)")
		}
	})
	t.Run("object unobserved never credits", func(t *testing.T) {
		g, _, _ := run(t, ObjectStateReading{Healthy: true, Observed: false, Detail: "unmonitored target"})
		if g.clean {
			t.Fatal("an unobservable object is INDETERMINATE and must never credit (TG-662/TG-668)")
		}
	})
}

// The terminus credit treats an UNKNOWN collateral reading as indeterminate: not clean, never credited.
func TestTerminusCreditRequiresKnownQuietCollateral(t *testing.T) {
	for name, tc := range map[string]struct {
		known, observed, wantClean bool
	}{
		"surveyed quiet credits":   {true, false, true},
		"unknown is indeterminate": {false, false, false},
		"observed demotes":         {true, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			g := &gradSpy{}
			in := gradInput()
			in.CollateralKnown, in.CollateralObserved = tc.known, tc.observed
			if _, err := NewActivities(Deps{RecordGraduation: g.record}).ReconcileActivity(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			if g.calls != 1 || g.clean != tc.wantClean {
				t.Fatalf("calls=%d clean=%v, want one feed with clean=%v", g.calls, g.clean, tc.wantClean)
			}
		})
	}
}

// A consult never confirms on facts OLDER than the execution it judges: the triage row is INSERT … ON CONFLICT
// DO NOTHING per external_ref, so a re-triaged ref still carries an earlier run's settled facts until this run's
// terminus writes its own. Those read PENDING (this window keeps waiting), never confirmed.
//
// KILLING MUTATION (executed 2026-11-07): drop the `facts.FactsAt.Before(notBefore)` case in applyTerminusFacts.
// RED: TestConsultTreatsFactsOlderThanTheExecutionAsPending ("facts predate the execution" confirmed). Restored.
func TestConsultTreatsFactsOlderThanTheExecutionAsPending(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name    string
		factsAt time.Time
		want    string
	}{
		{"facts predate the execution", now.Add(-time.Hour), ConsultPendingClear},
		{"facts follow the execution", now.Add(7 * time.Minute), ConsultConfirmed},
	}
	for _, tc := range cases {
		execs := newCCT0293FakeExecs()
		execs.set(db.ForwardExecution{Verdict: "match", ExecutedAt: now})
		at := tc.factsAt
		facts := TerminusFacts{Mutated: true, ConfirmedClear: true, ConfirmedClearAt: &at, CollateralKnown: true, FactsAt: &at}
		acts := NewActivities(Deps{Executions: execs, Ledger: audit.NewLedger(), TriageFacts: factsReader(facts, true)})
		got, err := acts.ConsultCommitConfirmActivity(context.Background(), ConsultCommitConfirmInput{ActionID: "a", ExternalRef: "r"})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Disposition != tc.want {
			t.Fatalf("%s: disposition %q, want %q (detail %q)", tc.name, got.Disposition, tc.want, got.Detail)
		}
	}
}

// A history that entered the clear loop BEFORE TG-662 replays terminus-object-read at DefaultVersion: its loop never
// read the object, so its "clear" is host-quiet alone. When such a history reaches the terminus after the deploy it
// must take the LEGACY mark (confirmed_clear only — no confirmed_clear_at / terminus_facts_at), never the facts
// write: persisting a quiet-only clear as a fact would let the window confirm on the F8 hole. The window then
// reads PENDING and resolves elapsed_unconfirmed — a lost credit, never a granted one.
//
// KILLING MUTATION (executed 2026-11-07): `factsV2 := workflow.GetVersion(ctx, "terminus-facts-persist", …) >= 1`
// without the objectReadV1 conjunct. RED: TestLegacyClearLoopHistoriesNeverPersistTheFacts (the facts fake holds a
// ConfirmedClear record and the legacy mark never ran). Restored from a copy, green.
func TestLegacyClearLoopHistoriesNeverPersistTheFacts(t *testing.T) {
	script := []string{
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
		`{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-cc-e2e","target":"web01","op_class":"start-service","op":"start","params":{"unit":"nginx"},"reversible":true,"confidence":0.85,"evidence_ids":["tr-1"]}}`,
	}
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.OnGetVersion("terminus-object-read", workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
	gate := safety.NewActuatingChokepoint()
	act := &recordingActuator{}
	sink := &fakeManifestSink{}
	deps := testDeps(script...)
	deps.Mutation = gate
	deps.Interceptor = withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger()))
	deps.Manifests = sink
	deps.ManifestSink = sink
	deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
	deps.ClearObserve = faultedUntilHealed("web01", "NginxDown", act)
	var mu sync.Mutex
	objectReads, legacyMarks, legacyCleared := 0, 0, false
	deps.ObjectState = func(context.Context, string, string, map[string]string) ObjectStateReading {
		mu.Lock()
		defer mu.Unlock()
		objectReads++
		return ObjectStateReading{Healthy: true, Observed: true}
	}
	deps.BlastMembers = func(string) []string { return []string{"web01", "db01"} }
	deps.CollateralOpenedSince = func(context.Context, []string, string, string, time.Time) ([]CollateralHit, error) { return nil, nil }
	facts := newFactsFake()
	deps.TriageMarkFacts = facts.mark
	deps.TriageFacts = facts.get
	deps.TriageMarkCleared = func(_ context.Context, _ string, cleared bool) error {
		mu.Lock()
		defer mu.Unlock()
		legacyMarks++
		legacyCleared = cleared
		return nil
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
	if act.execs != 1 {
		t.Fatalf("precondition: the heal must execute once (execs=%d)", act.execs)
	}
	mu.Lock()
	defer mu.Unlock()
	if objectReads != 0 {
		t.Fatalf("a legacy history never schedules the object read, got %d", objectReads)
	}
	if rec, found, _ := facts.get(context.Background(), "TG-cc-e2e"); found {
		t.Fatalf("a legacy clear loop must NOT persist the terminus facts (its clear was host-quiet alone), got %+v", rec)
	}
	if legacyMarks != 1 || !legacyCleared {
		t.Fatalf("the legacy 0039 mark must run exactly once with cleared=true, got marks=%d cleared=%v", legacyMarks, legacyCleared)
	}
}

// The facts await (commitConfirmAwaitFactsRechecks × commitConfirmAwaitFactsMinutes after the window elapses) must
// outlast the parent's clear loop (clearMaxWindow after execute, plus the debounce and a margin for the object
// read + collateral re-check) for EVERY commit-confirmed class, or a slow-but-genuine heal would resolve
// elapsed_unconfirmed by construction. Nothing else ties the two constant sets together.
func TestCommitConfirmFactsAwaitOutlastsTheClearLoop(t *testing.T) {
	await := time.Duration(commitConfirmAwaitFactsRechecks) * commitConfirmAwaitFactsMinutes * time.Minute
	need := clearMaxWindow + time.Duration(clearConfirmReadings)*clearRecheckInterval + 5*time.Minute
	n := 0
	for _, s := range opschema.Specs() {
		if s.CommitConfirmed == nil {
			continue
		}
		n++
		window := time.Duration(s.CommitConfirmed.WindowSeconds) * time.Second
		if window+await < need {
			t.Errorf("%s: window %s + facts await %s = %s cannot cover the clear-loop bound %s", s.OpClass, window, await, window+await, need)
		}
	}
	if n == 0 {
		t.Fatal("no commit-confirmed class in the registry — the check has no subject")
	}
}
