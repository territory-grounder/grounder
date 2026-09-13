package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/breaker"
)

// The pgx BreakerStore satisfies the cross-process breaker.Store interface (the compile-time proof also lives
// beside the impl; this keeps the assertion visible in the test surface).
func TestBreakerStoreSatisfiesInterface(t *testing.T) {
	var _ breaker.Store = (*BreakerStore)(nil)
}

// TestBreakerStoreRoundTrip_CrossProcess drives the REAL pgx path and proves the design-wisdom #3 guarantee at
// the durable layer: a breaker OPENED through one store instance is read back as OPEN by a SEPARATE store
// instance over the SAME row — exactly what two sibling worker processes see. It also proves a field the SQL
// forgets to carry (opened_at, failure_count, half_open_successes) fails HERE, not silently in prod. Gated on
// TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestBreakerStoreRoundTrip_CrossProcess(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the breaker store round-trip test")
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
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM mutation_breaker_state WHERE name = 'mutation'") }()

	// WORKER 1: open the shared breaker (a trip), through the real cross-process store.
	w1 := NewBreakerStore(p)
	opened := time.Now().UTC().Truncate(time.Second)
	rec := breaker.Record{
		Name: "mutation", State: breaker.StateOpen, FailureCount: 3,
		OpenedAt: opened, HalfOpenSuccesses: 0, LastTransitionAt: opened,
	}
	if err := w1.Save(ctx, rec); err != nil {
		t.Fatalf("worker-1 save (trip): %v", err)
	}

	// WORKER 2: a SEPARATE store instance reads the SAME row — it must see OPEN (the cross-process kill).
	w2 := NewBreakerStore(p)
	got, ok, err := w2.Load(ctx, "mutation")
	if err != nil || !ok {
		t.Fatalf("worker-2 load = (ok=%v, err=%v), want a stored record", ok, err)
	}
	if got.State != breaker.StateOpen || got.FailureCount != 3 || got.OpenedAt.IsZero() {
		t.Fatalf("cross-process breaker round-trip dropped a field: %+v", got)
	}
	if !got.OpenedAt.Equal(opened) {
		t.Fatalf("opened_at not preserved: got %v want %v", got.OpenedAt, opened)
	}

	// A never-seen breaker reads ok=false (a fresh CLOSED breaker), never a phantom open.
	if _, ok, err := w2.Load(ctx, "never-seen-breaker"); err != nil || ok {
		t.Fatalf("unseen breaker load = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	// List surfaces the row for the metrics exporter.
	rows, err := w2.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Name == "mutation" && r.State == breaker.StateOpen {
			found = true
		}
	}
	if !found {
		t.Fatalf("List did not surface the open mutation breaker: %+v", rows)
	}

	// Re-close (recovery) is a latest-wins upsert clearing the open timestamp — a sibling then reads CLOSED.
	if err := w1.Save(ctx, breaker.Record{Name: "mutation", State: breaker.StateClosed}); err != nil {
		t.Fatalf("worker-1 re-close: %v", err)
	}
	got2, _, err := w2.Load(ctx, "mutation")
	if err != nil || got2.State != breaker.StateClosed || !got2.OpenedAt.IsZero() {
		t.Fatalf("after re-close worker-2 load = (%+v, %v), want closed with no opened_at", got2, err)
	}
}
