package policy

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// newLadder builds a ladder with an in-memory store for the oracles.
func newTestLadder(t *testing.T, n int) *Ladder {
	t.Helper()
	return NewLadder(n, NewMemGraduationStore(), nil)
}

// recordClean records k verified-clean runs and returns the last result.
func recordN(t *testing.T, l *Ladder, opClass string, o RunOutcome, k int) RecordResult {
	t.Helper()
	var last RecordResult
	for i := 0; i < k; i++ {
		r, err := l.Record(context.Background(), opClass, o)
		if err != nil {
			t.Fatalf("Record(%s) #%d: unexpected error: %v", o, i+1, err)
		}
		last = r
	}
	return last
}

// TestStartsAtApprove proves every class starts fail-closed at LevelApprove with a zero count (REQ-1514).
func TestStartsAtApprove(t *testing.T) {
	l := newTestLadder(t, 3)
	st := l.State(context.Background(), "service.restart")
	if st.Level != LevelApprove {
		t.Fatalf("fresh class level = %v, want approve (fail closed)", st.Level)
	}
	if st.CleanRunCount != 0 {
		t.Fatalf("fresh class count = %d, want 0", st.CleanRunCount)
	}
}

// TestPromoteAfterNCleanVerified proves a class promotes to auto ONLY after exactly N consecutive
// verified-clean runs — not before (REQ-1514). Bound acceptance: "An op-class promotes to auto after N clean
// verified runs".
func TestPromoteAfterNCleanVerified(t *testing.T) {
	const n = 4
	l := newTestLadder(t, n)
	op := "service.restart"

	// The first N-1 clean runs must NOT promote (negative control: no promotion without N genuine cleans).
	for i := 1; i < n; i++ {
		r := recordN(t, l, op, OutcomeVerifiedClean, 1)
		if r.Promoted {
			t.Fatalf("promoted after %d clean runs, want promotion only at N=%d", i, n)
		}
		if r.To != LevelApprove {
			t.Fatalf("level after %d cleans = %v, want approve", i, r.To)
		}
		if r.CleanRunCount != i {
			t.Fatalf("count after %d cleans = %d, want %d", i, r.CleanRunCount, i)
		}
	}
	// The Nth clean run promotes.
	r := recordN(t, l, op, OutcomeVerifiedClean, 1)
	if !r.Promoted {
		t.Fatalf("did not promote on the Nth (%d) clean verified run", n)
	}
	if r.To != LevelAuto {
		t.Fatalf("level after N cleans = %v, want auto", r.To)
	}
	if l.LevelOf(context.Background(), op) != LevelAuto {
		t.Fatalf("class not durably at auto after promotion")
	}
}

// TestDemoteOnDeviationAtAuto proves a deviation at auto demotes to approve and resets the count (REQ-1514).
// Bound acceptance: "An op-class demotes to approve on the first deviation".
func TestDemoteOnDeviationAtAuto(t *testing.T) {
	const n = 2
	l := newTestLadder(t, n)
	op := "service.restart"
	recordN(t, l, op, OutcomeVerifiedClean, n) // promote to auto
	if l.LevelOf(context.Background(), op) != LevelAuto {
		t.Fatalf("precondition: class not at auto")
	}
	r, err := l.Record(context.Background(), op, OutcomeDeviated)
	if err != nil {
		t.Fatalf("Record(deviated): %v", err)
	}
	if !r.Demoted {
		t.Fatalf("deviation at auto did not record a demotion")
	}
	if r.To != LevelApprove {
		t.Fatalf("level after deviation = %v, want approve", r.To)
	}
	if r.CleanRunCount != 0 {
		t.Fatalf("count after deviation = %d, want 0 (reset)", r.CleanRunCount)
	}
}

// TestUnverifiedAutoDoesNotPromote proves verify-on-auto (REQ-1515): an unverified run does NOT count as
// clean — it never promotes and it resets the consecutive-clean streak.
func TestUnverifiedAutoDoesNotPromote(t *testing.T) {
	const n = 3
	l := newTestLadder(t, n)
	op := "service.restart"

	recordN(t, l, op, OutcomeVerifiedClean, n-1) // one short of promotion
	// An unverified run must NOT promote and MUST reset the streak.
	r, err := l.Record(context.Background(), op, OutcomeUnverified)
	if err != nil {
		t.Fatalf("Record(unverified): %v", err)
	}
	if r.Promoted {
		t.Fatalf("unverified run promoted — verify-on-auto (REQ-1515) violated")
	}
	if r.To != LevelApprove {
		t.Fatalf("unverified run changed level to %v, want approve", r.To)
	}
	if r.CleanRunCount != 0 {
		t.Fatalf("unverified run left count = %d, want 0 (streak reset)", r.CleanRunCount)
	}
	// It must now take N further clean runs to promote — the streak genuinely reset.
	last := recordN(t, l, op, OutcomeVerifiedClean, n)
	if !last.Promoted {
		t.Fatalf("did not promote after a full fresh N cleans post-unverified")
	}
}

// TestDeviationMidClimbResets proves a deviation mid-climb (at approve) resets the count to 0 (REQ-1514).
func TestDeviationMidClimbResets(t *testing.T) {
	const n = 5
	l := newTestLadder(t, n)
	op := "service.restart"
	recordN(t, l, op, OutcomeVerifiedClean, 3) // count = 3, still approve
	r, err := l.Record(context.Background(), op, OutcomeDeviated)
	if err != nil {
		t.Fatalf("Record(deviated): %v", err)
	}
	if r.Demoted {
		t.Fatalf("deviation at approve reported a demotion; there was no autonomy to drop")
	}
	if r.To != LevelApprove || r.CleanRunCount != 0 {
		t.Fatalf("deviation mid-climb: level=%v count=%d, want approve/0", r.To, r.CleanRunCount)
	}
}

// TestNegativeControlNoPromoteWithoutNClean proves a class NEVER promotes without N genuinely-clean-verified
// runs: partials/unverified interleaved with cleans never accumulate to a promotion.
func TestNegativeControlNoPromoteWithoutNClean(t *testing.T) {
	const n = 3
	l := newTestLadder(t, n)
	op := "service.restart"
	// Alternate clean / unverified forever — the streak never reaches N.
	for i := 0; i < 20; i++ {
		if r, _ := l.Record(context.Background(), op, OutcomeVerifiedClean); r.Promoted {
			t.Fatalf("promoted on an interrupted streak at step %d", i)
		}
		if r, _ := l.Record(context.Background(), op, OutcomeUnverified); r.Promoted {
			t.Fatalf("unverified run promoted at step %d", i)
		}
	}
	if l.LevelOf(context.Background(), op) != LevelApprove {
		t.Fatalf("class promoted without N consecutive genuine cleans")
	}
}

// TestPartialMapsToUnverifiedNonPromoting proves the boundary mapping: a verified `partial` is neither
// promoting nor demoting (REQ-1514 — not a `match`, not a `deviation`).
func TestPartialMapsToUnverifiedNonPromoting(t *testing.T) {
	if got := OutcomeFromVerdict(safety.VerdictPartial, true); got != OutcomeUnverified {
		t.Fatalf("partial mapped to %v, want unverified (non-promoting, non-demoting)", got)
	}
	if got := OutcomeFromVerdict(safety.VerdictMatch, true); got != OutcomeVerifiedClean {
		t.Fatalf("verified match mapped to %v, want verified_clean", got)
	}
	if got := OutcomeFromVerdict(safety.VerdictDeviation, true); got != OutcomeDeviated {
		t.Fatalf("deviation mapped to %v, want deviated", got)
	}
	// An unverified post-state (verified=false) is unverified regardless of any verdict value (verify-on-auto).
	if got := OutcomeFromVerdict(safety.VerdictMatch, false); got != OutcomeUnverified {
		t.Fatalf("unverified match mapped to %v, want unverified (REQ-1515)", got)
	}
}

// TestGraduatedVerdictHook proves the graduation→verdict hook semantics (REQ-1514): a non-graduated auto is
// downgraded to approve, a graduated auto stays auto, and deny is never affected.
func TestGraduatedVerdictHook(t *testing.T) {
	const n = 1
	l := newTestLadder(t, n)
	ctx := context.Background()
	op := "service.restart"

	// Non-graduated: an auto rule verdict downgrades to approve.
	if v := l.GraduatedVerdict(ctx, op, VerdictAuto); v != VerdictApprove {
		t.Fatalf("ungraduated auto → %v, want approve", v)
	}
	// deny and approve are untouched even while ungraduated.
	if v := l.GraduatedVerdict(ctx, op, VerdictDeny); v != VerdictDeny {
		t.Fatalf("deny was affected by graduation: got %v", v)
	}
	if v := l.GraduatedVerdict(ctx, op, VerdictApprove); v != VerdictApprove {
		t.Fatalf("approve changed: got %v", v)
	}
	// Promote (N=1) then a graduated auto stays auto; deny STILL never affected.
	recordN(t, l, op, OutcomeVerifiedClean, n)
	if v := l.GraduatedVerdict(ctx, op, VerdictAuto); v != VerdictAuto {
		t.Fatalf("graduated auto → %v, want auto", v)
	}
	if v := l.GraduatedVerdict(ctx, op, VerdictDeny); v != VerdictDeny {
		t.Fatalf("deny affected by graduation after promotion: got %v", v)
	}
}

// TestLoadAbsentFailsClosedToApprove proves an absent persisted class resolves fail-closed to approve.
func TestLoadAbsentFailsClosedToApprove(t *testing.T) {
	l := NewLadder(3, NewMemGraduationStore(), nil) // empty store → every class absent
	if got := l.LevelOf(context.Background(), "never.seen"); got != LevelApprove {
		t.Fatalf("absent class level = %v, want approve (fail closed)", got)
	}
}

// TestLoadErrorFailsClosedToApprove proves an unreadable store resolves fail-closed to approve — a class is
// never loaded into auto from an errored store.
func TestLoadErrorFailsClosedToApprove(t *testing.T) {
	store := NewMemGraduationStore().
		Seed(ClassState{OpClass: "op", Level: LevelAuto}). // even a stored auto ...
		WithLoadError(errors.New("db down"))               // ... is masked by a load error → approve.
	l := NewLadder(3, store, nil)
	if got := l.LevelOf(context.Background(), "op"); got != LevelApprove {
		t.Fatalf("errored-load class level = %v, want approve (never auto from a bad store)", got)
	}
}

// TestLoadCorruptFailsClosedToApprove proves a corrupt persisted level (out of the closed enum) resolves
// fail-closed to approve — never straight into auto.
func TestLoadCorruptFailsClosedToApprove(t *testing.T) {
	store := NewMemGraduationStore().Seed(ClassState{OpClass: "op", Level: Level(99)}) // corrupt
	l := NewLadder(3, store, nil)
	if got := l.LevelOf(context.Background(), "op"); got != LevelApprove {
		t.Fatalf("corrupt-level class = %v, want approve (fail closed, never auto)", got)
	}
}

// TestDurableAutoReload proves a LEGITIMATELY persisted auto state is honored on reload (the store is durable
// state, not a threat) — distinguishing it from the corrupt/errored fail-closed paths above.
func TestDurableAutoReload(t *testing.T) {
	store := NewMemGraduationStore().Seed(ClassState{OpClass: "op", Level: LevelAuto})
	l := NewLadder(3, store, nil)
	if got := l.LevelOf(context.Background(), "op"); got != LevelAuto {
		t.Fatalf("durable auto not honored on reload: got %v", got)
	}
}

// failingStore Saves always fail — to exercise the persistence fail-closed policy.
type failingStore struct{ inner *MemGraduationStore }

func (f failingStore) Load(ctx context.Context, op string) (ClassState, error) {
	return f.inner.Load(ctx, op)
}
func (f failingStore) Save(context.Context, ClassState) error { return errors.New("save failed") }

// TestPromotionNotPersistedWithholdsAutonomy proves a promotion that cannot be persisted is REFUSED — the
// class stays approve (autonomy is never granted on state that would vanish on restart).
func TestPromotionNotPersistedWithholdsAutonomy(t *testing.T) {
	l := NewLadder(1, failingStore{inner: NewMemGraduationStore()}, nil)
	op := "op"
	r, err := l.Record(context.Background(), op, OutcomeVerifiedClean) // would promote (N=1)
	if !errors.Is(err, ErrPromotionNotPersisted) {
		t.Fatalf("promotion-persist-failure error = %v, want ErrPromotionNotPersisted", err)
	}
	if r.To != LevelApprove {
		t.Fatalf("withheld promotion left level %v, want approve", r.To)
	}
	if l.LevelOf(context.Background(), op) != LevelApprove {
		t.Fatalf("class advanced to auto despite persist failure — autonomy not withheld")
	}
}

// TestConcurrentRecordSerializes proves concurrent Record calls serialize with no torn state under -race.
func TestConcurrentRecordSerializes(t *testing.T) {
	l := newTestLadder(t, 1000000) // high N so nothing promotes; we only test the counter integrity
	op := "op"
	const goroutines, per = 16, 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if _, err := l.Record(context.Background(), op, OutcomeVerifiedClean); err != nil {
					t.Errorf("concurrent Record: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if got := l.State(context.Background(), op).CleanRunCount; got != goroutines*per {
		t.Fatalf("clean count = %d, want %d (torn state under concurrency)", got, goroutines*per)
	}
}

// TestApplyOutcomePureDeterministic proves the state machine is a pure, deterministic function: identical
// inputs yield identical outputs, and it mutates no shared state.
func TestApplyOutcomePureDeterministic(t *testing.T) {
	in := ClassState{OpClass: "op", Level: LevelApprove, CleanRunCount: 2}
	a, ra := applyOutcome(in, OutcomeVerifiedClean, 3)
	b, rb := applyOutcome(in, OutcomeVerifiedClean, 3)
	if a != b || ra != rb {
		t.Fatalf("applyOutcome not deterministic: %+v/%+v vs %+v/%+v", a, ra, b, rb)
	}
	if in.CleanRunCount != 2 {
		t.Fatalf("applyOutcome mutated its input: %+v", in)
	}
	if !ra.Promoted || a.Level != LevelAuto {
		t.Fatalf("3rd clean at count 2 should promote: %+v", ra)
	}
}
