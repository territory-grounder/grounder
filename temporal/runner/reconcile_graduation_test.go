package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// GRADUATION IS DECIDED AT THE TERMINUS, NOT AT EXECUTE (REQ-1223).
//
// The interceptor's post-execution verdict is taken ~1s after the effect against a monitoring surface whose
// own poll cycle is minutes long, with a baseline that subtracts every alert already firing — so `match` is
// very nearly structural and cannot distinguish a heal that WORKED from one whose consequences had not
// surfaced yet. Promoting on it let an op-class climb toward AUTO on a non-signal.
//
// The promote therefore moved HERE, to the same confirmed-clean facts the novelty writeback already trusts to
// remove a human's first-sight review. These tests are the other half of that change: the interceptor tests
// prove it no longer promotes, and these prove something still does — without them the fix would silently
// disable graduation altogether.

func gradInput() ReconcileInput {
	in := cleanWritebackInput()
	in.OpClass = "restart-container"
	return in
}

type gradSpy struct {
	calls    int
	op       string
	clean    bool
	failWith error
}

func (g *gradSpy) record(_ context.Context, opClass, externalRef string, clean bool) error {
	g.calls++
	g.op, g.clean = opClass, clean
	return g.failWith
}

// A confirmed-clean terminus feeds exactly one CLEAN run for the session's op-class.
func TestReconcileFeedsCleanRunOnConfirmedClean(t *testing.T) {
	g := &gradSpy{}
	acts := NewActivities(Deps{RecordGraduation: g.record})
	if _, err := acts.ReconcileActivity(context.Background(), gradInput()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if g.calls != 1 {
		t.Fatalf("a confirmed-clean terminus must feed the ladder exactly once, got %d calls", g.calls)
	}
	if g.op != "restart-container" {
		t.Errorf("the session's op-class must be the one credited, got %q", g.op)
	}
	if !g.clean {
		t.Fatal("a confirmed-clean terminus IS the clean run — without this nothing can ever promote again")
	}
}

// Every precondition is load-bearing: falsify exactly one and the run must be fed as NOT clean. It is still
// FED (an executed session that could not be confirmed breaks the consecutive-clean streak) — it just must
// never count toward autonomy.
func TestReconcileFeedsNotCleanWhenAnyPreconditionFails(t *testing.T) {
	deviation := gradInput()
	deviation.Verdict = safety.VerdictDeviation

	partial := gradInput()
	partial.Verdict = safety.VerdictPartial

	unconfirmed := gradInput()
	unconfirmed.ConfirmedClear = false // the heal verified `match` but never stayed clear

	noVerdict := gradInput()
	noVerdict.HasVerdict = false
	noVerdict.Verdict = ""

	orphanedPoll := gradInput()
	orphanedPoll.PollUnanswered = true

	for name, in := range map[string]ReconcileInput{
		"deviation":         deviation,
		"partial":           partial,
		"unconfirmed-clear": unconfirmed,
		"no-verdict":        noVerdict,
		"orphaned-poll":     orphanedPoll,
	} {
		t.Run(name, func(t *testing.T) {
			g := &gradSpy{}
			acts := NewActivities(Deps{RecordGraduation: g.record})
			if _, err := acts.ReconcileActivity(context.Background(), in); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if g.calls != 1 {
				t.Fatalf("an executed terminus is still fed (to break the streak), got %d calls", g.calls)
			}
			if g.clean {
				t.Fatalf("%s must NOT count as a clean run toward autonomy", name)
			}
		})
	}
}

// A session that never executed writes NOTHING: it neither earns nor penalises, because no mutation happened.
func TestReconcileFeedsNothingWhenNothingExecuted(t *testing.T) {
	in := gradInput()
	in.Executed = false
	g := &gradSpy{}
	acts := NewActivities(Deps{RecordGraduation: g.record})
	if _, err := acts.ReconcileActivity(context.Background(), in); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if g.calls != 0 {
		t.Fatalf("no mutation happened, so the ladder must not be touched at all; got %d calls", g.calls)
	}
}

// An absent op-class writes nothing (fail closed), and a nil seam is a documented no-op that must not break
// the terminus. A ladder write error is best-effort: it never fails the session.
func TestReconcileGraduationIsFailSafe(t *testing.T) {
	t.Run("no op-class → no write", func(t *testing.T) {
		in := gradInput()
		in.OpClass = ""
		g := &gradSpy{}
		acts := NewActivities(Deps{RecordGraduation: g.record})
		if _, err := acts.ReconcileActivity(context.Background(), in); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if g.calls != 0 {
			t.Fatalf("an unattributable run must not credit some other class; got %d calls", g.calls)
		}
	})
	t.Run("nil seam → no-op", func(t *testing.T) {
		acts := NewActivities(Deps{})
		if _, err := acts.ReconcileActivity(context.Background(), gradInput()); err != nil {
			t.Fatalf("a nil graduation seam must not break the terminus: %v", err)
		}
	})
	t.Run("write error → terminus unaffected", func(t *testing.T) {
		g := &gradSpy{failWith: errors.New("ladder down")}
		acts := NewActivities(Deps{RecordGraduation: g.record})
		if _, err := acts.ReconcileActivity(context.Background(), gradInput()); err != nil {
			t.Fatalf("a ladder write error is best-effort and must not fail the session: %v", err)
		}
		if g.calls != 1 {
			t.Fatalf("the write must still have been attempted, got %d", g.calls)
		}
	})
}

// MUTATION CONTROL. The suite above is only meaningful if `clean` actually tracks ConfirmedClear rather than
// being hardcoded true by a passing-but-wrong implementation. This asserts the two differ on the SAME input
// with a single field flipped — if both come back identical, the predicate is not reading ConfirmedClear and
// every assertion above is vacuous.
func TestMutationControl_CleanTracksConfirmedClear(t *testing.T) {
	on, off := &gradSpy{}, &gradSpy{}

	yes := gradInput()
	acts := NewActivities(Deps{RecordGraduation: on.record})
	if _, err := acts.ReconcileActivity(context.Background(), yes); err != nil {
		t.Fatal(err)
	}
	no := gradInput()
	no.ConfirmedClear = false
	acts2 := NewActivities(Deps{RecordGraduation: off.record})
	if _, err := acts2.ReconcileActivity(context.Background(), no); err != nil {
		t.Fatal(err)
	}
	if on.clean == off.clean {
		t.Fatalf("flipping ONLY ConfirmedClear must change the recorded outcome (got %v both ways) — the "+
			"predicate is not reading it, so the tests above prove nothing", on.clean)
	}
	if !on.clean || off.clean {
		t.Fatalf("confirmed-clear must be the clean run and unconfirmed must not: on=%v off=%v", on.clean, off.clean)
	}
}
