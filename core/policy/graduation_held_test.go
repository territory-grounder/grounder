package policy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// heldTestStore wraps the in-memory store so an oracle can fail Save and stall Load (TG-666 review F1/F2).
type heldTestStore struct {
	*MemGraduationStore
	saveErr   error
	stallLoad bool
}

func (s *heldTestStore) Save(ctx context.Context, st ClassState) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.MemGraduationStore.Save(ctx, st)
}

func (s *heldTestStore) Load(ctx context.Context, opClass string) (ClassState, error) {
	if s.stallLoad {
		select {
		case <-ctx.Done():
			return ClassState{}, ctx.Err()
		case <-time.After(2 * time.Second): // an unbounded read eventually answers with the stale row
		}
	}
	return s.MemGraduationStore.Load(ctx, opClass)
}

// A deviation whose Save fails must stay in force on the very next read. Every read reloads the durable row
// (F22), so before the hold the class read `auto` again one call later — the demotion was silently lost.
// KILLING MUTATION: drop `l.held[opClass] = next` in record — RED here.
func TestADemotionWhoseSaveFailsStaysInForceOnTheNextRead(t *testing.T) {
	ctx := context.Background()
	const oc = "start-service"
	store := &heldTestStore{MemGraduationStore: NewMemGraduationStore().Seed(ClassState{OpClass: oc, Level: LevelAuto})}
	var reported []RecordResult
	l := NewLadder(1, store, nil).WithTransitionHook(func(r RecordResult) { reported = append(reported, r) })

	store.saveErr = errors.New("db: connection reset")
	res, err := l.Record(ctx, oc, OutcomeDeviated)
	if err == nil || !res.Demoted {
		t.Fatalf("control: a failed Save of a demotion returns the error with the applied result, got %+v / %v", res, err)
	}
	if got := l.LevelOf(ctx, oc); got != LevelApprove {
		t.Fatalf("the unpersisted demotion must hold on the next read, got %v (the durable row still says auto)", got)
	}
	if len(reported) != 1 || !reported[0].Demoted || !strings.Contains(reported[0].Reason, "NOT persisted") {
		t.Fatalf("an in-force but unpersisted demotion must be reported, saying so, got %+v", reported)
	}

	// The store recovers: the next Record builds on the HELD state, so its write lands the demotion durably.
	store.saveErr = nil
	if _, err := l.Record(ctx, oc, OutcomeVerifiedClean); err != nil {
		t.Fatal(err)
	}
	durable, err := store.MemGraduationStore.Load(ctx, oc)
	if err != nil {
		t.Fatal(err)
	}
	if durable.Level == LevelAuto {
		t.Fatalf("the next successful write must carry the held demotion, the durable row still says auto: %+v", durable)
	}
	if len(l.held) != 0 {
		t.Fatalf("a landed write clears the hold, still held: %+v", l.held)
	}
}

// An UNREADABLE row is not evidence the demotion landed: it fails closed to approve on that read, and the hold
// must survive it, or the next good read would serve the stale `auto` again.
// KILLING MUTATION: drop `loaded.Version > 0 &&` in withHeldLocked — RED here.
func TestAnUnreadableRowNeverRetiresAHeldDemotion(t *testing.T) {
	ctx := context.Background()
	const oc = "start-container"
	mem := NewMemGraduationStore().Seed(ClassState{OpClass: oc, Level: LevelAuto})
	store := &heldTestStore{MemGraduationStore: mem, saveErr: errors.New("db down")}
	l := NewLadder(1, store, nil)
	if _, err := l.Record(ctx, oc, OutcomeDeviated); err == nil {
		t.Fatal("control: the Save must fail")
	}
	mem.WithLoadError(errors.New("db down"))
	if got := l.LevelOf(ctx, oc); got != LevelApprove {
		t.Fatalf("an unreadable row fails closed, got %v", got)
	}
	mem.WithLoadError(nil) // readable again, and still `auto`: the demotion never landed
	if got := l.LevelOf(ctx, oc); got != LevelApprove {
		t.Fatalf("the hold must survive an unreadable read; the durable row still says auto, got %v", got)
	}
}

// The hold is not a permanent override: once a demotion durably lands elsewhere (a peer process), the durable row
// is authoritative again, including a later legitimate re-climb.
// KILLING MUTATION: never delete the hold in withHeldLocked — RED here (the re-climb is masked).
func TestADurableDemotionRetiresTheHoldAndTheRowIsAuthoritativeAgain(t *testing.T) {
	ctx := context.Background()
	const oc = "restart-service"
	mem := NewMemGraduationStore().Seed(ClassState{OpClass: oc, Level: LevelAuto})
	failing := &heldTestStore{MemGraduationStore: mem, saveErr: errors.New("db: timeout")}
	mine := NewLadder(1, failing, nil)
	if _, err := mine.Record(ctx, oc, OutcomeDeviated); err == nil {
		t.Fatal("control: the Save must fail")
	}
	peer := NewLadder(1, mem, nil)
	if _, err := peer.Record(ctx, oc, OutcomeDeviated); err != nil { // the peer's demotion lands
		t.Fatal(err)
	}
	if got := mine.LevelOf(ctx, oc); got != LevelApprove || len(mine.held) != 0 {
		t.Fatalf("a durably landed demotion retires the hold, got %v held=%+v", got, mine.held)
	}
	if _, err := peer.Record(ctx, oc, OutcomeVerifiedClean); err != nil { // threshold 1: approve → auto_notice
		t.Fatal(err)
	}
	if got := mine.LevelOf(ctx, oc); got != LevelAutoNotice {
		t.Fatalf("after a durable demotion and a legitimate re-climb the row is authoritative, got %v", got)
	}
}

// A stalled store read must not stall the gate: it times out and fails closed to approve (review F2).
// KILLING MUTATION: load with the caller's ctx (drop the WithTimeout) — RED here (the stale `auto` row answers).
func TestAStalledLadderReadFailsClosedWithinTheBound(t *testing.T) {
	old := ladderLoadTimeout
	ladderLoadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { ladderLoadTimeout = old })
	const oc = "start-guest"
	store := &heldTestStore{MemGraduationStore: NewMemGraduationStore().Seed(ClassState{OpClass: oc, Level: LevelAuto}), stallLoad: true}
	l := NewLadder(1, store, nil)
	start := time.Now()
	if got := l.LevelOf(context.Background(), oc); got != LevelApprove {
		t.Fatalf("a stalled read must fail closed to approve, got %v", got)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the read must be bounded, took %v", d)
	}
}
