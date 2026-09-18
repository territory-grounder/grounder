package runner

import (
	"context"
	"errors"
	"testing"
)

// THE A3 DENOMINATOR MUST COUNT WHAT ACTUALLY RAN.
//
// session_triage's insert is ON CONFLICT (external_ref) DO NOTHING — FIRST-WRITE-WINS. A session that pauses
// for a human vote records its triage row BEFORE it executes, so the post-execute write is silently discarded
// and `mutated` keeps its propose-time value: FALSE, for an action that demonstrably ran.
//
// That decides which incidents enter the published numbers: `mutated` is the A3 heal-rate DENOMINATOR and half
// the A7 ineffective-actuation bound. Measured live the day this was found: 94 real executions in
// action_execution against 91 triage rows marked mutated — and the three missing were NOT random. They were
// exactly the three SSH-lane heals (restart-container x2, start-service x1), which pause for a vote, while all
// 91 Proxmox-lane start-guest heals execute straight through and record correctly. The bias is OP-CLASS
// CORRELATED: the benchmark counted the lane that never waits and dropped every lane that does — which is
// exactly the breadth evidence P4 exists to accrue.

type markSpy struct {
	clearedRef string
	cleared    bool
	mutatedRef string
	mutatedN   int
	failMutate error
}

func (m *markSpy) markCleared(_ context.Context, ref string, cleared bool) error {
	m.clearedRef, m.cleared = ref, cleared
	return nil
}
func (m *markSpy) markMutated(_ context.Context, ref string) error {
	m.mutatedRef = ref
	m.mutatedN++
	return m.failMutate
}

func (m *markSpy) deps() Deps {
	return Deps{TriageMarkCleared: m.markCleared, TriageMarkMutated: m.markMutated}
}

// THE FIX: the terminus back-fills the denominator for an executed session.
func TestTerminusBackfillsMutated(t *testing.T) {
	m := &markSpy{}
	in := MarkClearInput{ExternalRef: "librenms-dc1-179115", Cleared: true, Mutated: true}
	if _, err := NewActivities(m.deps()).MarkTriageClearedActivity(context.Background(), in); err != nil {
		t.Fatalf("activity: %v", err)
	}
	if m.mutatedN != 1 || m.mutatedRef != in.ExternalRef {
		t.Fatalf("an executed session must have its A3 denominator back-filled exactly once, got n=%d ref=%q",
			m.mutatedN, m.mutatedRef)
	}
	if m.clearedRef != in.ExternalRef || !m.cleared {
		t.Errorf("the confirmed-clear mark must still happen: ref=%q cleared=%v", m.clearedRef, m.cleared)
	}
}

// A session that executed NOTHING must not be counted as a mutation — the denominator would then over-count,
// which is the opposite error and just as wrong.
func TestTerminusDoesNotInventAMutation(t *testing.T) {
	m := &markSpy{}
	in := MarkClearInput{ExternalRef: "ref-2", Cleared: true, Mutated: false}
	if _, err := NewActivities(m.deps()).MarkTriageClearedActivity(context.Background(), in); err != nil {
		t.Fatalf("activity: %v", err)
	}
	if m.mutatedN != 0 {
		t.Fatalf("nothing executed, so the denominator must not be touched; got %d call(s)", m.mutatedN)
	}
}

// Fail-open, exactly like the confirmed-clear mark beside it: this feeds the offline scorer, never
// authorization, so a persistence outage must never fail a completed session.
func TestTerminusMutatedMarkIsFailOpen(t *testing.T) {
	m := &markSpy{failMutate: errors.New("db down")}
	res, err := NewActivities(m.deps()).MarkTriageClearedActivity(context.Background(),
		MarkClearInput{ExternalRef: "ref-3", Cleared: true, Mutated: true})
	if err != nil {
		t.Fatalf("a write error must not fail the session: %v", err)
	}
	if !res.Recorded {
		t.Errorf("the clear mark succeeded, so the activity still reports recorded; got %+v", res)
	}
	// A nil seam is a documented no-op and must not panic.
	if _, err := NewActivities(Deps{TriageMarkCleared: m.markCleared}).MarkTriageClearedActivity(
		context.Background(), MarkClearInput{ExternalRef: "ref-4", Cleared: true, Mutated: true}); err != nil {
		t.Fatalf("a nil mark-mutated seam must be a no-op: %v", err)
	}
}

// MUTATION CONTROL. The guard is only load-bearing if Mutated actually drives the call. Flip that ONE field on
// otherwise identical input and the behaviour must differ — if both paths call the same number of times, the
// field is being ignored and every assertion above is vacuous.
func TestMutationControl_MutatedFieldDrivesTheBackfill(t *testing.T) {
	on, off := &markSpy{}, &markSpy{}
	base := MarkClearInput{ExternalRef: "same-ref", Cleared: true}

	yes := base
	yes.Mutated = true
	if _, err := NewActivities(on.deps()).MarkTriageClearedActivity(context.Background(), yes); err != nil {
		t.Fatal(err)
	}
	no := base
	no.Mutated = false
	if _, err := NewActivities(off.deps()).MarkTriageClearedActivity(context.Background(), no); err != nil {
		t.Fatal(err)
	}
	if on.mutatedN == off.mutatedN {
		t.Fatalf("flipping ONLY Mutated must change the back-fill (both got %d) — the field is ignored and "+
			"the tests above prove nothing", on.mutatedN)
	}
	if on.mutatedN != 1 || off.mutatedN != 0 {
		t.Fatalf("want exactly one back-fill when executed and none when not: on=%d off=%d", on.mutatedN, off.mutatedN)
	}
}
