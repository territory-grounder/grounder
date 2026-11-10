package db

// TG-663: the INDETERMINATE per-run row round-trips against a REAL Postgres (TG_TEST_DSN, migration 0123 applied):
// the enum value exists, the 0043 pairing CHECK admits (verdict present, unverifiable=false), and ExecutionFor
// reads it back as the distinct disposition — never as a verified outcome, never as unverifiable, never absent.
//
// KILLING MUTATION (executed 2026-11-07): in RecordIndeterminate, write `unverifiable=true` with the verdict —
// the 0043 pairing CHECK refuses the row and the round-trip below goes red at the insert. Restored, green.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestActionExecutionIndeterminateRoundTrip(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	s := NewActionExecutionStore(p)
	id := time.Now().UnixNano()
	action := fmt.Sprintf("%s-tg663-act-%d", fixtureNS, id)
	ref := fmt.Sprintf("%s-tg663-ref-%d", fixtureNS, id)
	if err := s.RecordIndeterminate(ctx, action, ref, "web01", "dc1", "", "ack lost after launch"); err != nil {
		t.Fatalf("record indeterminate: %v", err)
	}
	e, found, err := s.ExecutionFor(ctx, action, ref)
	if err != nil || !found {
		t.Fatalf("execution for: found=%v err=%v", found, err)
	}
	if e.Verdict != VerdictIndeterminate || e.Unverifiable {
		t.Fatalf("the row must read back as the indeterminate disposition with unverifiable=false, got %+v", e)
	}
	if e.TargetHost != "web01" || e.Site != "dc1" || e.InvertsActionID != "" {
		t.Fatalf("keys/target/site must round-trip, got %+v", e)
	}
	// An inverse's indeterminate row keeps its inverts_action_id, so InversesOf still finds it.
	if err := s.RecordIndeterminate(ctx, action+"-inv", ref, "web01", "dc1", action, "inverse ack lost"); err != nil {
		t.Fatalf("record indeterminate inverse: %v", err)
	}
	inv, err := s.InversesOf(ctx, action, ref)
	if err != nil || len(inv) != 1 || inv[0].Verdict != VerdictIndeterminate {
		t.Fatalf("an indeterminate inverse must be queryable as an inverse, got %+v err=%v", inv, err)
	}
}
