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

// KILLING MUTATION: make Forget a no-op (drop the `delete`). RED — the final read below keeps serving the
// stale cached auto_notice even though the durable store was reset to approve.
func TestForgetEvictsCachedStateSoAReloadSeesAnExternalReset(t *testing.T) {
	ctx := context.Background()
	const op = "tg177-overlay-class"
	// Seeded as if the class graduated in a prior life. Overlay classes cap at auto_notice (ADR-0016), so
	// that is the realistic inherited level for a revoked-then-re-ratified overlay slug.
	store := NewMemGraduationStore().Seed(ClassState{OpClass: op, Level: LevelAutoNotice, NoticeRunCount: 5})
	l := NewLadder(DefaultPromoteThreshold, store, nil)

	if got := l.LevelOf(ctx, op); got != LevelAutoNotice {
		t.Fatalf("seed: LevelOf=%v, want auto_notice (this read also warms the cache)", got)
	}

	// The ratify verb resets the DURABLE row to approve — a write the per-process cache never observes.
	store.Seed(ClassState{OpClass: op, Level: LevelApprove})

	// VACUITY / the bug being closed: with the cache warm and no eviction, the enforcement read still serves
	// the stale auto_notice. If this assertion did not hold, the test could not distinguish Forget working
	// from a class that was never cached.
	if got := l.LevelOf(ctx, op); got != LevelAutoNotice {
		t.Fatalf("pre-Forget: the warm cache must still serve the stale level; LevelOf=%v", got)
	}

	l.Forget(op)

	if got := l.LevelOf(ctx, op); got != LevelApprove {
		t.Fatalf("after Forget the reload must see the durable reset; LevelOf=%v, want approve — a re-ratified "+
			"class would keep enforcing inherited trust it never re-earned", got)
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
