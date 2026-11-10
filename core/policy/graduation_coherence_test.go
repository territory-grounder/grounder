package policy

import (
	"context"
	"testing"
)

// TG-666 (F22): two worker processes, two Ladders, ONE durable store. The actuation plane records the run that moves
// the rung; the triage plane — which classifies and judges the REQ-2905 canary mandate — must read the rung now in
// force, not the one it cached at first touch.
// KILLING MUTATION: restore the first-touch cache in stateLocked (return l.states[opClass] when present) — RED here.
func TestATriagePlaneLadderReadsARungPromotedByAnotherProcess(t *testing.T) {
	ctx := context.Background()
	store := NewMemGraduationStore()
	const oc = "start-service"
	triage := NewLadder(1, store, nil)  // the reader (classification + the mandate's rung)
	actuate := NewLadder(1, store, nil) // the writer (the interceptor's earn path)

	if got := triage.LevelOf(ctx, oc); got != LevelApprove {
		t.Fatalf("control: a fresh class starts at approve, got %v", got)
	}
	// The other process records a verified-clean run: threshold 1 ⇒ approve → auto_notice, in the store.
	res, err := actuate.Record(ctx, oc, OutcomeVerifiedClean)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Promoted {
		t.Fatalf("control: the writer must promote at threshold 1, got %+v", res)
	}
	if got := actuate.LevelOf(ctx, oc); got != LevelAutoNotice {
		t.Fatalf("control: the writer reads its own promotion, got %v", got)
	}
	// The reader touched the class BEFORE the promotion; it must still see the rung in force now.
	if got := triage.LevelOf(ctx, oc); got != LevelAutoNotice {
		t.Fatalf("the triage plane must read the durable rung (auto_notice), not its first-touch cache — got %v", got)
	}

	// And a demotion made elsewhere reaches the reader too (a deviation drops to approve).
	if _, err := actuate.Record(ctx, oc, OutcomeDeviated); err != nil {
		t.Fatal(err)
	}
	if got := triage.LevelOf(ctx, oc); got != LevelApprove {
		t.Fatalf("a demotion recorded by another process must reach the reader, got %v", got)
	}
}

// TG-666 part 4: every rung change that durably landed is reported to the transition hook — a promotion, a demotion
// — and a streak-only record is not. KILLING MUTATION: drop the hook call in Record — RED here.
func TestEveryDurableRungChangeIsReportedToTheTransitionHook(t *testing.T) {
	ctx := context.Background()
	var got []RecordResult
	l := NewLadder(2, NewMemGraduationStore(), nil).WithTransitionHook(func(r RecordResult) { got = append(got, r) })
	const oc = "start-service"

	if _, err := l.Record(ctx, oc, OutcomeVerifiedClean); err != nil { // 1/2: streak only
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a streak-only record is not a transition, got %+v", got)
	}
	if _, err := l.Record(ctx, oc, OutcomeVerifiedClean); err != nil { // 2/2: approve → auto_notice
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Promoted || got[0].From != LevelApprove || got[0].To != LevelAutoNotice {
		t.Fatalf("the promotion must be reported once with from/to, got %+v", got)
	}
	if _, err := l.Record(ctx, oc, OutcomeDeviated); err != nil { // auto_notice → approve
		t.Fatal(err)
	}
	if len(got) != 2 || !got[1].Demoted || got[1].To != LevelApprove {
		t.Fatalf("the demotion must be reported, got %+v", got)
	}
}
