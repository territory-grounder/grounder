package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// TG-180: the coverage snapshot round-trips through the real pgx path, latest-wins on read, and an empty
// table reads ok=false (a named gap), never a phantom 0/0 snapshot. Gated like every live test.
func TestTG180ObservationCoverageRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the observation-coverage round-trip test")
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
	s := NewObservationCoverageStore(p)

	// The table may carry rows from other runs; assert on ordering relative to what THIS test writes.
	base := time.Now().Add(48 * time.Hour) // strictly newer than anything recorded so far
	older := ObservationCoverage{RecordedAt: base, Total: 40, Observed: 30, HealthyQuiet: 4, Unobservable: 6, Confirmed: 0, ProbeArmed: false}
	newer := ObservationCoverage{RecordedAt: base.Add(time.Minute), Total: 41, Observed: 30, HealthyQuiet: 4, Unobservable: 7, Confirmed: 2, ProbeArmed: true}
	if err := s.Record(ctx, older); err != nil {
		t.Fatalf("record older: %v", err)
	}
	if err := s.Record(ctx, newer); err != nil {
		t.Fatalf("record newer: %v", err)
	}
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM observation_coverage WHERE recorded_at >= $1", base)
	}()

	got, ok, err := s.Latest(ctx)
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	if got.Total != 41 || got.Unobservable != 7 || got.Confirmed != 2 || !got.ProbeArmed {
		t.Fatalf("Latest did not return the newest snapshot: %+v", got)
	}
	if err := s.Record(ctx, ObservationCoverage{Total: -1}); err == nil {
		t.Fatal("a negative count must be refused, not become the scorecard denominator")
	}
}
