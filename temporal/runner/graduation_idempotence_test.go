package runner

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// THE LADDER WRITE IS NOT IDEMPOTENT, SO IT MUST NOT SHARE A RETRYING ACTIVITY.
//
// Shipped this morning, the graduation credit moved from the execute activity (pinned MaximumAttempts:1,
// with a comment stating the rule as law) into ReconcileActivity, which runs at MaximumAttempts:4. Ladder
// .Record does a bare CleanRunCount++ with no dedupe key, so ONE confirmed heal that timed out mid-activity
// could be credited up to FOUR times — four of the five runs an op-class needs to reach AUTO, from a single
// incident. Nothing on the estate had been over-promoted when this was found (start-guest earned AUTO on
// 2026-07-22, five days before the regression), but the earn path was live and accruing.

type countingLadder struct {
	calls  int
	clean  []bool
	opClas []string
	refs   []string
}

func (c *countingLadder) record(_ context.Context, opClass, externalRef string, clean bool) error {
	c.calls++
	c.opClas = append(c.opClas, opClass)
	c.clean = append(c.clean, clean)
	c.refs = append(c.refs, externalRef)
	return nil
}

func cleanTerminus() ReconcileInput {
	return ReconcileInput{
		ExternalRef: "librenms-1", OpClass: "restart-container", Executed: true,
		HasTerminalResult: true, HasVerdict: true, Verdict: safety.VerdictMatch, ConfirmedClear: true,
	}
}

// ReconcileActivity must NOT write the ladder when the workflow has taken over the dispatch. Without this,
// the versioned path would credit twice per terminus — once in its own activity, once inline.
func TestReconcileDoesNotDoubleWriteWhenGraduationIsDispatchedSeparately(t *testing.T) {
	c := &countingLadder{}
	a := &Activities{D: Deps{RecordGraduation: c.record}}
	in := cleanTerminus()
	in.SkipGraduation = true

	if _, err := a.ReconcileActivity(context.Background(), in); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if c.calls != 0 {
		t.Fatalf("ReconcileActivity must not write the ladder when SkipGraduation is set — the workflow already "+
			"dispatched it as its own single-attempt activity; got %d write(s)", c.calls)
	}
}

// MUTATION CONTROL for the legacy path: an execution that started BEFORE the version guard has no separate
// activity in its history, so ReconcileActivity must still write inline. Losing this would silently stop
// crediting every in-flight workflow.
func TestLegacyExecutionStillCreditsInline(t *testing.T) {
	c := &countingLadder{}
	a := &Activities{D: Deps{RecordGraduation: c.record}}
	if _, err := a.ReconcileActivity(context.Background(), cleanTerminus()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if c.calls != 1 {
		t.Fatalf("a replaying legacy execution (SkipGraduation false) must still credit inline, got %d", c.calls)
	}
	if !c.clean[0] {
		t.Fatal("a confirmed-clean terminus must credit a CLEAN run")
	}
}

// The dedicated activity credits exactly once per invocation, and carries the same clean/not-clean decision.
func TestGraduationActivityCreditsExactlyOnce(t *testing.T) {
	c := &countingLadder{}
	a := &Activities{D: Deps{RecordGraduation: c.record}}
	if err := a.GraduationActivity(context.Background(), cleanTerminus()); err != nil {
		t.Fatalf("graduation: %v", err)
	}
	if c.calls != 1 || !c.clean[0] || c.opClas[0] != "restart-container" {
		t.Fatalf("want exactly one CLEAN credit for restart-container, got calls=%d clean=%v op=%v",
			c.calls, c.clean, c.opClas)
	}
}

// An unanswered poll or a missing verdict is NOT a clean run — the credit must be recorded as not-clean
// rather than skipped, so a streak is broken rather than silently preserved.
func TestUnconfirmedTerminusIsCreditedNotClean(t *testing.T) {
	for name, mut := range map[string]func(*ReconcileInput){
		"poll unanswered":  func(i *ReconcileInput) { i.PollUnanswered = true },
		"no verdict":       func(i *ReconcileInput) { i.HasVerdict = false },
		"not confirmed":    func(i *ReconcileInput) { i.ConfirmedClear = false },
		"verdict deviated": func(i *ReconcileInput) { i.Verdict = safety.VerdictDeviation },
	} {
		t.Run(name, func(t *testing.T) {
			c := &countingLadder{}
			a := &Activities{D: Deps{RecordGraduation: c.record}}
			in := cleanTerminus()
			mut(&in)
			if err := a.GraduationActivity(context.Background(), in); err != nil {
				t.Fatalf("graduation: %v", err)
			}
			if c.calls != 1 {
				t.Fatalf("want one credit, got %d", c.calls)
			}
			if c.clean[0] {
				t.Fatal("this terminus is NOT confirmed clean and must not be credited as a clean run")
			}
		})
	}
}

// THE SECOND LAYER (TG-266). The tests above pin WHERE the credit call sits — a single-attempt activity,
// never the retrying one. That is a convention about call placement, and it cannot survive what it does
// not see: a workflow replay after a worker restart, a resumed session, or the same incident re-run. The
// durable claim (graduation_credit, REQ-2804) is the structural layer beneath it, and it needs the session
// ref to key on — so the ref must actually REACH the seam.
//
// KILLING MUTATION: drop ExternalRef from the RecordGraduation call in recordGraduationCredit (pass ""),
// and the durable claim below it has no key to dedupe by — every replay credits again.
func TestTheSessionRefReachesTheGraduationSeam(t *testing.T) {
	c := &countingLadder{}
	a := &Activities{D: Deps{RecordGraduation: c.record}}
	in := cleanTerminus()

	if _, err := a.ReconcileActivity(context.Background(), in); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if c.calls != 1 {
		t.Fatalf("want exactly one credit call, got %d", c.calls)
	}
	if len(c.refs) != 1 || c.refs[0] != in.ExternalRef {
		t.Fatalf("the seam received ref %q, want %q — without the session ref the exactly-once claim has "+
			"no key and every replay credits again (TG-266/REQ-2804)", c.refs, in.ExternalRef)
	}
}
