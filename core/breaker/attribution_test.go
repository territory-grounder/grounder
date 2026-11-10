package breaker

// TG-665: trip ATTRIBUTION on the breaker record and the ONE admitted compensation. The engine stamps who/why
// when it opens, keeps the first attribution, escalates a deviation cause under a stronger one, clears on
// close, and admits exactly one compensation for exactly the (open, deviation, tripped-by) record.
//
// KILLING MUTATION (executed 2026-11-07): in MemStore.AdmitCompensation drop `rec.CompensationActionID != ""`
// from the refusal predicate — a second inverse is admitted. RED: TestAdmitCompensationIsExactlyOnce. Restored
// from a copy, green.

import (
	"context"
	"testing"
	"time"
)

func TestRecordFailureForStampsTheOpeningTrip(t *testing.T) {
	ctx := context.Background()
	st := NewMemStore()
	b, err := New("mutation", st, WithThreshold(2))
	if err != nil {
		t.Fatal(err)
	}
	// Below the threshold: no attribution yet (the breaker is still closed).
	if err := b.RecordFailureFor(ctx, "act-1", "deviation"); err != nil {
		t.Fatal(err)
	}
	rec, _ := b.Snapshot(ctx)
	if rec.State != StateClosed || rec.TrippedBy != "" || rec.Cause != "" {
		t.Fatalf("a failure below the threshold must not attribute, got %+v", rec)
	}
	// The OPENING failure stamps who and why.
	if err := b.RecordFailureFor(ctx, "act-2", "deviation"); err != nil {
		t.Fatal(err)
	}
	rec, _ = b.Snapshot(ctx)
	if rec.State != StateOpen || rec.TrippedBy != "act-2" || rec.Cause != "deviation" {
		t.Fatalf("the opening trip must be attributed, got %+v", rec)
	}
	// A later deviation changes nothing (first trip wins).
	if err := b.RecordFailureFor(ctx, "act-3", "deviation"); err != nil {
		t.Fatal(err)
	}
	rec, _ = b.Snapshot(ctx)
	if rec.TrippedBy != "act-2" || rec.Cause != "deviation" {
		t.Fatalf("first trip must win, got %+v", rec)
	}
	// A stronger cause ESCALATES the record (the compensation CAS keys on `deviation`).
	if err := b.RecordFailureFor(ctx, "", "kill-switch"); err != nil {
		t.Fatal(err)
	}
	rec, _ = b.Snapshot(ctx)
	if rec.TrippedBy != "act-2" || rec.Cause != "kill-switch" {
		t.Fatalf("a kill-switch over a deviation must escalate the cause and keep the tripping action, got %+v", rec)
	}
	// A close clears everything.
	if err := b.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	rec, _ = b.Snapshot(ctx)
	if rec.State != StateClosed || rec.TrippedBy != "" || rec.Cause != "" || rec.CompensationActionID != "" {
		t.Fatalf("a close must clear the attribution, got %+v", rec)
	}
}

func TestAdmitCompensationIsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, st Store) {
		b, err := New("mutation", st, WithThreshold(1))
		if err != nil {
			t.Fatal(err)
		}
		// Closed: nothing to compensate.
		if ok, _ := b.AdmitCompensation(ctx, "act-1", "deviation", "inv-1"); ok {
			t.Fatal("a closed breaker admits no compensation")
		}
		if err := b.RecordFailureFor(ctx, "act-1", "deviation"); err != nil {
			t.Fatal(err)
		}
		if ok, _ := b.AdmitCompensation(ctx, "act-OTHER", "deviation", "inv-1"); ok {
			t.Fatal("an inverse of a DIFFERENT action must not be admitted")
		}
		if ok, _ := b.AdmitCompensation(ctx, "act-1", "kill-switch", "inv-1"); ok {
			t.Fatal("a cause that is not the record's must not admit")
		}
		if ok, err := b.AdmitCompensation(ctx, "act-1", "deviation", "inv-1"); err != nil || !ok {
			t.Fatalf("the one matching compensation must be admitted, got ok=%v err=%v", ok, err)
		}
		if ok, _ := b.AdmitCompensation(ctx, "act-1", "deviation", "inv-2"); ok {
			t.Fatal("a SECOND compensation must be refused")
		}
		if ok, _ := b.AdmitCompensation(ctx, "act-1", "deviation", "inv-1"); ok {
			t.Fatal("the same compensation twice must be refused")
		}
		rec, _ := b.Snapshot(ctx)
		if rec.CompensationActionID != "inv-1" || rec.CompensationAt.IsZero() {
			t.Fatalf("the admitted compensation must be recorded, got %+v", rec)
		}
		// Blank keys never admit.
		if ok, _ := b.AdmitCompensation(ctx, "", "deviation", "inv-9"); ok {
			t.Fatal("a blank tripped-by must not admit")
		}
	}
	t.Run("store CAS (MemStore)", func(t *testing.T) { run(t, NewMemStore()) })
	t.Run("fallback (a Store without the capability)", func(t *testing.T) { run(t, &plainStore{rows: map[string]Record{}}) })
}

// plainStore is a Store WITHOUT the atomic capabilities, to drive the engine's fallback path.
type plainStore struct{ rows map[string]Record }

func (p *plainStore) Load(_ context.Context, name string) (Record, bool, error) {
	r, ok := p.rows[name]
	return r, ok, nil
}
func (p *plainStore) Save(_ context.Context, rec Record) error { p.rows[rec.Name] = rec; return nil }
func (p *plainStore) List(_ context.Context) ([]Record, error) {
	out := make([]Record, 0, len(p.rows))
	for _, r := range p.rows {
		out = append(out, r)
	}
	return out, nil
}

// The escalation path on an OPEN record only changes the cause; an unrelated cause over a kill-switch record
// never downgrades it, and a deviation over a kill-switch never re-opens the compensation door.
func TestEscalationNeverDowngrades(t *testing.T) {
	ctx := context.Background()
	b, _ := New("mutation", NewMemStore(), WithThreshold(1), WithClock(func() time.Time { return time.Unix(1700000000, 0) }))
	if err := b.RecordFailureFor(ctx, "act-1", "kill-switch"); err != nil {
		t.Fatal(err)
	}
	if err := b.RecordFailureFor(ctx, "act-2", "deviation"); err != nil {
		t.Fatal(err)
	}
	rec, _ := b.Snapshot(ctx)
	if rec.Cause != "kill-switch" || rec.TrippedBy != "act-1" {
		t.Fatalf("a deviation after a kill-switch must not downgrade the record, got %+v", rec)
	}
	if ok, _ := b.AdmitCompensation(ctx, "act-2", "deviation", "inv"); ok {
		t.Fatal("no compensation is admissible on a kill-switch record")
	}
}
