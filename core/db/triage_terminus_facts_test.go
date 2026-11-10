package db

// TG-662: the terminus facts round-trip against a REAL Postgres (TG_TEST_DSN, migration 0122 applied). The whole
// risk is SQL — the NULL-when-unknown collateral mapping and the timestamp CASE — which a pgx fake cannot see.
//
// KILLING MUTATION (executed 2026-11-07): in MarkTerminusFacts, assign `collateral = f.CollateralObserved`
// unconditionally (dropping the CollateralKnown guard) — the unknown reading persists as FALSE (surveyed-quiet)
// and the "unknown stays unknown" assertion below goes red. Restored from a copy, green.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
)

func TestTriageTerminusFactsRoundTrip(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	s := NewTriageStore(p)
	ref := fmt.Sprintf("%s-tg662-%d", fixtureNS, time.Now().UnixNano())
	if err := s.RecordTriage(ctx, judge.TriageRow{ExternalRef: ref, Host: "web01", AlertRule: "NginxDown",
		Band: "AUTO", Outcome: "proposed", Proposed: true, Op: "start", OpClass: "start-service", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("record triage: %v", err)
	}

	// Fresh row: no facts yet — PENDING, never clear.
	f, found, err := s.TerminusFactsFor(ctx, ref)
	if err != nil || !found {
		t.Fatalf("facts for a fresh row: found=%v err=%v", found, err)
	}
	if f.FactsAt != nil || f.ConfirmedClear || f.ConfirmedClearAt != nil || f.CollateralKnown {
		t.Fatalf("a fresh row must read unsettled/unconfirmed/unknown, got %+v", f)
	}

	// Settled, clear, collateral UNKNOWN: unknown stays unknown (NULL), never surveyed-quiet.
	if err := s.MarkTerminusFacts(ctx, ref, TerminusFacts{Mutated: true, ConfirmedClear: true}); err != nil {
		t.Fatalf("mark: %v", err)
	}
	f, _, err = s.TerminusFactsFor(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if f.FactsAt == nil || !f.ConfirmedClear || f.ConfirmedClearAt == nil {
		t.Fatalf("a settled clear must carry both timestamps, got %+v", f)
	}
	if f.CollateralKnown {
		t.Fatalf("unknown collateral must round-trip as UNKNOWN (NULL), not as a surveyed reading: %+v", f)
	}

	// Settled, clear, collateral surveyed QUIET.
	if err := s.MarkTerminusFacts(ctx, ref, TerminusFacts{Mutated: true, ConfirmedClear: true, CollateralKnown: true}); err != nil {
		t.Fatal(err)
	}
	f, _, _ = s.TerminusFactsFor(ctx, ref)
	if !f.CollateralKnown || f.CollateralObserved {
		t.Fatalf("surveyed-quiet must round-trip as known=true observed=false, got %+v", f)
	}

	// Settled, NOT clear, collateral observed: the clear timestamp is NULL, the observation true.
	if err := s.MarkTerminusFacts(ctx, ref, TerminusFacts{Mutated: true, CollateralKnown: true, CollateralObserved: true}); err != nil {
		t.Fatal(err)
	}
	f, _, _ = s.TerminusFactsFor(ctx, ref)
	if f.ConfirmedClear || f.ConfirmedClearAt != nil || !f.CollateralKnown || !f.CollateralObserved || f.FactsAt == nil {
		t.Fatalf("not-clear + observed collateral round-trip: %+v", f)
	}

	// Absent ref ⇒ found=false; blank refs refuse on both paths.
	if _, found, err := s.TerminusFactsFor(ctx, ref+"-absent"); err != nil || found {
		t.Fatalf("an absent ref must read found=false, got found=%v err=%v", found, err)
	}
	if err := s.MarkTerminusFacts(ctx, "", TerminusFacts{}); err == nil {
		t.Fatal("a blank external_ref must be refused on mark")
	}
	if _, _, err := s.TerminusFactsFor(ctx, ""); err == nil {
		t.Fatal("a blank external_ref must be refused on read")
	}
}
