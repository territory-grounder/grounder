package db

import (
	"context"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/cost"
)

// The pgx CostStore satisfies the cross-process cost.Store interface (the compile-time proof also lives
// beside the impl; this keeps the assertion visible in the test surface).
func TestCostStoreSatisfiesInterface(t *testing.T) {
	var _ cost.Store = (*CostStore)(nil)
}

// TestCostStoreRoundTrip_CrossProcess drives the REAL pgx path and proves the spend-guard's cross-process
// guarantee at the durable layer: spend ADDED through one store instance is read back by a SEPARATE
// instance (additive across workers), and a breaker OPENED through one instance is read OPEN by another
// over the SAME row — exactly what two sibling worker processes see. Gated on TG_TEST_POSTGRES_DSN (CI has
// no Postgres).
func TestCostStoreRoundTrip_CrossProcess(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the cost store round-trip test")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM cost_accrual WHERE bucket_key IN ('2026-07-20', 'sess-x')")
		_, _ = p.Exec(ctx, "DELETE FROM cost_breaker_state WHERE name = 'cost'")
	}()

	// WORKER 1: accrue spend into the shared day + session buckets.
	w1 := NewCostStore(p)
	total, err := w1.Accrue(ctx, cost.BucketDay, "2026-07-20", 1.25)
	if err != nil || total != 1.25 {
		t.Fatalf("worker-1 first accrual = (%v, %v), want (1.25, nil)", total, err)
	}
	if _, err := w1.Accrue(ctx, cost.BucketSession, "sess-x", 0.50); err != nil {
		t.Fatalf("worker-1 session accrual: %v", err)
	}

	// WORKER 2: a SEPARATE instance ADDS to the SAME day bucket — the additive upsert coordinates the total.
	w2 := NewCostStore(p)
	total2, err := w2.Accrue(ctx, cost.BucketDay, "2026-07-20", 0.75)
	if err != nil || total2 != 2.0 {
		t.Fatalf("worker-2 additive accrual = (%v, %v), want (2.0, nil) — additive upsert dropped a delta", total2, err)
	}
	if got, err := w2.Total(ctx, cost.BucketDay, "2026-07-20"); err != nil || got != 2.0 {
		t.Fatalf("worker-2 day total = (%v, %v), want 2.0", got, err)
	}

	// An unseen bucket reads $0 (never a phantom).
	if got, err := w2.Total(ctx, cost.BucketSession, "never-seen"); err != nil || got != 0 {
		t.Fatalf("unseen bucket total = (%v, %v), want (0, nil)", got, err)
	}

	// Breaker: closed before any trip; OPEN after worker-1 trips; a sibling reads OPEN (the cross-process kill).
	if open, _, err := w2.BreakerOpen(ctx); err != nil || open {
		t.Fatalf("breaker before trip = (open=%v, err=%v), want closed", open, err)
	}
	if err := w1.TripBreaker(ctx, "daily budget exceeded", 2.0); err != nil {
		t.Fatalf("worker-1 trip: %v", err)
	}
	open, reason, err := w2.BreakerOpen(ctx)
	if err != nil || !open || reason != "daily budget exceeded" {
		t.Fatalf("worker-2 breaker after trip = (open=%v, reason=%q, err=%v), want OPEN with the reason", open, reason, err)
	}
}
