package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

// Gated on TG_TEST_POSTGRES_DSN: the boot reconciliation's ledger read returns the NEWEST row matching any of the
// patterns — the mode-governing event, not merely the newest row of any kind — and the rung read round-trips.
func TestLatestDecisionMatchingReturnsTheNewestGoverningRow(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the boot-reconcile ledger read")
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
	ls := NewLedgerStore(p)
	seq, hash, err := ls.Tail(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l := audit.NewLedgerFromTail(seq, hash)
	uniq := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	for _, d := range []audit.GovDecision{
		{Decision: "mode-transition:Shadow->Semi-auto", Reason: "boot-reads-test " + uniq, ActionID: "mt-" + uniq},
		{Decision: "classify:AUTO", Reason: "noise " + uniq, ActionID: "c-" + uniq},
		{Decision: "safety:halt", Reason: "boot-reads-test " + uniq, ActionID: "halt-" + uniq},
		{Decision: "classify:AUTO", Reason: "noise " + uniq, ActionID: "c2-" + uniq},
	} {
		e, err := l.Append(d)
		if err != nil {
			t.Fatal(err)
		}
		if err := ls.PersistContext(ctx, e); err != nil {
			t.Fatalf("persist: %v", err)
		}
	}
	got, found, err := ls.LatestDecisionMatching(ctx, []string{"mode-transition:%", "mode-seed:%", "safety:halt", "safety:breaker-trip"})
	if err != nil || !found {
		t.Fatalf("want the halt row, got found=%v err=%v", found, err)
	}
	if got.Decision != "safety:halt" || got.ActionID != "halt-"+uniq {
		t.Fatalf("the NEWEST governing row is the halt, not the earlier transition or the noise after it: %+v", got)
	}
	if _, found, err := ls.LatestDecisionMatching(ctx, []string{"never-written:%"}); err != nil || found {
		t.Fatalf("no match is (false, nil), got found=%v err=%v", found, err)
	}
	if _, found, err := ls.LatestDecisionMatching(ctx, nil); err != nil || found {
		t.Fatalf("no patterns is (false, nil), got found=%v err=%v", found, err)
	}

	gs := NewPolicyGraduationStore(p)
	if _, err := gs.Rungs(ctx); err != nil {
		t.Fatalf("rungs: %v", err)
	}
	if _, err := gs.VerifiedCleanCredits(ctx); err != nil {
		t.Fatalf("credits: %v", err)
	}
}
