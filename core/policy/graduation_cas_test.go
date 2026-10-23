package policy

// TG-146 S3/S4 — the durable graduation ladder's cross-process coherence. Two gaps let a MULTI-worker
// deployment resurrect withdrawn autonomy: the store's Save was a BLIND last-writer-wins upsert, and the
// Ladder cached each class on first touch and never reloaded it. So worker A could durably DEMOTE a class (a
// verified deviation dropped it auto->approve) while worker B, still holding a warm `auto` cache, clobbered
// the demotion back to `auto` on its next write. The fix mirrors the durable breaker: reload-on-Record (the
// store is authoritative) + a compare-and-set Save (a stale write is refused, not applied). These oracles run
// on the in-memory store, which enforces the SAME version guard the pgx store does; the Postgres round-trip
// that proves the real CompareAndOpen SQL lives in core/db/policy_graduation_store_cas_test.go.

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestGraduationStoreCASSemantics pins the store contract the fix relies on: a POSITIVE in-hand version must
// still match the durable row or the write is refused (ErrConcurrentModification); a version-0 write is
// UNCONDITIONAL (a fresh class, or the ratify verb's authoritative reset, which must win over inherited trust).
func TestGraduationStoreCASSemantics(t *testing.T) {
	ctx := context.Background()
	const op = "restart-service"
	store := NewMemGraduationStore().Seed(ClassState{OpClass: op, Level: LevelAuto}) // a persisted row → version 1

	a, err := store.Load(ctx, op)
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	b, err := store.Load(ctx, op)
	if err != nil {
		t.Fatalf("load B: %v", err)
	}
	if a.Version < 1 || b.Version != a.Version {
		t.Fatalf("both readers must see the same persisted version >= 1: a=%d b=%d", a.Version, b.Version)
	}

	// Writer A commits first: the durable row moves to a.Version+1.
	a.Level = LevelApprove
	if err := store.Save(ctx, a); err != nil {
		t.Fatalf("writer A save (fresh version): %v", err)
	}

	// Writer B still holds the pre-A version — its blind clobber MUST be refused.
	b.Level = LevelAuto // B would resurrect auto over A's demotion
	if err := store.Save(ctx, b); !errors.Is(err, ErrConcurrentModification) {
		t.Fatalf("stale writer B save = %v, want ErrConcurrentModification (the clobber the guard exists to stop)", err)
	}
	if fresh, _ := store.Load(ctx, op); fresh.Level != LevelApprove {
		t.Fatalf("durable level = %v after a refused stale write, want approve (A's demotion survived)", fresh.Level)
	}

	// An UNCONDITIONAL (version 0) write — the ratify reset shape — is NEVER refused, whatever the row holds.
	if err := store.Save(ctx, ClassState{OpClass: op, Level: LevelApprove}); err != nil {
		t.Fatalf("unconditional version-0 reset save = %v, want nil (an authoritative reset must always land)", err)
	}
}

// TestReloadOnRecordPreventsPeerDemotionResurrection is the end-to-end TG-146 S3/S4 oracle: two independent
// Ladders over ONE durable store model two workers. Worker A demotes a class on a verified deviation; worker B,
// whose per-process cache still holds the pre-demotion `auto`, then records a clean run. Reload-on-Record makes
// B decide on the durable `approve`, and the CAS refuses any clobber — so the class stays `approve` rather than
// B's stale cache resurrecting `auto` on evidence the op deviated.
//
// KILLING MUTATION: make loadFreshLocked serve the cache without reloading (drop the store read), OR drop the
// version guard from MemGraduationStore.Save — either resurrects `auto` and this test goes RED at the final
// durable assertion (and at rB.From).
func TestReloadOnRecordPreventsPeerDemotionResurrection(t *testing.T) {
	ctx := context.Background()
	const op = "restart-service"
	const n = 5 // high enough that B's single clean run cannot itself promote off approve — isolates the clobber
	store := NewMemGraduationStore().Seed(ClassState{OpClass: op, Level: LevelAuto})
	workerA := NewLadder(n, store, nil)
	workerB := NewLadder(n, store, nil)

	// Both workers warm their per-process cache at the graduated level.
	if got := workerA.LevelOf(ctx, op); got != LevelAuto {
		t.Fatalf("workerA seed read = %v, want auto", got)
	}
	if got := workerB.LevelOf(ctx, op); got != LevelAuto {
		t.Fatalf("workerB seed read = %v, want auto (its cache is now warm at auto)", got)
	}

	// Worker A observes a verified deviation → durable demotion to approve.
	rA, err := workerA.Record(ctx, op, OutcomeDeviated)
	if err != nil {
		t.Fatalf("workerA demote Record: %v", err)
	}
	if !rA.Demoted || rA.To != LevelApprove {
		t.Fatalf("workerA deviation should demote to approve: %+v", rA)
	}

	// Worker B — cache still `auto` — records a later clean run. It must reload the peer demotion and NOT clobber.
	rB, err := workerB.Record(ctx, op, OutcomeVerifiedClean)
	if err != nil {
		t.Fatalf("workerB clean Record after peer demotion: %v", err)
	}
	if rB.From != LevelApprove {
		t.Fatalf("workerB recorded FROM %v — it did not reload the peer's demotion (used the stale auto cache)", rB.From)
	}
	if rB.To != LevelApprove {
		t.Fatalf("workerB moved the class to %v on a single clean run at N=%d — a stale-auto clobber, not a fresh decide", rB.To, n)
	}
	if fresh, _ := store.Load(ctx, op); fresh.Level != LevelApprove {
		t.Fatalf("durable level = %v after a peer demotion + stale-cache clean run, want approve "+
			"(a resurrected auto is the exact S3 clobber this closes)", fresh.Level)
	}
}

// peerWriteOnceStore injects a single mid-Record conflict: on the FIRST Save it lands a peer DEMOTION into the
// backing store and then refuses this (now stale) write, so Record must reload and re-decide on the peer's
// fresh state. Every later Save delegates normally.
type peerWriteOnceStore struct {
	inner *MemGraduationStore
	fired bool
}

func (p *peerWriteOnceStore) Load(ctx context.Context, op string) (ClassState, error) {
	return p.inner.Load(ctx, op)
}

func (p *peerWriteOnceStore) Save(ctx context.Context, st ClassState) error {
	if !p.fired {
		p.fired = true
		// A peer worker's demotion lands right now (unconditional, as an authoritative write would), then this
		// caller's optimistic write is refused because the row moved underneath it.
		_ = p.inner.Save(ctx, ClassState{OpClass: st.OpClass, Level: LevelApprove})
		return fmt.Errorf("injected peer write: %w", ErrConcurrentModification)
	}
	return p.inner.Save(ctx, st)
}

// TestRecordRetriesAndReDecidesOnAConcurrentModification proves Record does not surface a transient CAS miss as
// an error: it reloads the peer's fresh state, re-runs the pure state machine on it, and converges. A clean run
// that would have stayed `auto` on the stale cache instead lands on the freshly-demoted `approve`.
func TestRecordRetriesAndReDecidesOnAConcurrentModification(t *testing.T) {
	ctx := context.Background()
	const op = "restart-service"
	inner := NewMemGraduationStore().Seed(ClassState{OpClass: op, Level: LevelAuto})
	l := NewLadder(5, &peerWriteOnceStore{inner: inner}, nil)
	if got := l.LevelOf(ctx, op); got != LevelAuto {
		t.Fatalf("warm cache read = %v, want auto", got)
	}

	r, err := l.Record(ctx, op, OutcomeVerifiedClean)
	if err != nil {
		t.Fatalf("Record must converge after one injected CAS miss, got error: %v", err)
	}
	if r.From != LevelApprove || r.To != LevelApprove {
		t.Fatalf("Record re-decided from %v to %v; want it to act on the peer's fresh approve, not the stale auto", r.From, r.To)
	}
	if fresh, _ := inner.Load(ctx, op); fresh.Level != LevelApprove {
		t.Fatalf("durable level = %v, want approve (the retry landed on fresh state, no clobber)", fresh.Level)
	}
}
