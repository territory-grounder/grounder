package db

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/judge"
)

// TestPriorSessionsReadsHostHistoryNewestFirst drives the REAL pgx path behind the get-incident-history
// tool (INSERT via the production TriageStore writer -> SELECT via IncidentHistoryStore), so a column the
// SQL forgets to carry — the exact silent-drop class migration 0014 was needed for — fails HERE. Gated on
// TG_TEST_POSTGRES_DSN (CI has no Postgres); the formatting/family-fold logic has its own pure unit tests
// in modules/observability/incidenthistory.
func TestPriorSessionsReadsHostHistoryNewestFirst(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the incident-history integration test")
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

	uniq := fmt.Sprintf("hist-it-%d", os.Getpid())
	host, otherHost := uniq+"-web01", uniq+"-web02"
	refA, refB, refOther := uniq+"-a", uniq+"-b", uniq+"-other"
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = ANY($1)", []string{refA, refB, refOther})
	}()

	w := NewTriageStore(p)
	// Two sessions on the host (a healed proposal, then a stood-down one) + one on ANOTHER host that the
	// host-scoped read must never return. created_at defaults to now(), so insertion order fixes recency:
	// refA first (older), refB second (newer).
	if err := w.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: refA, Host: host, AlertRule: "Devices-up/down", Band: "POLL_PAUSE",
		Outcome: "proposed", Proposed: true, Op: "start", OpClass: "start-guest", Mutated: true,
		Conclusion: "guest stopped; start-guest healed it",
	}); err != nil {
		t.Fatalf("record refA: %v", err)
	}
	if err := w.MarkCleared(ctx, refA, true); err != nil {
		t.Fatalf("mark cleared refA: %v", err)
	}
	if err := w.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: refB, Host: host, AlertRule: "HostDown",
		Outcome: "no-proposal:stop", Conclusion: "device disabled on purpose",
	}); err != nil {
		t.Fatalf("record refB: %v", err)
	}
	if err := w.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: refOther, Host: otherHost, AlertRule: "HostDown", Outcome: "no-proposal:stop",
	}); err != nil {
		t.Fatalf("record refOther: %v", err)
	}

	rows, err := NewIncidentHistoryStore(p).PriorSessions(ctx, host, 10)
	if err != nil {
		t.Fatalf("PriorSessions: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want exactly the host's 2 sessions (never another host's), got %d: %+v", len(rows), rows)
	}
	// Newest first.
	if rows[0].ExternalRef != refB || rows[1].ExternalRef != refA {
		t.Fatalf("want newest-first [%s %s], got [%s %s]", refB, refA, rows[0].ExternalRef, rows[1].ExternalRef)
	}
	// Every projected column survives the round-trip.
	a := rows[1]
	if a.AlertRule != "Devices-up/down" || a.Outcome != "proposed" || a.OpClass != "start-guest" ||
		!a.Proposed || !a.Mutated || !a.ConfirmedClear || a.Conclusion != "guest stopped; start-guest healed it" ||
		a.CreatedAt.IsZero() {
		t.Fatalf("healed session's columns dropped in the round-trip: %+v", a)
	}
	b := rows[0]
	if b.Outcome != "no-proposal:stop" || b.OpClass != "" || b.Proposed || b.Mutated || b.ConfirmedClear {
		t.Fatalf("stood-down session's columns dropped in the round-trip: %+v", b)
	}

	// The limit bounds the read.
	one, err := NewIncidentHistoryStore(p).PriorSessions(ctx, host, 1)
	if err != nil {
		t.Fatalf("PriorSessions limit 1: %v", err)
	}
	if len(one) != 1 || one[0].ExternalRef != refB {
		t.Fatalf("limit must keep the newest row, got %+v", one)
	}
}
