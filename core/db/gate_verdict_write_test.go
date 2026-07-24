package db

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/trace"
)

// TestGateVerdictRoundTrip drives the REAL pgx path (INSERT -> ordered SELECT) for the per-gate verdict trail
// (spec/020 T-020-7, REQ-2007). It writes several gate rows out of ordinal order, reads them back, and asserts
// they return ORDERED by ordinal with every field carried — so a dropped column or a lost order fails HERE
// rather than silently in prod (the pgx-fake-hides-field-drop lesson). Gated on TG_TEST_POSTGRES_DSN.
func TestGateVerdictRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the gate-verdict round-trip test")
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
	s := NewGateVerdictStore(p)

	aid := fmt.Sprintf("gv-it-%d", os.Getpid())
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM interceptor_gate_verdict WHERE action_id = $1", aid) }()

	// Emit out of order to prove the ORDERED read (not insert order) is what the tracer sees.
	want := []trace.GateVerdict{
		{Ordinal: 1, Gate: "admission", Verdict: "pass", ActionID: aid, ExternalRef: "ext-gv"},
		{Ordinal: 3, Gate: "structure", Verdict: "pass", ActionID: aid, ExternalRef: "ext-gv"},
		{Ordinal: 2, Gate: "never-auto-floor", Verdict: "pass", ActionID: aid, ExternalRef: "ext-gv"},
		{Ordinal: 4, Gate: "evidence", Verdict: "refuse", Reason: "evidence unbound", ActionID: aid, ExternalRef: "ext-gv"},
	}
	for _, gv := range want {
		if err := s.Emit(ctx, gv); err != nil {
			t.Fatalf("emit %d: %v", gv.Ordinal, err)
		}
	}
	got, err := s.GateRows(ctx, aid)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("gate rows: got %d, want 4", len(got))
	}
	wantOrder := []string{"admission", "never-auto-floor", "structure", "evidence"}
	for i, r := range got {
		if r.Ordinal != i+1 || r.Gate != wantOrder[i] {
			t.Fatalf("row %d = (ord %d, %q), want (ord %d, %q) — ordered-by-ordinal read failed", i, r.Ordinal, r.Gate, i+1, wantOrder[i])
		}
		if r.ActionID != aid || r.ExternalRef != "ext-gv" {
			t.Fatalf("row %d keys = (%q,%q), want (%q,ext-gv) — a correlation key was dropped", i, r.ActionID, r.ExternalRef, aid)
		}
	}
	if got[3].Verdict != "refuse" || got[3].Reason != "evidence unbound" {
		t.Fatalf("refusing row = (%q,%q), want (refuse, evidence unbound)", got[3].Verdict, got[3].Reason)
	}

	// Append-only (REQ-2016): the DML runtime role holds no UPDATE/DELETE on this evidence table. The migration
	// role used here CAN update, so this asserts the ROW is immutable-by-grant only indirectly — the grant proof
	// belongs to a tg_runtime-connected check; here we at least prove the append + ordered read are stable.
	if _, err := s.GateRows(ctx, aid); err != nil {
		t.Fatalf("re-read: %v", err)
	}
}
