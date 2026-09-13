package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
)

// TestTriageConfidenceRoundTrip drives the REAL pgx path (INSERT -> SELECT) for the agent confidence scalar
// (spec/020 T-020-1, REQ-2003 — the observability half of the decision-tracer keystone). Like the prediction
// round-trip, it guards the exact failure mode the in-memory fake hides: an added struct field the SQL
// forgets to carry (the pgx-fake-hides-field-drop lesson). Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestTriageConfidenceRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the triage confidence round-trip test")
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
	s := NewTriageStore(p)

	uniq := fmt.Sprintf("triage-conf-it-%d", os.Getpid())
	proposed, stop := uniq+"-proposed", uniq+"-stop"
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = ANY($1)", []string{proposed, stop})
	}()

	// A proposing session carries the agent's emitted confidence; a grounded stop carries none (0).
	if err := s.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: proposed, Host: "web01", AlertRule: "HostDown", Band: "POLL_PAUSE",
		Outcome: "proposal", Proposed: true, Op: "restart", Confidence: 0.82,
	}); err != nil {
		t.Fatalf("record proposed: %v", err)
	}
	if err := s.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: stop, Host: "web02", AlertRule: "HostDown", Outcome: "no-proposal:stop", Proposed: false,
	}); err != nil {
		t.Fatalf("record stop: %v", err)
	}

	rows, err := s.UnjudgedSince(ctx, time.Hour, 100)
	if err != nil {
		t.Fatalf("unjudged: %v", err)
	}
	got := map[string]float64{}
	present := map[string]bool{}
	for _, r := range rows {
		got[r.ExternalRef] = r.Confidence
		present[r.ExternalRef] = true
	}
	if !present[proposed] || got[proposed] != 0.82 {
		t.Fatalf("proposed confidence: got %v (present=%v), want 0.82 — the pgx INSERT/SELECT dropped the field",
			got[proposed], present[proposed])
	}
	if !present[stop] || got[stop] != 0 {
		t.Fatalf("stop confidence: got %v (present=%v), want 0", got[stop], present[stop])
	}
}
