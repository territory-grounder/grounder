package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/breaker"
)

// TestBreakerStoreCompareAndOpen_AtomicCrossProcess drives the REAL pgx conditional upsert (TG-432) and proves
// the cross-process compare-and-open contract at the durable layer: the FIRST open of an absent/closed breaker
// reports openedNow=true, and a SEPARATE store instance (a sibling worker process) racing the SAME death finds
// it already open — openedNow=false, so exactly one of the two pages. It also proves the already-open path
// preserves the first trip's opened_at (the ongoing-death timestamp does not move) and that recovery re-arms
// the transition. Gated on TG_TEST_POSTGRES_DSN (only the CI harness job has Postgres).
func TestBreakerStoreCompareAndOpen_AtomicCrossProcess(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the compare-and-open test")
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
	const name = "judge-death-catest"
	clean := func() { _, _ = p.Exec(ctx, "DELETE FROM mutation_breaker_state WHERE name = $1", name) }
	clean()
	defer clean()

	s1 := NewBreakerStore(p)
	s2 := NewBreakerStore(p) // a SEPARATE instance = a sibling worker process over the same row
	t0 := time.Now().UTC().Truncate(time.Second)

	// 1. Fresh (absent) breaker: the first compare-and-open flips absent→open and reports openedNow=true.
	opened, err := s1.CompareAndOpen(ctx, name, t0)
	if err != nil {
		t.Fatalf("s1 first open: %v", err)
	}
	if !opened {
		t.Fatal("the first CompareAndOpen on an absent breaker must report openedNow=true (it opened it)")
	}

	// 2. A sibling worker racing the SAME death an hour later must find it ALREADY open: openedNow=false, so it
	// does not re-page — and the FIRST trip's opened_at must be preserved (an ongoing death does not re-stamp).
	opened, err = s2.CompareAndOpen(ctx, name, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("s2 racing open: %v", err)
	}
	if opened {
		t.Fatal("a second CompareAndOpen on an already-open breaker must report openedNow=false (the cross-process dedup)")
	}
	got, ok, err := s2.Load(ctx, name)
	if err != nil || !ok {
		t.Fatalf("load after open: ok=%v err=%v", ok, err)
	}
	if got.State != breaker.StateOpen {
		t.Fatalf("breaker not open after CompareAndOpen: %+v", got)
	}
	if !got.OpenedAt.Equal(t0) {
		t.Fatalf("the idempotent second open overwrote opened_at: got %v want the first trip %v", got.OpenedAt, t0)
	}

	// 3. A half-open breaker (mid-probe) is NOT open, so CompareAndOpen re-opens it and reports true — the
	// WHERE state <> 'open' guard admits closed AND half_open, only skipping an already-open row.
	if err := s1.Save(ctx, breaker.Record{Name: name, State: breaker.StateHalfOpen}); err != nil {
		t.Fatalf("set half-open: %v", err)
	}
	opened, err = s2.CompareAndOpen(ctx, name, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("open from half-open: %v", err)
	}
	if !opened {
		t.Fatal("CompareAndOpen on a half-open breaker must report openedNow=true (half_open is not open)")
	}

	// 4. After a recovery to CLOSED, a fresh death re-opens and reports true again — the dedup is
	// transition-based, not a permanent gag.
	if err := s1.Save(ctx, breaker.Record{Name: name, State: breaker.StateClosed}); err != nil {
		t.Fatalf("recovery reset: %v", err)
	}
	opened, err = s2.CompareAndOpen(ctx, name, t0.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("re-open after recovery: %v", err)
	}
	if !opened {
		t.Fatal("a fresh death after recovery (closed) must report openedNow=true again")
	}
}
