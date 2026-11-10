package runner

// TG-662 — the owner's 2026-11-07 prompt §2.D: the credit predicate's EXPLICIT classification and the separate
// CAUSALITY record. A verified target state is not the claim that TG caused it; only `tg-caused` credits, only
// `tg-caused` teaches a precedent, and every other outcome is recorded as what it is.
//
// KILLING MUTATIONS (each executed 2026-11-07, whole-package run, restored from a copy):
//   M1 classifyCausality: drop the `ev.InjectorKnown && ev.InjectorRestores > 0` case. RED:
//      TestClassifyCausality ("injector restored in the interval" reads tg-caused).
//   M2 classifyCausality: replace the `!ev.InjectorKnown || !ev.RecoveryKnown || !ev.ActorsKnown` case with
//      nothing (unknown readers fall through to tg-caused). RED: TestClassifyCausality ("actors unreadable").
//   M3 creditDecision: drop the `in.Causality != CausalityTGCaused` withhold. RED:
//      TestTerminusWithholdsCreditAndLessonWhenCausalityIsNotEstablished (the ladder is fed clean).
//   M4 ResolveCommitConfirmActivity: feed clean=true on `confirmed` without the facts read. RED:
//      TestWindowConfirmedFeedRequiresTGCausedFacts.
//   M5 the precedent gate: drop `&& tgCaused`. RED: TestTerminusWithholdsCreditAndLessonWhenCausalityIsNotEstablished
//      (a precedent lesson is written for an injector-restored heal).
//   M8 classifyCausality: disable the `!ev.ActorCovered` case (a blind reader reads quiet). RED: TestClassifyCausality
//      ("actors answered but nobody covers the subject" reads tg-caused). Review of !1819 round 2, finding 3.
//   M9 classifyCausality: disable the `ev.ActorCandidates > 1` case. RED: TestClassifyCausality ("two candidate actor
//      taxonomies"). Review round 2, finding 1.
//   M12 classifyCausality: disable the `ev.ActorUnattributed > 0` case. RED: TestClassifyCausality ("records the core
//       could not bind to the subject"). Review round 3.
//   M13 classifyCausality: require `ev.ActorsKnown &&` again on the positive actor cases. RED: TestClassifyCausality
//       ("a sanctioned admin seen while the subject read failed" reads indeterminate). Review round 3.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/lessons"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

func TestClassifyCausality(t *testing.T) {
	known := CausalityEvidence{InjectorKnown: true, RecoveryKnown: true, ActorsKnown: true, ActorCovered: true, ActorTaxonomy: attribution.Unattributable.String()}
	with := func(f func(*CausalityEvidence)) CausalityEvidence {
		ev := known
		f(&ev)
		return ev
	}
	cases := []struct {
		name     string
		verified bool
		have     bool
		ev       CausalityEvidence
		want     string
	}{
		{"target not verified", false, true, known, CausalityIndeterminate},
		{"no evidence reader", true, false, known, CausalityIndeterminate},
		{"every reader quiet", true, true, known, CausalityTGCaused},
		{"TG's own action is self, not another actor", true, true, with(func(e *CausalityEvidence) { e.ActorTaxonomy = attribution.AttributedSelf.String(); e.ActorActions = 1 }), CausalityTGCaused},
		{"injector restored in the interval", true, true, with(func(e *CausalityEvidence) { e.InjectorRestores = 1 }), CausalityInjectorRestore},
		{"carve-out actor acted in the interval", true, true, with(func(e *CausalityEvidence) { e.ActorTaxonomy = attribution.AuthorizedTest.String(); e.ActorActions = 1 }), CausalityInjectorRestore},
		{"recovery preceded the execution", true, true, with(func(e *CausalityEvidence) { e.RecoveredBeforeExecute = true }), CausalityExternal},
		{"a sanctioned admin acted in the interval", true, true, with(func(e *CausalityEvidence) {
			e.ActorTaxonomy = attribution.AttributedAuthorized.String()
			e.ActorActions = 2
		}), CausalityExternal},
		{"an unsanctioned actor acted in the interval", true, true, with(func(e *CausalityEvidence) {
			e.ActorTaxonomy = attribution.AttributedSuspicious.String()
			e.ActorActions = 1
		}), CausalityExternal},
		{"injector unreadable", true, true, with(func(e *CausalityEvidence) { e.InjectorKnown = false }), CausalityIndeterminate},
		{"recovery unreadable", true, true, with(func(e *CausalityEvidence) { e.RecoveryKnown = false }), CausalityIndeterminate},
		{"actors unreadable", true, true, with(func(e *CausalityEvidence) { e.ActorsKnown = false }), CausalityIndeterminate},
		{"actors answered but nobody covers the subject (blind, not quiet)", true, true, with(func(e *CausalityEvidence) { e.ActorCovered = false }), CausalityIndeterminate},
		{"two candidate actor taxonomies (ambiguous)", true, true, with(func(e *CausalityEvidence) { e.ActorCandidates = 2 }), CausalityIndeterminate},
		{"records the core could not bind to the subject (unattributed)", true, true, with(func(e *CausalityEvidence) { e.ActorUnattributed = 1 }), CausalityIndeterminate},
		// Positive evidence from a reader that answered wins over an unreadable sibling: the label is external, not indeterminate.
		{"a sanctioned admin seen while the subject read failed", true, true, with(func(e *CausalityEvidence) {
			e.ActorsKnown = false
			e.ActorTaxonomy = attribution.AttributedAuthorized.String()
			e.ActorActions = 1
		}), CausalityExternal},
		{"the carve-out actor seen while the subject read failed", true, true, with(func(e *CausalityEvidence) {
			e.ActorsKnown = false
			e.ActorTaxonomy = attribution.AuthorizedTest.String()
			e.ActorActions = 1
		}), CausalityInjectorRestore},
		{"overlapping injection confounds", true, true, with(func(e *CausalityEvidence) { e.InjectorInjections = 1 }), CausalityIndeterminate},
		// Positive evidence of another cause wins over an unreadable sibling reader.
		{"restore seen while actors unreadable", true, true, with(func(e *CausalityEvidence) { e.InjectorRestores = 1; e.ActorsKnown = false }), CausalityInjectorRestore},
	}
	for _, tc := range cases {
		got, evidence := classifyCausality(tc.verified, tc.have, tc.ev)
		if got != tc.want {
			t.Errorf("%s: causality %q, want %q (evidence %q)", tc.name, got, tc.want, evidence)
		}
		if evidence == "" {
			t.Errorf("%s: the evidence record must never be empty", tc.name)
		}
	}
}

// recordSpy captures the append-only terminus records the reconcile path emits.
type recordSpy struct {
	mu   sync.Mutex
	rows []TerminusRecord
}

func (r *recordSpy) append(_ context.Context, rec TerminusRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, rec)
	return nil
}

func (r *recordSpy) last(t *testing.T) TerminusRecord {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.rows) == 0 {
		t.Fatal("no terminus record was appended")
	}
	return r.rows[len(r.rows)-1]
}

// Every non-clean terminus is CLASSIFIED (the record names the first failing conjunct) and feeds the ladder as a
// streak break; a no-op (never executed) feeds nothing and leaves no record — nothing happened to classify.
func TestTerminusClassifiesEveryNonCleanOutcome(t *testing.T) {
	base := func() ReconcileInput {
		in := cleanTerminus()
		in.Host, in.TargetHost, in.ActionID = "web01", "web01", "act-1"
		return in
	}
	cases := []struct {
		name    string
		mutate  func(*ReconcileInput)
		wantFed bool
		wantDis string
	}{
		{"no-op action (never executed)", func(in *ReconcileInput) { in.Executed = false }, false, ""},
		{"empty prediction / no verdict", func(in *ReconcileInput) { in.HasVerdict = false }, true, "not-clean:no-verdict"},
		{"verdict deviation", func(in *ReconcileInput) { in.Verdict = safety.VerdictDeviation }, true, "not-clean:verdict-deviation"},
		{"still-faulted target (object unhealthy)", func(in *ReconcileInput) { in.ConfirmedClear = false; in.ClearReason = "object-unhealthy" }, true, "not-clean:not-confirmed-clear:object-unhealthy"},
		{"unmonitored target (object unobserved)", func(in *ReconcileInput) { in.ConfirmedClear = false; in.ClearReason = "object-unobserved" }, true, "not-clean:not-confirmed-clear:object-unobserved"},
		{"host kept alerting", func(in *ReconcileInput) { in.ConfirmedClear = false; in.ClearReason = "host-alerting" }, true, "not-clean:not-confirmed-clear:host-alerting"},
		{"cross-host collateral harm", func(in *ReconcileInput) { in.CollateralObserved = true }, true, "not-clean:collateral-observed"},
		{"collateral unknown (no reader)", func(in *ReconcileInput) { in.CollateralKnown = false }, true, "not-clean:collateral-unknown"},
		{"poll unanswered", func(in *ReconcileInput) { in.PollUnanswered = true }, true, "not-clean:poll-unanswered"},
		{"clean and tg-caused", func(*ReconcileInput) {}, true, "clean"},
	}
	for _, tc := range cases {
		g := &gradSpy{}
		rec := &recordSpy{}
		a := &Activities{D: Deps{RecordGraduation: g.record, TerminusRecord: rec.append}}
		in := base()
		tc.mutate(&in)
		if _, err := a.ReconcileActivity(context.Background(), in); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !tc.wantFed {
			if g.calls != 0 || len(rec.rows) != 0 {
				t.Fatalf("%s: a never-executed session feeds nothing and records nothing, got calls=%d rows=%d", tc.name, g.calls, len(rec.rows))
			}
			continue
		}
		if g.calls != 1 {
			t.Fatalf("%s: the ladder must be fed exactly once, got %d", tc.name, g.calls)
		}
		wantClean := tc.wantDis == "clean"
		if g.clean != wantClean {
			t.Fatalf("%s: clean=%v, want %v", tc.name, g.clean, wantClean)
		}
		row := rec.last(t)
		if row.CreditDisposition != tc.wantDis || row.CreditFed != true || row.CreditClean != wantClean {
			t.Fatalf("%s: record %+v, want disposition %q fed=true clean=%v", tc.name, row, tc.wantDis, wantClean)
		}
		if row.OpClass != "disk-grow" || row.ExternalRef != "librenms-1" || row.Host != "web01" {
			t.Fatalf("%s: the record must name the session, got %+v", tc.name, row)
		}
	}
}

// A verified target state whose causality is not tg-caused is WITHHELD: the ladder is not fed (neither credit nor
// streak break), no precedent lesson and no caution lesson is written, and the record says why.
func TestTerminusWithholdsCreditAndLessonWhenCausalityIsNotEstablished(t *testing.T) {
	for _, causality := range []string{"", CausalityIndeterminate, CausalityExternal, CausalityInjectorRestore} {
		g := &gradSpy{}
		rec := &recordSpy{}
		lessonsWritten, cautions := 0, 0
		a := &Activities{D: Deps{
			RecordGraduation: g.record, TerminusRecord: rec.append,
			LearnResolved: func(context.Context, lessons.ResolvedIncident) error { lessonsWritten++; return nil },
			LearnCaution:  func(context.Context, lessons.ResolvedIncident) error { cautions++; return nil },
		}}
		in := cleanWritebackInput()
		in.OpClass = "disk-grow"
		in.Causality = causality
		in.CausalityEvidence = "drill"
		if _, err := a.ReconcileActivity(context.Background(), in); err != nil {
			t.Fatalf("%q: %v", causality, err)
		}
		if g.calls != 0 {
			t.Fatalf("%q: a verified target state without tg-caused must NOT feed the ladder (got %d calls, clean=%v)", causality, g.calls, g.clean)
		}
		if lessonsWritten != 0 {
			t.Fatalf("%q: no precedent lesson may be taught when TG was not shown to be the cause", causality)
		}
		if cautions != 0 {
			t.Fatalf("%q: a heal that was not shown bad is no caution either", causality)
		}
		want := "withheld:causality-" + causality
		if causality == "" {
			want = "withheld:causality-" + CausalityIndeterminate
		}
		row := rec.last(t)
		if row.CreditDisposition != want || row.CreditFed || row.CreditClean {
			t.Fatalf("%q: record %+v, want disposition %q, not fed, not clean", causality, row, want)
		}
	}
	// The positive control: tg-caused credits and teaches.
	g := &gradSpy{}
	lessonsWritten := 0
	a := &Activities{D: Deps{RecordGraduation: g.record,
		LearnResolved: func(context.Context, lessons.ResolvedIncident) error { lessonsWritten++; return nil }}}
	in := cleanWritebackInput()
	in.OpClass = "disk-grow"
	if _, err := a.ReconcileActivity(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if g.calls != 1 || !g.clean || lessonsWritten != 1 {
		t.Fatalf("tg-caused must credit and teach exactly once, got calls=%d clean=%v lessons=%d", g.calls, g.clean, lessonsWritten)
	}
}

// The window's own confirmed feed reads the persisted causality: not tg-caused ⇒ no feed (and no streak break).
func TestWindowConfirmedFeedRequiresTGCausedFacts(t *testing.T) {
	run := func(facts func(context.Context, string) (TerminusFacts, bool, error)) (feeds int, clean bool) {
		fake := newCCT0292Fake()
		if err := fake.ArmCommitConfirm(context.Background(), db.CommitConfirmRow{
			ActionID: "act-cc-1", ExternalRef: "TG-cc-1", OpClass: "restart-service", TargetHost: "web01", WindowSeconds: 600}); err != nil {
			t.Fatal(err)
		}
		d := Deps{CommitConfirm: fake, Ledger: audit.NewLedger(), TriageFacts: facts,
			RecordGraduation: func(_ context.Context, _, _ string, c bool) error { feeds++; clean = c; return nil }}
		if err := NewActivities(d).ResolveCommitConfirmActivity(context.Background(), ResolveCommitConfirmInput{
			ActionID: "act-cc-1", ExternalRef: "TG-cc-1", State: db.CommitConfirmConfirmed, Detail: "drill"}); err != nil {
			t.Fatal(err)
		}
		return feeds, clean
	}
	now := time.Now().UTC()
	for _, c := range []string{"", CausalityIndeterminate, CausalityExternal, CausalityInjectorRestore} {
		f := TerminusFacts{Mutated: true, ConfirmedClear: true, ConfirmedClearAt: &now, CollateralKnown: true, FactsAt: &now, Causality: c}
		if feeds, _ := run(factsReader(f, true)); feeds != 0 {
			t.Fatalf("causality %q: the confirmed window must WITHHOLD the credit, got %d feed(s)", c, feeds)
		}
	}
	if feeds, _ := run(nil); feeds != 0 {
		t.Fatal("no facts seam: the confirmed window must withhold the credit")
	}
	if feeds, _ := run(func(context.Context, string) (TerminusFacts, bool, error) {
		return TerminusFacts{}, false, errors.New("pg down")
	}); feeds != 0 {
		t.Fatal("unreadable facts: the confirmed window must withhold the credit")
	}
	if feeds, clean := run(settledQuietFacts); feeds != 1 || !clean {
		t.Fatalf("tg-caused facts: the confirmed window credits exactly once, got feeds=%d clean=%v", feeds, clean)
	}
}

// The facts activity classifies causality from the evidence seam and persists it with the facts; an unverified
// target state never consults the seam (nothing to attribute) and reads indeterminate.
func TestMarkTerminusFactsClassifiesCausality(t *testing.T) {
	facts := newFactsFake()
	asked := 0
	ev := CausalityEvidence{InjectorKnown: true, RecoveryKnown: true, ActorsKnown: true, ActorCovered: true}
	acts := NewActivities(Deps{TriageMarkFacts: facts.mark, CausalityEvidence: func(_ context.Context, q CausalityQuery) (CausalityEvidence, error) {
		asked++
		if q.Host != "web01" || q.OpClass != "start-service" || q.AlertRule != "NginxDown" {
			t.Errorf("the query must name the subject, class and rule, got %+v", q)
		}
		return ev, nil
	}})
	verified := MarkTerminusFactsInput{ExternalRef: "r1", Mutated: true, ConfirmedClear: true, CollateralKnown: true, VerdictMatch: true,
		Host: "web01", TargetHost: "web01", AlertRule: "NginxDown", OpClass: "start-service"}
	out, err := acts.MarkTerminusFactsActivity(context.Background(), verified)
	if err != nil || !out.Recorded || out.Causality != CausalityTGCaused || asked != 1 {
		t.Fatalf("verified + quiet evidence must classify tg-caused, got %+v err=%v asked=%d", out, err, asked)
	}
	if f, ok, _ := facts.get(context.Background(), "r1"); !ok || f.Causality != CausalityTGCaused || f.CausalityEvidence == "" {
		t.Fatalf("the causality must be persisted with the facts, got %+v", f)
	}
	ev.InjectorRestores = 1
	verified.ExternalRef = "r2"
	if out, _ = acts.MarkTerminusFactsActivity(context.Background(), verified); out.Causality != CausalityInjectorRestore {
		t.Fatalf("an injector restore must classify injector-restore, got %+v", out)
	}
	unverified := verified
	unverified.ExternalRef, unverified.ConfirmedClear = "r3", false
	before := asked
	if out, _ = acts.MarkTerminusFactsActivity(context.Background(), unverified); out.Causality != CausalityIndeterminate || asked != before {
		t.Fatalf("an unverified target state reads indeterminate WITHOUT consulting the readers, got %+v asked=%d", out, asked-before)
	}
	// A seam error is unreadable evidence: indeterminate, never tg-caused.
	acts = NewActivities(Deps{TriageMarkFacts: facts.mark, CausalityEvidence: func(context.Context, CausalityQuery) (CausalityEvidence, error) {
		return CausalityEvidence{}, errors.New("pg down")
	}})
	verified.ExternalRef = "r4"
	if out, _ = acts.MarkTerminusFactsActivity(context.Background(), verified); out.Causality != CausalityIndeterminate || !strings.Contains(out.CausalityEvidence, "pg down") {
		t.Fatalf("a failed evidence read is indeterminate and names the failure, got %+v", out)
	}
}

// END TO END on the runner workflow: the heal executes, the host goes quiet, the object reads healthy, the collateral
// is quiet — and the injector's own restore lands in the interval. The terminus must WITHHOLD the credit (no ladder
// feed), teach no precedent, and the record must read withheld:causality-injector-restore with the evidence.
func TestInjectorRestoreWithholdsTheCreditEndToEnd(t *testing.T) {
	registerNonCCServiceClass(t)
	script := []string{
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
		`{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-cc-e2e","target":"web01","op_class":"tg648-start-unit","op":"start","params":{"unit":"nginx"},"reversible":true,"confidence":0.85,"evidence_ids":["tr-1"]}}`,
	}
	run := func(t *testing.T, ev CausalityEvidence) (*gradSpy, *recordSpy, int) {
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
		deps.BlastMembers = func(string) []string { return []string{"web01", "db01"} }
		deps.CollateralOpenedSince = func(context.Context, []string, string, string, time.Time) ([]CollateralHit, error) { return nil, nil }
		deps.CausalityEvidence = func(context.Context, CausalityQuery) (CausalityEvidence, error) { return ev, nil }
		g := &gradSpy{}
		rec := &recordSpy{}
		lessonsWritten := 0
		deps.RecordGraduation = g.record
		deps.TerminusRecord = rec.append
		deps.LearnResolved = func(context.Context, lessons.ResolvedIncident) error { lessonsWritten++; return nil }
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
		return g, rec, lessonsWritten
	}
	t.Run("injector restored in the interval", func(t *testing.T) {
		g, rec, lessonsWritten := run(t, CausalityEvidence{InjectorKnown: true, InjectorRestores: 1, RecoveryKnown: true, ActorsKnown: true, ActorCovered: true})
		if g.calls != 0 || lessonsWritten != 0 {
			t.Fatalf("an injector-restored heal must neither credit nor teach, got calls=%d lessons=%d", g.calls, lessonsWritten)
		}
		row := rec.last(t)
		if row.CreditDisposition != "withheld:causality-injector-restore" || row.Causality != CausalityInjectorRestore || !row.ConfirmedClear || row.Collateral != "quiet" {
			t.Fatalf("the record must carry the verified target state AND the withheld causality, got %+v", row)
		}
		if !strings.Contains(row.CausalityEvidence, "1 restore(s)") || row.ExecutedAt.IsZero() || row.ClearedAt.IsZero() {
			t.Fatalf("the record must carry the evidence and the interval, got %+v", row)
		}
	})
	t.Run("quiet evidence credits", func(t *testing.T) {
		g, rec, lessonsWritten := run(t, CausalityEvidence{InjectorKnown: true, RecoveryKnown: true, ActorsKnown: true, ActorCovered: true})
		if g.calls != 1 || !g.clean || lessonsWritten != 1 {
			t.Fatalf("the positive control must credit and teach once, got calls=%d clean=%v lessons=%d", g.calls, g.clean, lessonsWritten)
		}
		if row := rec.last(t); row.CreditDisposition != "clean" || row.Causality != CausalityTGCaused || !row.CreditClean {
			t.Fatalf("the record must read clean/tg-caused, got %+v", row)
		}
	})
}
