package db

import (
	"context"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
)

// TestManifestLifecycleRoundTrip drives the REAL pgx path (Seal INSERT → BackfillLifecycle UPDATE → Lifecycle
// SELECT) for a sealed manifest's non-hashed lifecycle labels (spec/020 T-020-4, REQ-2006). It guards the exact
// failure the in-memory twin hides — an UPDATE/SELECT the SQL drops — and confirms COALESCE semantics (a later
// verdict backfill must NOT erase an earlier approval_choice backfill) plus that the content-hash Assert still
// passes after the backfill (INV-07: only non-hashed columns changed). Gated on TG_TEST_POSTGRES_DSN.
func TestManifestLifecycleRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the manifest lifecycle round-trip test")
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
	s := NewManifestStore(p)

	m, err := manifest.New(
		manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Reversible: true},
		safety.BandAuto, "ph-lc-it", "predh-lc-it")
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM action_manifest WHERE action_id = $1", m.ActionID) }()

	if err := s.Seal(ctx, m); err != nil {
		t.Fatalf("seal: %v", err)
	}
	// Before backfill: both lifecycle labels are NULL → read back as empty.
	if lc, ok, lerr := s.Lifecycle(ctx, m.ActionID); lerr != nil || !ok || lc.ApprovalChoice != "" || lc.Verdict != "" {
		t.Fatalf("pre-backfill lifecycle = (%+v, %v, %v), want empty/present", lc, ok, lerr)
	}

	// Backfill approval, then verdict (two separate lifecycle points) — COALESCE must preserve both.
	if err := s.BackfillLifecycle(ctx, m.ActionID, "approved", ""); err != nil {
		t.Fatalf("backfill approval: %v", err)
	}
	if err := s.BackfillLifecycle(ctx, m.ActionID, "", safety.VerdictMatch); err != nil {
		t.Fatalf("backfill verdict: %v", err)
	}
	lc, ok, err := s.Lifecycle(ctx, m.ActionID)
	if err != nil {
		t.Fatalf("lifecycle read: %v", err)
	}
	if !ok || lc.ApprovalChoice != "approved" || lc.Verdict != "match" {
		t.Fatalf("lifecycle round-trip = %+v (present=%v), want approved/match — the pgx UPDATE/SELECT dropped a label or COALESCE erased one", lc, ok)
	}

	// INV-07: the sealed binding still re-asserts — the backfill touched only non-hashed columns.
	if _, got, gerr := s.Get(ctx, m.ActionID); gerr != nil || !got {
		t.Fatalf("post-backfill Get/Assert failed — a lifecycle backfill must NOT tamper the sealed binding: ok=%v err=%v", got, gerr)
	}
}
