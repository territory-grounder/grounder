package audit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// sharedLedgerStore models the ONE durable governance_ledger that several worker processes append to
// (the org-global chain). Like the pgx PRIMARY KEY on seq, a duplicate seq is rejected with
// ErrDuplicateSeq; and like the pgx LedgerStore it exposes Tail, so a Ledger backed by it can re-read the
// head and re-chain after a collision — exactly the recovery path TG-549 adds.
type sharedLedgerStore struct {
	bySeq    map[int64]LedgerEntry
	headSeq  int64
	headHash string
}

func newSharedLedgerStore() *sharedLedgerStore {
	return &sharedLedgerStore{bySeq: map[int64]LedgerEntry{}}
}

func (s *sharedLedgerStore) PersistContext(_ context.Context, e LedgerEntry) error {
	if _, taken := s.bySeq[e.Seq]; taken {
		return fmt.Errorf("%w: seq %d", ErrDuplicateSeq, e.Seq) // the seq PK is already occupied
	}
	s.bySeq[e.Seq] = e
	if e.Seq > s.headSeq {
		s.headSeq, s.headHash = e.Seq, e.Hash
	}
	return nil
}

func (s *sharedLedgerStore) Persist(e LedgerEntry) error {
	return s.PersistContext(context.Background(), e)
}

func (s *sharedLedgerStore) Tail(context.Context) (int64, string, error) {
	return s.headSeq, s.headHash, nil
}

// ordered returns the persisted rows in seq order — what a verifier reads back from the durable store.
func (s *sharedLedgerStore) ordered() []LedgerEntry {
	out := make([]LedgerEntry, 0, len(s.bySeq))
	for _, e := range s.bySeq {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// Two workers seed the SAME boot tail (the TG-549 shape: a simultaneous deploy). The first append wins the
// seq; the second collides on it, re-reads the advanced head, re-chains onto it, and succeeds — rather than
// failing closed and wedging its whole governance lane on a dead seq. The single global chain still verifies.
func TestAppendRecoversFromASiblingSeqCollision(t *testing.T) {
	store := newSharedLedgerStore()

	// A shared history 1..3 that both workers will seed from.
	seed := NewLedger().WithSink(store)
	var tail LedgerEntry
	for i := 0; i < 3; i++ {
		tail, _ = seed.Append(GovDecision{Decision: "classify:AUTO", ActionID: "seed"})
	}

	workerA := NewLedgerFromTail(tail.Seq, tail.Hash).WithSink(store)
	workerB := NewLedgerFromTail(tail.Seq, tail.Hash).WithSink(store) // same tail — the collision setup

	eA, err := workerA.Append(GovDecision{Decision: "gate:deny", ActionID: "a"})
	if err != nil {
		t.Fatalf("worker A (uncontended) must append: %v", err)
	}
	if eA.Seq != 4 {
		t.Fatalf("worker A should take seq 4, got %d", eA.Seq)
	}

	eB, err := workerB.Append(GovDecision{Decision: "gate:deny", ActionID: "b"})
	if err != nil {
		t.Fatalf("worker B must RECOVER from the seq-4 collision A just caused, not fail closed: %v", err)
	}
	if eB.Seq != 5 {
		t.Fatalf("worker B should re-chain onto the advanced head at seq 5, got %d", eB.Seq)
	}
	if eB.PrevHash != eA.Hash {
		t.Fatalf("worker B must chain onto A's row (prev=%s), got prev=%s", eA.Hash, eB.PrevHash)
	}
	if err := VerifyChain(store.ordered()); err != nil {
		t.Fatalf("the recovered two-writer chain must verify as one unbroken chain: %v", err)
	}
}

// The recovery holds under sustained interleaving: two workers from a common tail append alternately, so
// every append after the first lands on a head the other worker just moved. Each collides once and
// re-chains; the interleaved chain of every write verifies, with nothing lost or overwritten.
func TestAppendRecoversRepeatedlyUnderInterleaving(t *testing.T) {
	store := newSharedLedgerStore()
	seed := NewLedger().WithSink(store)
	var tail LedgerEntry
	for i := 0; i < 2; i++ {
		tail, _ = seed.Append(GovDecision{Decision: "classify:AUTO", ActionID: "seed"})
	}
	a := NewLedgerFromTail(tail.Seq, tail.Hash).WithSink(store)
	b := NewLedgerFromTail(tail.Seq, tail.Hash).WithSink(store)

	const rounds = 12
	for i := 0; i < rounds; i++ {
		if _, err := a.Append(GovDecision{Decision: "gate:deny", ActionID: "a"}); err != nil {
			t.Fatalf("round %d: worker A must append (recovering as needed): %v", i, err)
		}
		if _, err := b.Append(GovDecision{Decision: "gate:deny", ActionID: "b"}); err != nil {
			t.Fatalf("round %d: worker B must recover from A's collision: %v", i, err)
		}
	}
	rows := store.ordered()
	if want := 2 + 2*rounds; len(rows) != want {
		t.Fatalf("every interleaved append must persist exactly once: want %d rows, got %d", want, len(rows))
	}
	if err := VerifyChain(rows); err != nil {
		t.Fatalf("the interleaved two-writer chain must verify: %v", err)
	}
}

// alwaysCollideSink models a head that never yields a free seq (pathological sustained contention). The
// recovery is BOUNDED: it re-reads and retries up to the ceiling, then fails closed with a diagnostic that
// names the contention — never an unbounded spin, and never a silent skip of the audit write.
type alwaysCollideSink struct{ seq int64 }

func (s alwaysCollideSink) PersistContext(_ context.Context, e LedgerEntry) error {
	return fmt.Errorf("%w: seq %d", ErrDuplicateSeq, e.Seq)
}
func (s alwaysCollideSink) Persist(e LedgerEntry) error { return s.PersistContext(context.Background(), e) }
func (s alwaysCollideSink) Tail(context.Context) (int64, string, error) {
	return s.seq, "fixedheadhash", nil // the head never advances past the collision
}

func TestAppendGivesUpBoundedOnSustainedCollision(t *testing.T) {
	l := NewLedgerFromTail(5, "h5").WithSink(alwaysCollideSink{seq: 5})
	_, err := l.Append(GovDecision{Decision: "gate:deny", ActionID: "x"})
	if err == nil {
		t.Fatal("a head that never yields a free seq must fail the Append, not spin forever")
	}
	if !errors.Is(err, ErrDuplicateSeq) {
		t.Fatalf("the give-up error must carry the underlying collision cause, got %v", err)
	}
	if !strings.Contains(err.Error(), "contended") {
		t.Fatalf("the give-up error must name the contention, got %v", err)
	}
}

// A durable sink that reports a collision but CANNOT re-read the head (does not implement LedgerTailReader)
// keeps the old, safe fail-closed behaviour — the recovery is a strict addition, never a regression.
func TestAppendCollisionWithoutTailReaderFailsClosed(t *testing.T) {
	l := NewLedger().WithSink(sinkFunc(func(LedgerEntry) error {
		return fmt.Errorf("%w: seq 1", ErrDuplicateSeq)
	}))
	_, err := l.Append(GovDecision{Decision: "gate:deny", ActionID: "x"})
	if !errors.Is(err, ErrDuplicateSeq) {
		t.Fatalf("a non-recoverable collision must fail closed with the collision error, got %v", err)
	}
	if l.Len() != 0 {
		t.Fatal("the chain must not advance when the durable write failed")
	}
}
