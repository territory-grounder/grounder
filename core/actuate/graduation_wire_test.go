package actuate

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// The graduation earn-path (spec/013 REQ-1217, spec/015 REQ-1514): AFTER a governed action EXECUTES and its
// post-state VERIFIES, the interceptor feeds the run outcome to the per-op-class graduation ladder so a
// verified-clean run accrues toward `auto`. These tests prove the WRITE-BACK half of the earn-path that was
// designed (REQ-1216 comments) but never wired — the dead-ladder bug: with no Record call the ladder's
// clean-run count stayed 0 forever and no class could graduate. A *policy.Ladder satisfies the
// GraduationRecorder seam; a scripted spy proves a refused action never touches the ladder and that a record
// failure is non-fatal.

// spyGradRecorder is a scripted GraduationRecorder: it counts Record calls and remembers the last op-class +
// outcome, so a test can assert WHETHER the interceptor recorded and WITH WHICH outcome (the verdict→outcome
// mapping) without a real ladder.
type spyGradRecorder struct {
	calls   int
	lastOp  string
	lastOut policy.RunOutcome
}

func (s *spyGradRecorder) Record(_ context.Context, opClass string, outcome policy.RunOutcome) (policy.RecordResult, error) {
	s.calls++
	s.lastOp = opClass
	s.lastOut = outcome
	return policy.RecordResult{}, nil
}

// errGradRecorder always fails Record — to prove a record error is NON-FATAL to an already-executed action.
type errGradRecorder struct{ calls int }

func (e *errGradRecorder) Record(_ context.Context, _ string, _ policy.RunOutcome) (policy.RecordResult, error) {
	e.calls++
	return policy.RecordResult{}, errors.New("graduation store unavailable")
}

// deviationRequest is goodRequest whose post-state SURPRISES its committed prediction (an alert on a host the
// prediction never named) — so the deterministic verifier returns DEVIATION (mirrors
// TestMispredictedPostStateYieldsDeviation).
func deviationRequest(t *testing.T) Request {
	t.Helper()
	r := goodRequest(t) // op-class "restart-service", target web01
	r.Prediction = verify.Prediction{
		ActionID:       r.Manifest.ActionID,
		TargetHost:     "web01",
		Site:           "nl",
		PredictedHosts: map[string]struct{}{"web01": {}},
	}
	// The surprise host appears AFTER the action (a real cascade), NOT before it. The interceptor captures a
	// pre-execute BASELINE (TG-148), so a surprise must be NEW to trigger a deviation: the first Observe (pre) is
	// quiet, the second (post) surfaces the cascade on surprise99.
	call := 0
	r.Observe = func(context.Context) ([]verify.ObservedAlert, bool) {
		call++
		if call == 1 {
			return []verify.ObservedAlert{}, true // pre-execute baseline: quiet (observed OK)
		}
		return []verify.ObservedAlert{{Host: "surprise99", Rule: "HostDown", Site: "nl"}}, true // post: the cascade appears
	}
	return r
}

// (a) A verified-`match` executed action records NOTHING on the ladder here (REQ-1223). The promote moved to
// the session terminus, where the clear-confirm loop has re-observed AFTER the monitoring surface refreshed;
// the immediate post-execution read is ~1s old against a minutes-long poll cycle and cannot tell a heal that
// worked from one whose consequences have not surfaced. Recording it as `unverified` instead would be worse
// than silence — that resets the consecutive-clean count, so every good heal would wipe its own streak.
func TestGraduationDefersTheCleanRunToTheTerminus(t *testing.T) {
	ctx := context.Background()
	ladder := policy.NewLadder(policy.DefaultPromoteThreshold, policy.NewMemGraduationStore(), nil)
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).
		WithGraduationRecorder(ladder)

	out, err := i.Do(ctx, goodRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || out.Verdict != safety.VerdictMatch {
		t.Fatalf("an admissible action must execute and verify match: %+v", out)
	}
	st := ladder.State(ctx, "restart-service")
	if st.CleanRunCount != 0 {
		t.Fatalf("the immediate observation must NOT promote — the terminus decides it; got count=%d", st.CleanRunCount)
	}
	if st.Level != policy.LevelApprove {
		t.Fatalf("deferring the promote must not change the level, got %v", st.Level)
	}
}

// (b) Five consecutive verified-clean runs PROMOTE the class approve→auto: LevelOf flips to auto and
// GraduatedVerdict now honors an `auto` rule verdict (before promotion it downgrades auto→approve). This is
// the whole point — a class can finally graduate.
func TestGraduationPromotesAfterThresholdCleanRuns(t *testing.T) {
	ctx := context.Background()
	ladder := policy.NewLadder(policy.DefaultPromoteThreshold, policy.NewMemGraduationStore(), nil)
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).
		WithGraduationRecorder(ladder)

	// The clean run now arrives from the session TERMINUS (temporal/runner/reconcile.go), so this drives the
	// ladder the way that path does. What is under test is the ladder's threshold behaviour, which is unchanged.
	_ = i
	for run := 1; run <= policy.DefaultPromoteThreshold; run++ {
		if _, err := ladder.Record(ctx, "restart-service", policy.OutcomeVerifiedClean); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		st := ladder.State(ctx, "restart-service")
		if run < policy.DefaultPromoteThreshold {
			if st.Level != policy.LevelApprove || st.CleanRunCount != run {
				t.Fatalf("run %d: want approve count=%d, got level=%v count=%d", run, run, st.Level, st.CleanRunCount)
			}
			if v := ladder.GraduatedVerdict(ctx, "restart-service", policy.VerdictAuto); v != policy.VerdictApprove {
				t.Fatalf("run %d: an ungraduated class must downgrade an auto verdict to approve, got %v", run, v)
			}
		} else {
			// LAW CHANGE (spec/028 REQ-2807): the first climb lands at auto_notice. The class stops needing a
			// human VOTE — which is what this oracle has always been about — but every action it now takes
			// raises a notice, and the silent rung is a second, longer climb away.
			if st.Level != policy.LevelAutoNotice {
				t.Fatalf("after %d clean runs the class must be promoted to auto_notice, got level=%v", run, st.Level)
			}
			if v := ladder.GraduatedVerdict(ctx, "restart-service", policy.VerdictAuto); v != policy.VerdictAuto {
				t.Fatalf("a graduated class must honor an auto verdict, got %v", v)
			}
		}
	}
}

// (c) A verified-`deviation` executed action DEMOTES the class and resets its count: a class seeded at auto
// drops to approve on the first deviation (autonomy is always dropped on a deviation).
func TestGraduationDeviationDemotesAndResets(t *testing.T) {
	ctx := context.Background()
	store := policy.NewMemGraduationStore().Seed(policy.ClassState{OpClass: "restart-service", Level: policy.LevelAuto})
	ladder := policy.NewLadder(policy.DefaultPromoteThreshold, store, nil)
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).
		WithGraduationRecorder(ladder)

	if lvl := ladder.LevelOf(ctx, "restart-service"); lvl != policy.LevelAuto {
		t.Fatalf("precondition: seeded class must load at auto, got %v", lvl)
	}
	out, err := i.Do(ctx, deviationRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || out.Verdict != safety.VerdictDeviation {
		t.Fatalf("the action must execute and verify a deviation: %+v", out)
	}
	st := ladder.State(ctx, "restart-service")
	if st.Level != policy.LevelApprove || st.CleanRunCount != 0 {
		t.Fatalf("a deviation must demote to approve and reset the count, got level=%v count=%d", st.Level, st.CleanRunCount)
	}
	if v := ladder.GraduatedVerdict(ctx, "restart-service", policy.VerdictAuto); v != policy.VerdictApprove {
		t.Fatalf("a demoted class must downgrade an auto verdict to approve, got %v", v)
	}
}

// The verdict→outcome mapping the interceptor applies at the boundary: a verified match records
// OutcomeVerifiedClean; a verified deviation records OutcomeDeviated. Proven with a spy so the exact recorded
// outcome (not just the ladder side effect) is asserted.
func TestGraduationMapsVerdictToOutcome(t *testing.T) {
	ctx := context.Background()
	t.Run("match → deferred, nothing recorded", func(t *testing.T) {
		spy := &spyGradRecorder{}
		i := actuatingInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).
			WithGraduationRecorder(spy)
		if _, err := i.Do(ctx, goodRequest(t)); err != nil {
			t.Fatal(err)
		}
		if spy.calls != 0 {
			t.Fatalf("a verified match must record NOTHING here — the promote is decided at the terminus; got calls=%d outcome=%v", spy.calls, spy.lastOut)
		}
	})
	t.Run("deviation → deviated", func(t *testing.T) {
		spy := &spyGradRecorder{}
		i := actuatingInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).
			WithGraduationRecorder(spy)
		if _, err := i.Do(ctx, deviationRequest(t)); err != nil {
			t.Fatal(err)
		}
		if spy.calls != 1 || spy.lastOut != policy.OutcomeDeviated {
			t.Fatalf("a verified deviation must record OutcomeDeviated, got calls=%d outcome=%v", spy.calls, spy.lastOut)
		}
	})
}

// (c2) THE FAIL-CLOSED VERIFIER (TG-182 + TG-550): an executed action whose POST-STATE COULD NOT BE OBSERVED
// (the observer reports ok=false — e.g. a monitoring outage during verify) must NOT be laundered as a
// verified-clean run. An empty observation would otherwise compute `match` and advance graduation on ZERO
// evidence — the fail-OPEN bug. The interceptor's immediate ~1s verify handles this by staying SILENT on an
// unobservable post-state: it is "too early to tell", not evidence of a bad heal, and recording it as
// OutcomeUnverified prematurely RESET the consecutive-clean streak a slow-settling heal's own decider then
// credited (TG-550). So here the immediate verify must record NOTHING (never a clean run, never any outcome)
// and WITHHOLD the durable verdict (empty Outcome.Verdict). The fail-closed-against-promotion for a genuinely
// unobservable run lives at the DECIDER (the session terminus / the commit-confirm window resolution, whose
// tests own it); this guard proves the immediate verify neither launders a clean run NOR prematurely resets.
func TestUnobservablePostStateIsNotVerifiedClean(t *testing.T) {
	ctx := context.Background()
	// The PRE read (the baseline gate's first attempt) succeeds — otherwise the gate refuses and the action
	// never executes, which is REQ-1228's job and TestUnestablishedBaselineRefusesToExecute's subject. This
	// test's subject is the POST read failing on an action that DID execute: calls after the first fail.
	call := 0
	unobservable := func(context.Context) ([]verify.ObservedAlert, bool) {
		call++
		if call == 1 {
			return []verify.ObservedAlert{}, true // pre-execute baseline: established, quiet
		}
		return nil, false // post-state: could NOT be read
	}

	// (i) exact recorded outcome, via a spy: the immediate verify stays SILENT on an unobservable post-state —
	// it neither launders a clean run nor prematurely resets the streak (TG-550). The fail-closed decision is
	// the decider's.
	spy := &spyGradRecorder{}
	rSpy := goodRequest(t)
	rSpy.Observe = unobservable
	out, err := actuatingInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).
		WithGraduationRecorder(spy).Do(ctx, rSpy)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed {
		t.Fatalf("the action still EXECUTED (only its verification was blind) — must report Executed: %+v", out)
	}
	if out.Verdict != "" {
		t.Fatalf("an unobservable post-state must WITHHOLD the verdict (empty), got %q", out.Verdict)
	}
	if spy.calls != 0 {
		t.Fatalf("the immediate verify must stay SILENT on an unobservable post-state (TG-550) — never a clean credit, never a premature reset; the fail-closed decision belongs to the decider, got calls=%d outcome=%v", spy.calls, spy.lastOut)
	}

	// (ii) ladder side effect: a real ladder must NOT accrue a clean run or stamp verified_clean.
	ladder := policy.NewLadder(policy.DefaultPromoteThreshold, policy.NewMemGraduationStore(), nil)
	rLad := goodRequest(t) // fresh manifest — the lifecycle chain is per-action
	// A FRESH pre-ok/post-fail closure: reusing the one above would fail its PRE read too (its counter is
	// already spent), the baseline gate would refuse, and CleanRunCount==0 would pass for the wrong reason —
	// an action that never executed proves nothing about how an executed one is credited.
	ladCall := 0
	rLad.Observe = func(context.Context) ([]verify.ObservedAlert, bool) {
		ladCall++
		if ladCall == 1 {
			return []verify.ObservedAlert{}, true
		}
		return nil, false
	}
	if _, err := actuatingInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).
		WithGraduationRecorder(ladder).Do(ctx, rLad); err != nil {
		t.Fatal(err)
	}
	st := ladder.State(ctx, "restart-service")
	if st.CleanRunCount != 0 {
		t.Fatalf("an unobservable run must NOT advance the clean-run count, got %d", st.CleanRunCount)
	}
	if st.LastOutcome == policy.OutcomeVerifiedClean {
		t.Fatalf("an unobservable run must NOT be recorded verified_clean (fail-open regression)")
	}
}

// (c3) TG-550: the immediate ~1s verify must not PREMATURELY RESET a pre-existing consecutive-clean streak on
// an unobservable post-state. A slow-settling heal (a guest still booting at T+1s) is unobservable that fast,
// but its own decider (commit-confirm window / session terminus) credits it clean moments later once the
// effect surfaces — resetting here cancelled that credit and capped the streak below the promote threshold
// forever (13 verified-clean start-guest heals stuck oscillating at clean_run_count 2). The immediate verify
// touches the ladder ONLY on a deviation; an unobservable run leaves the streak intact.
func TestImmediateVerifyDoesNotResetTheStreakOnUnobservable(t *testing.T) {
	ctx := context.Background()
	// Seed a ladder mid-climb: three consecutive clean runs already earned toward the threshold.
	ladder := policy.NewLadder(policy.DefaultPromoteThreshold, policy.NewMemGraduationStore(), nil)
	for i := 0; i < 3; i++ {
		if _, err := ladder.Record(ctx, "restart-service", policy.OutcomeVerifiedClean); err != nil {
			t.Fatal(err)
		}
	}
	if got := ladder.State(ctx, "restart-service").CleanRunCount; got != 3 {
		t.Fatalf("precondition: the seeded streak must be 3, got %d", got)
	}

	// An executed action whose post-state cannot be observed at the immediate verify (post read fails).
	call := 0
	r := goodRequest(t)
	r.Observe = func(context.Context) ([]verify.ObservedAlert, bool) {
		call++
		if call == 1 {
			return []verify.ObservedAlert{}, true // pre-execute baseline: established, quiet
		}
		return nil, false // post-state: could NOT be read
	}
	out, err := actuatingInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).
		WithGraduationRecorder(ladder).Do(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed {
		t.Fatalf("the action executed (only its verify was blind): %+v", out)
	}
	// THE FIX: the streak is intact — the immediate verify did not reset it. The decider credits (or resets)
	// this run later, past the monitoring refresh; that is its job, not the ~1s verify's.
	if got := ladder.State(ctx, "restart-service").CleanRunCount; got != 3 {
		t.Fatalf("an unobservable immediate verify must NOT reset the streak (TG-550), got clean_run_count=%d (want 3)", got)
	}
}

// (d) A REFUSED / withheld action does NOT touch the ladder — autonomy is only ever earned by an action that
// actually executed and verified. Both a mutation-off (withheld at the mode chokepoint) and an admission
// refuse (ungated) are proven to record nothing.
func TestRefusedActionDoesNotRecordGraduation(t *testing.T) {
	ctx := context.Background()

	t.Run("mutation off (withheld at the mode chokepoint)", func(t *testing.T) {
		spy := &spyGradRecorder{}
		act := &fakeActuator{}
		i := NewInterceptor(safety.NewReadOnlyChokepoint(), act, audit.NewLedger()).
			WithGraduationRecorder(spy)
		out, err := i.Do(ctx, goodRequest(t))
		if err != nil {
			t.Fatal(err)
		}
		if !out.Refused || act.execs != 0 {
			t.Fatalf("a read-only system must refuse and not execute: %+v execs=%d", out, act.execs)
		}
		if spy.calls != 0 {
			t.Fatalf("a withheld action must NOT advance the ladder, got %d record(s)", spy.calls)
		}
	})

	t.Run("admission refuse (ungated)", func(t *testing.T) {
		spy := &spyGradRecorder{}
		act := &fakeActuator{}
		i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
			WithGraduationRecorder(spy)
		r := goodRequest(t)
		r.Gated = false // no committed prediction — refused at the structure gate before execute
		out, err := i.Do(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if !out.Refused || act.execs != 0 {
			t.Fatalf("an ungated action must refuse and not execute: %+v execs=%d", out, act.execs)
		}
		if spy.calls != 0 {
			t.Fatalf("a refused action must NOT advance the ladder, got %d record(s)", spy.calls)
		}
	})
}

// (e) A nil recorder is a documented no-op — the interceptor executes exactly as before (no regression). This
// guards the "wired everywhere but optional" contract: the seam being absent must never change actuation.
func TestNilGraduationRecorderIsNoOp(t *testing.T) {
	ctx := context.Background()
	act := &fakeActuator{}
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()) // no WithGraduationRecorder
	out, err := i.Do(ctx, goodRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || act.execs != 1 || out.Verdict != safety.VerdictMatch {
		t.Fatalf("a nil recorder must not regress execution: %+v execs=%d", out, act.execs)
	}
}

// A record ERROR is NON-FATAL to the already-executed, already-audited action (it cannot be un-run): Do still
// returns the executed outcome; the failure is swallowed after being recorded to the ledger.
func TestGraduationRecordErrorIsNonFatal(t *testing.T) {
	ctx := context.Background()
	rec := &errGradRecorder{}
	act := &fakeActuator{}
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
		WithGraduationRecorder(rec)
	// A deviation, because that is what the interceptor still records immediately (a match defers to the
	// terminus, so it would attempt no record at all and this error path would never be reached).
	out, err := i.Do(ctx, deviationRequest(t))
	if err != nil {
		t.Fatalf("a graduation record error must not fail the executed action, got err=%v", err)
	}
	if !out.Executed || act.execs != 1 {
		t.Fatalf("the action must still execute despite a record error: %+v execs=%d", out, act.execs)
	}
	if rec.calls != 1 {
		t.Fatalf("the interceptor must attempt exactly one record, got %d", rec.calls)
	}
}
