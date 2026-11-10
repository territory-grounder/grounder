package policy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TG-666 part 3: an operator demotion lands in the durable row, so the OTHER process (the triage plane that
// classifies and judges the REQ-2905 mandate) reads approve on its next decision, and the change is reported with
// the operator's reason. KILLING MUTATION: leave next.Level at cur.Level in demote — RED here.
func TestAnOperatorDemotionDropsTheRungDurablyForEveryPlane(t *testing.T) {
	ctx := context.Background()
	const oc = "start-service"
	store := NewMemGraduationStore().Seed(ClassState{OpClass: oc, Level: LevelAuto})
	triage := NewLadder(5, store, nil)
	var got []RecordResult
	operator := NewLadder(5, store, nil).WithTransitionHook(func(r RecordResult) { got = append(got, r) })
	if triage.LevelOf(ctx, oc) != LevelAuto {
		t.Fatal("control: the class starts at auto")
	}
	res, err := operator.Demote(ctx, oc, "TG-666 audit: credits not reusable under the corrected predicate")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Demoted || res.From != LevelAuto || res.To != LevelApprove || !res.Operator {
		t.Fatalf("an operator demotion of an auto class reports auto → approve, got %+v", res)
	}
	if lv := triage.LevelOf(ctx, oc); lv != LevelApprove {
		t.Fatalf("the other plane must read the durable demotion, got %v", lv)
	}
	if len(got) != 1 || !got[0].Demoted || !strings.Contains(got[0].Reason, "TG-666 audit") {
		t.Fatalf("the demotion must reach the transition hook with the operator's reason, got %+v", got)
	}
}

// Restriction-only: at approve the operator write only zeroes a streak (reported as its own kind), and a class
// already at approve with no streak is a no-op with no ledger row. Nothing in Demote can raise a rung.
// KILLING MUTATION: drop the CleanRunCount reset — RED here.
func TestAnOperatorDemotionAtApproveOnlyRestartsTheStreak(t *testing.T) {
	ctx := context.Background()
	const oc = "restart-service"
	store := NewMemGraduationStore().Seed(ClassState{OpClass: oc, Level: LevelApprove, CleanRunCount: 3})
	var got []RecordResult
	l := NewLadder(5, store, nil).WithTransitionHook(func(r RecordResult) { got = append(got, r) })
	res, err := l.Demote(ctx, oc, "requalify from zero")
	if err != nil {
		t.Fatal(err)
	}
	if res.Demoted || !res.StreakReset {
		t.Fatalf("at approve the write is a streak reset, not a demotion, got %+v", res)
	}
	if st := l.State(ctx, oc); st.Level != LevelApprove || st.CleanRunCount != 0 {
		t.Fatalf("the streak must restart at zero, got %+v", st)
	}
	if len(got) != 1 || !got[0].StreakReset {
		t.Fatalf("a streak reset is reported, got %+v", got)
	}
	res, err = l.Demote(ctx, oc, "again")
	if err != nil || res.Demoted || res.StreakReset || len(got) != 1 {
		t.Fatalf("nothing to change ⇒ no-op and no hook, got %+v / %v / %d reports", res, err, len(got))
	}
}

// A demotion whose Save fails is held in this process and the error is returned so the operator retries.
// KILLING MUTATION: drop `l.held[opClass] = next` in demote — RED here.
func TestAnOperatorDemotionWhoseSaveFailsIsHeld(t *testing.T) {
	ctx := context.Background()
	const oc = "start-container"
	store := &heldTestStore{MemGraduationStore: NewMemGraduationStore().Seed(ClassState{OpClass: oc, Level: LevelAuto}),
		saveErr: errors.New("db: read-only transaction")}
	l := NewLadder(5, store, nil)
	res, err := l.Demote(ctx, oc, "audit")
	if err == nil || !res.Demoted {
		t.Fatalf("control: the failed Save is surfaced with the applied result, got %+v / %v", res, err)
	}
	if lv := l.LevelOf(ctx, oc); lv != LevelApprove {
		t.Fatalf("a failed operator demotion must stay in force in this process, got %v", lv)
	}
}

// TG-666 review S1: an unreadable row reads as a FABRICATED approve. Demote must not report "nothing to change" on
// it — it holds approve in-process, returns the error, and the retry (row readable again, still auto) lands it.
// KILLING MUTATION: drop the readErr branch in demote — RED here (the first call returns nil, nothing held).
func TestAnOperatorDemotionOnAnUnreadableRowIsHeldAndNeverReportedDone(t *testing.T) {
	ctx := context.Background()
	const oc = "start-service"
	mem := NewMemGraduationStore().Seed(ClassState{OpClass: oc, Level: LevelAuto})
	var got []RecordResult
	l := NewLadder(5, mem, nil).WithTransitionHook(func(r RecordResult) { got = append(got, r) })
	mem.WithLoadError(errors.New("db: connection refused"))
	if _, err := l.Demote(ctx, oc, "audit"); err == nil {
		t.Fatal("a demotion decided on an unreadable row must not report success")
	}
	mem.WithLoadError(nil) // readable again, and still auto: nothing landed
	if lv := l.LevelOf(ctx, oc); lv != LevelApprove {
		t.Fatalf("the demotion must be held in this process meanwhile, got %v", lv)
	}
	res, err := l.Demote(ctx, oc, "audit (retry)")
	if err != nil || !res.Demoted || res.From != LevelAuto {
		t.Fatalf("the retry must land the demotion from the durable auto, got %+v / %v", res, err)
	}
	if st, _ := mem.Load(ctx, oc); st.Level != LevelApprove {
		t.Fatalf("the durable row must now read approve, got %+v", st)
	}
	if len(got) != 1 || !got[0].Demoted {
		t.Fatalf("only the landed demotion is reported, got %+v", got)
	}
}

// absentOnLoadStore answers every Load the way the production store answers an unrecognised level spelling:
// ErrClassAbsent — while a row DOES exist (here: whatever the last Save wrote, invisible to Load).
type absentOnLoadStore struct{ saved []ClassState }

func (s *absentOnLoadStore) Load(_ context.Context, opClass string) (ClassState, error) {
	return ClassState{}, fmt.Errorf("%w: %q (corrupt level %q)", ErrClassAbsent, opClass, "auto_silent")
}
func (s *absentOnLoadStore) Save(_ context.Context, st ClassState) error {
	s.saved = append(s.saved, st)
	return nil
}

// TG-666 re-review: the production store answers ErrClassAbsent for a corrupt or unrecognised level, so "absent"
// is not proof the class is at approve. Demote must WRITE approve (unconditionally) and report it, never answer
// "nothing changed" over a row nobody could read. KILLING MUTATION: treat absent as a readable approve — RED here.
func TestAnOperatorDemotionWritesApproveOverAnUnreadableLevel(t *testing.T) {
	store := &absentOnLoadStore{}
	var got []RecordResult
	l := NewLadder(5, store, nil).WithTransitionHook(func(r RecordResult) { got = append(got, r) })
	res, err := l.Demote(context.Background(), "start-guest", "audit")
	if err != nil {
		t.Fatal(err)
	}
	if len(store.saved) != 1 || store.saved[0].Level != LevelApprove || store.saved[0].Version != 0 {
		t.Fatalf("approve must be written unconditionally over the unreadable row, got %+v", store.saved)
	}
	if !res.StreakReset || len(got) != 1 || !strings.Contains(got[0].Reason, "approve written") {
		t.Fatalf("the write must be reported, saying approve was written, got %+v / %+v", res, got)
	}
}
