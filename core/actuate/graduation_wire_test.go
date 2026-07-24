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
	r.Observe = func(context.Context) []verify.ObservedAlert {
		call++
		if call == 1 {
			return []verify.ObservedAlert{} // pre-execute baseline: quiet
		}
		return []verify.ObservedAlert{{Host: "surprise99", Rule: "HostDown", Site: "nl"}} // post: the cascade appears
	}
	return r
}

// (a) A verified-`match` executed action feeds ONE verified-clean run to the ladder — the clean-run count
// increments from 0 to 1 (the increment that was dead before this wiring).
func TestGraduationRecordsVerifiedCleanRun(t *testing.T) {
	ctx := context.Background()
	ladder := policy.NewLadder(policy.DefaultPromoteThreshold, policy.NewMemGraduationStore(), nil)
	i := NewInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).
		WithGraduationRecorder(ladder)

	out, err := i.Do(ctx, goodRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || out.Verdict != safety.VerdictMatch {
		t.Fatalf("an admissible action must execute and verify match: %+v", out)
	}
	st := ladder.State(ctx, "restart-service")
	if st.Level != policy.LevelApprove || st.CleanRunCount != 1 {
		t.Fatalf("a verified-clean run must accrue one clean run at approve, got level=%v count=%d", st.Level, st.CleanRunCount)
	}
	if st.LastOutcome != policy.OutcomeVerifiedClean {
		t.Fatalf("a verified match must record OutcomeVerifiedClean, got %v", st.LastOutcome)
	}
}

// (b) Five consecutive verified-clean runs PROMOTE the class approve→auto: LevelOf flips to auto and
// GraduatedVerdict now honors an `auto` rule verdict (before promotion it downgrades auto→approve). This is
// the whole point — a class can finally graduate.
func TestGraduationPromotesAfterThresholdCleanRuns(t *testing.T) {
	ctx := context.Background()
	ladder := policy.NewLadder(policy.DefaultPromoteThreshold, policy.NewMemGraduationStore(), nil)
	i := NewInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).
		WithGraduationRecorder(ladder)

	for run := 1; run <= policy.DefaultPromoteThreshold; run++ {
		out, err := i.Do(ctx, goodRequest(t)) // fresh manifest each run — the lifecycle chain is per-action
		if err != nil {
			t.Fatal(err)
		}
		if !out.Executed || out.Verdict != safety.VerdictMatch {
			t.Fatalf("run %d must execute and verify match: %+v", run, out)
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
			if st.Level != policy.LevelAuto {
				t.Fatalf("after %d clean runs the class must be promoted to auto, got level=%v", run, st.Level)
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
	i := NewInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).
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
	t.Run("match → verified-clean", func(t *testing.T) {
		spy := &spyGradRecorder{}
		i := NewInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).
			WithGraduationRecorder(spy)
		if _, err := i.Do(ctx, goodRequest(t)); err != nil {
			t.Fatal(err)
		}
		if spy.calls != 1 || spy.lastOp != "restart-service" || spy.lastOut != policy.OutcomeVerifiedClean {
			t.Fatalf("a verified match must record (restart-service, verified_clean), got calls=%d op=%q outcome=%v", spy.calls, spy.lastOp, spy.lastOut)
		}
	})
	t.Run("deviation → deviated", func(t *testing.T) {
		spy := &spyGradRecorder{}
		i := NewInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).
			WithGraduationRecorder(spy)
		if _, err := i.Do(ctx, deviationRequest(t)); err != nil {
			t.Fatal(err)
		}
		if spy.calls != 1 || spy.lastOut != policy.OutcomeDeviated {
			t.Fatalf("a verified deviation must record OutcomeDeviated, got calls=%d outcome=%v", spy.calls, spy.lastOut)
		}
	})
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
		i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
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
	i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()) // no WithGraduationRecorder
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
	i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
		WithGraduationRecorder(rec)
	out, err := i.Do(ctx, goodRequest(t))
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
