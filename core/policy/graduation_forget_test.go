package policy

// TG-177 — the ladder-cache eviction that carries the ratify verb's fail-closed graduation reset into the
// live enforcement path. GraduatedVerdict reads `stateLocked`, which caches a class's ClassState per process
// forever once touched. The ratify reset writes the durable store DIRECTLY (bypassing this cache), so without
// Forget a warm cache would keep serving the pre-reset level until restart — the exact stale-trust hole the
// composed-registry refresher closes by calling Forget when it (re)admits an overlay class.

import (
	"context"
	"testing"
)

// TG-666 changed this test's premise. With a durable store every ladder read now reloads the row (stateLocked),
// so the ratify verb's durable reset reaches the enforcement read AT ONCE — there is no warm-cache window left for
// Forget to close. Forget stays harmless there, and it is still the eviction for the store-less (in-memory) mode,
// pinned by the next test.
func TestAnExternalDurableResetReachesTheEnforcementReadAtOnce(t *testing.T) {
	ctx := context.Background()
	const op = "tg177-overlay-class"
	// Seeded as if the class graduated in a prior life. Overlay classes cap at auto_notice (ADR-0016), so
	// that is the realistic inherited level for a revoked-then-re-ratified overlay slug.
	store := NewMemGraduationStore().Seed(ClassState{OpClass: op, Level: LevelAutoNotice, NoticeRunCount: 5})
	l := NewLadder(DefaultPromoteThreshold, store, nil)

	if got := l.LevelOf(ctx, op); got != LevelAutoNotice {
		t.Fatalf("seed: LevelOf=%v, want auto_notice", got)
	}

	// The ratify verb resets the DURABLE row to approve — a write this process did not make.
	store.Seed(ClassState{OpClass: op, Level: LevelApprove})

	if got := l.LevelOf(ctx, op); got != LevelApprove {
		t.Fatalf("the enforcement read must see the durable reset immediately; LevelOf=%v, want approve — a "+
			"re-ratified class would keep enforcing inherited trust it never re-earned", got)
	}
	l.Forget(op) // harmless with a store
	if got := l.LevelOf(ctx, op); got != LevelApprove {
		t.Fatalf("after Forget the read still answers the durable row; LevelOf=%v", got)
	}
}

// KILLING MUTATION: make Forget a no-op (drop the `delete`). RED — without a store the cache IS the state, so the
// evicted class must come back fail-closed to approve.
func TestForgetEvictsTheInMemoryState(t *testing.T) {
	ctx := context.Background()
	const op = "tg177-memory-class"
	l := NewLadder(1, nil, nil)
	if _, err := l.Record(ctx, op, OutcomeVerifiedClean); err != nil {
		t.Fatal(err)
	}
	if got := l.LevelOf(ctx, op); got != LevelAutoNotice {
		t.Fatalf("control: threshold 1 promotes to auto_notice in memory, got %v", got)
	}
	l.Forget(op)
	if got := l.LevelOf(ctx, op); got != LevelApprove {
		t.Fatalf("after Forget an in-memory class must reload fail-closed to approve; LevelOf=%v", got)
	}
}

// Forget on a class that was never cached is a harmless no-op — the next read still loads fail-closed from
// the store (absent → approve). Guards against Forget accidentally seeding or corrupting a cache entry.
func TestForgetOfAnUncachedClassIsANoop(t *testing.T) {
	ctx := context.Background()
	l := NewLadder(DefaultPromoteThreshold, NewMemGraduationStore(), nil)
	l.Forget("never-seen")
	if got := l.LevelOf(ctx, "never-seen"); got != LevelApprove {
		t.Fatalf("an absent class must resolve fail-closed to approve after a spurious Forget; got %v", got)
	}
}
