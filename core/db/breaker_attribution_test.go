package db

// TG-665: the trip attribution and the ONE admitted compensation round-trip against a REAL Postgres (TG_TEST_DSN,
// migration 0124 applied), and the compensation CAS admits exactly once for exactly the (open, deviation,
// tripped-by) row. SQL semantics are the whole risk — the NULL mapping and the WHERE predicate.
//
// KILLING MUTATION (executed 2026-11-07): in BreakerStore.AdmitCompensation drop `AND compensation_action_id IS
// NULL` — the second inverse is admitted. RED: TestBreakerStoreAttributionAndCompensationRoundTrip. Restored,
// green.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/breaker"
)

func TestBreakerStoreAttributionAndCompensationRoundTrip(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	s := NewBreakerStore(p)
	name := fmt.Sprintf("tg665-%d", time.Now().UnixNano())
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Save(ctx, breaker.Record{Name: name, State: breaker.StateOpen, FailureCount: 1, OpenedAt: now,
		LastTransitionAt: now, TrippedBy: "act-1", Cause: "deviation"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	rec, ok, err := s.Load(ctx, name)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if rec.TrippedBy != "act-1" || rec.Cause != "deviation" || rec.CompensationActionID != "" || !rec.CompensationAt.IsZero() {
		t.Fatalf("attribution round-trip: %+v", rec)
	}
	// The CAS: wrong action, wrong cause, then the one admission, then refusals.
	if ok, _ := s.AdmitCompensation(ctx, name, "act-OTHER", "deviation", "inv-1", now); ok {
		t.Fatal("wrong tripped-by must not admit")
	}
	if ok, _ := s.AdmitCompensation(ctx, name, "act-1", "kill-switch", "inv-1", now); ok {
		t.Fatal("wrong cause must not admit")
	}
	if ok, err := s.AdmitCompensation(ctx, name, "act-1", "deviation", "inv-1", now); err != nil || !ok {
		t.Fatalf("the one compensation must be admitted, got ok=%v err=%v", ok, err)
	}
	if ok, _ := s.AdmitCompensation(ctx, name, "act-1", "deviation", "inv-2", now); ok {
		t.Fatal("a second compensation must be refused")
	}
	rec, _, _ = s.Load(ctx, name)
	if rec.CompensationActionID != "inv-1" || rec.CompensationAt.IsZero() {
		t.Fatalf("the admitted compensation must round-trip, got %+v", rec)
	}
	// A closed row (Save with cleared attribution) reads NULLs back as empty.
	if err := s.Save(ctx, breaker.Record{Name: name, State: breaker.StateClosed}); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = s.Load(ctx, name)
	if rec.TrippedBy != "" || rec.Cause != "" || rec.CompensationActionID != "" || !rec.CompensationAt.IsZero() {
		t.Fatalf("a closed row must carry no attribution, got %+v", rec)
	}
	if ok, _ := s.AdmitCompensation(ctx, name, "act-1", "deviation", "inv-1", now); ok {
		t.Fatal("a closed row must admit nothing")
	}
	// List carries the columns too.
	rows, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Name == name {
			found = true
		}
	}
	if !found {
		t.Fatal("List must include the row")
	}
}
