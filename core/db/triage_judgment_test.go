package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
)

// TestTriagePredictionRoundTrip is the guard seq B lacked: it drives the REAL pgx path (INSERT -> SELECT),
// so a struct field the SQL forgets to carry — as prediction/predicted were — fails HERE instead of
// silently dropping in production. Gated on TG_TEST_POSTGRES_DSN: CI has no Postgres, so the pure-Go fake
// sink (which stored the fields in memory) passed while the pgx impl dropped them; this closes that gap.
func TestTriagePredictionRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the triage round-trip integration test")
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

	uniq := fmt.Sprintf("triage-it-%d", os.Getpid())
	proposed, stop := uniq+"-proposed", uniq+"-stop"
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = ANY($1)", []string{proposed, stop})
	}()

	// A proposing session commits a prediction (the judge-readable line seq B renders); a grounded stop does not.
	predLine := "restart-service restart on web01 (reversible=true); target=web01; predicted-cascade-hosts=[db01]; predicted-rule-pairs=2"
	if err := s.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: proposed, Host: "web01", AlertRule: "HostDown", Band: "POLL_PAUSE",
		Outcome: "proposal", Proposed: true, Op: "restart", Prediction: predLine, Predicted: true,
	}); err != nil {
		t.Fatalf("record proposed: %v", err)
	}
	if err := s.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: stop, Host: "web02", AlertRule: "HostDown",
		Outcome: "no-proposal:stop", Proposed: false, Conclusion: "device disabled",
	}); err != nil {
		t.Fatalf("record stop: %v", err)
	}

	rows, err := s.UnjudgedSince(ctx, time.Hour, 100)
	if err != nil {
		t.Fatalf("unjudged: %v", err)
	}
	got := map[string]judge.TriageRow{}
	for _, r := range rows {
		got[r.ExternalRef] = r
	}
	// The committed prediction MUST survive the durable round-trip — the exact seq-B gap this test guards.
	if r, ok := got[proposed]; !ok || !r.Predicted || r.Prediction != predLine {
		t.Fatalf("proposed row must round-trip its prediction: found=%v predicted=%v pred=%q", ok, r.Predicted, r.Prediction)
	}
	// A grounded stop reaches no gate, so it honestly carries no prediction.
	if r, ok := got[stop]; !ok || r.Predicted || r.Prediction != "" {
		t.Fatalf("stop row must carry no prediction: found=%v predicted=%v pred=%q", ok, r.Predicted, r.Prediction)
	}
}
