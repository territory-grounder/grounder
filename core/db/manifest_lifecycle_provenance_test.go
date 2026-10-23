package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
)

// TG-532 — THE DEFECT, REPRODUCED AND FIXED. Two sessions propose the SAME operation, so they share one
// content-addressed action_id and ONE first-wins manifest row. Session A is approved and verified;
// session B resolves later. Before this change the row said "approved/match" with no owner, and any
// reader looking at session B concluded B was approved and matched. Now each label names its session.
//
// KILLING MUTATION: drop the *_ref writes from BackfillLifecycle (leave the COALESCE) — the refs read
// back empty and both ownership assertions fail, exactly as the pre-fix row behaved.
func TestManifestLifecycleLabelsNameTheirSession(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the manifest lifecycle provenance round-trip")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	s := NewManifestStore(p)

	// One SHAPE, sealed once — the identity two sessions share.
	target := fmt.Sprintf("tg532-web-%d", time.Now().UnixNano())
	m, err := manifest.New(manifest.Action{Target: target, OpClass: "restart-service", Op: "restart", Reversible: true},
		safety.BandPollPause, "plan#tg532", "pred#tg532")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := s.Seal(ctx, m); err != nil {
		t.Fatalf("seal write: %v", err)
	}

	// Session A: a human approved it, and the verifier scored it match.
	if err := s.BackfillLifecycle(ctx, m.ActionID, "TG-session-A", "approved", ""); err != nil {
		t.Fatalf("backfill A approval: %v", err)
	}
	if err := s.BackfillLifecycle(ctx, m.ActionID, "TG-session-A", "", safety.VerdictMatch); err != nil {
		t.Fatalf("backfill A verdict: %v", err)
	}
	lc, ok, err := s.Lifecycle(ctx, m.ActionID)
	if err != nil || !ok {
		t.Fatalf("read A: %v ok=%v", err, ok)
	}
	if lc.ApprovalChoice != "approved" || lc.ApprovalRef != "TG-session-A" ||
		lc.Verdict != "match" || lc.VerdictRef != "TG-session-A" {
		t.Fatalf("session A's labels must name session A: %+v", lc)
	}

	// Session B — the SAME shape, a different session — times out. Its label must claim only itself, and
	// must NOT restamp the verdict it never produced (the two refs move independently).
	if err := s.BackfillLifecycle(ctx, m.ActionID, "TG-session-B", "timeout", ""); err != nil {
		t.Fatalf("backfill B: %v", err)
	}
	lc, _, err = s.Lifecycle(ctx, m.ActionID)
	if err != nil {
		t.Fatalf("read B: %v", err)
	}
	if lc.ApprovalChoice != "timeout" || lc.ApprovalRef != "TG-session-B" {
		t.Fatalf("session B's approval must name session B: %+v", lc)
	}
	if lc.Verdict != "match" || lc.VerdictRef != "TG-session-A" {
		t.Fatalf("B's approval backfill must not restamp A's verdict ownership: %+v", lc)
	}

	// An unknown session writes honest absence, never the previous owner's name.
	if err := s.BackfillLifecycle(ctx, m.ActionID, "  ", "", safety.VerdictDeviation); err != nil {
		t.Fatalf("backfill unknown: %v", err)
	}
	lc, _, err = s.Lifecycle(ctx, m.ActionID)
	if err != nil {
		t.Fatalf("read unknown: %v", err)
	}
	if lc.Verdict != "deviation" || lc.VerdictRef != "" {
		t.Fatalf("an unowned label must read as unowned, not inherit A: %+v", lc)
	}
}

// The in-memory twin must carry the same provenance, or the CI oracles prove a seam production does not
// have (the reason this twin exists at all).
func TestMemManifestTwinCarriesProvenance(t *testing.T) {
	ctx := context.Background()
	m := NewMemManifestStore()
	mf, err := manifest.New(manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Reversible: true},
		safety.BandPollPause, "p", "pr")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Seal(ctx, mf); err != nil {
		t.Fatal(err)
	}
	if err := m.BackfillLifecycle(ctx, mf.ActionID, "TG-A", "approved", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.BackfillLifecycle(ctx, mf.ActionID, "TG-B", "", safety.VerdictMatch); err != nil {
		t.Fatal(err)
	}
	lc, ok, err := m.Lifecycle(ctx, mf.ActionID)
	if err != nil || !ok {
		t.Fatalf("read: %v ok=%v", err, ok)
	}
	if lc.ApprovalRef != "TG-A" || lc.VerdictRef != "TG-B" {
		t.Fatalf("the twin must track each label's own session: %+v", lc)
	}
}
