package db

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/judge"
)

// TG-527: the trajectory column round-trips through the REAL pgx path with the Diagnosis NULL-vs-empty
// discipline — a session with steps persists them in order, a session with none persists '[]' (never
// NULL, which is reserved for pre-0104 rows). KILLING MUTATIONS: drop `trajectory` from the INSERT column
// list → the steps assert fails; write `row.Trajectory` unguarded instead of the nil→[] substitution →
// the non-NULL assert fails.
func TestTG527TrajectoryColumnRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the trajectory round-trip test")
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

	uniq := fmt.Sprintf("tg527-traj-%d", os.Getpid())
	withSteps, noSteps := uniq+"-steps", uniq+"-none"
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = ANY($1)", []string{withSteps, noSteps})
	}()

	steps := []judge.TrajectoryStep{{Tool: "librenms.alerts", ArgsKey: "host=web01"}, {Tool: "hostdiag.journal", ArgsKey: "unit=nginx"}}
	if err := s.RecordTriage(ctx, judge.TriageRow{ExternalRef: withSteps, Host: "web01", AlertRule: "HostDown",
		Outcome: "no-proposal:stop", Conclusion: "x", Trajectory: steps}); err != nil {
		t.Fatalf("record with steps: %v", err)
	}
	if err := s.RecordTriage(ctx, judge.TriageRow{ExternalRef: noSteps, Host: "web02", AlertRule: "HostDown",
		Outcome: "no-proposal:stop", Conclusion: "y"}); err != nil {
		t.Fatalf("record without steps: %v", err)
	}

	var raw []byte
	if err := p.QueryRow(ctx, `SELECT trajectory FROM session_triage WHERE external_ref = $1`, withSteps).Scan(&raw); err != nil {
		t.Fatalf("read steps row: %v", err)
	}
	var got []judge.TrajectoryStep
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("persisted trajectory is not the step array: %v (%s)", err, raw)
	}
	if len(got) != 2 || got[0].Tool != "librenms.alerts" || got[1].ArgsKey != "unit=nginx" {
		t.Fatalf("trajectory did not round-trip in order: %+v", got)
	}

	var isNull bool
	var rawNone []byte
	if err := p.QueryRow(ctx, `SELECT trajectory IS NULL, trajectory FROM session_triage WHERE external_ref = $1`, noSteps).Scan(&isNull, &rawNone); err != nil {
		t.Fatalf("read no-steps row: %v", err)
	}
	if isNull || string(rawNone) != "[]" {
		t.Fatalf("a recorded session with no steps must persist '[]' (recorded-and-empty), got null=%v raw=%s — NULL is reserved for pre-0104 rows", isNull, rawNone)
	}
}
