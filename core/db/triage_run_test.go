package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
)

// TG-633: one triage_run row per workflow run. A retry of the same run is a no-op; a second run of the same
// alert is a second row, which is exactly what session_triage (first-write-wins per alert) cannot hold.
// Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestTriageRunOneRowPerRun(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the triage_run round-trip")
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

	ref := fmt.Sprintf("tg633-run-%d-%d", os.Getpid(), time.Now().UnixNano())
	row := judge.TriageRow{ExternalRef: ref, Host: "app01", AlertRule: "Service-up/down", Outcome: "proposed", Proposed: true,
		OpClass: "restart-service", Confidence: 0.8, StepCount: 4, DecisionMillis: 9100, ModelTier: "primary",
		DecisionTier: "primary", HarnessFP: "fp-1", Mutated: true}
	if err := s.RecordRun(ctx, "run-a", row); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRun(ctx, "run-a", row); err != nil { // an activity retry of the same run
		t.Fatalf("a retry must be a no-op, not an error: %v", err)
	}
	second := row
	second.Outcome, second.StepCount, second.Mutated = "no-proposal:stop", 2, false
	if err := s.RecordRun(ctx, "run-b", second); err != nil {
		t.Fatal(err)
	}

	type got struct {
		run, outcome string
		steps        int
		ms           int64
		mutated      bool
		sv           int
	}
	rows, err := p.Query(ctx, `SELECT run_id, outcome, step_count, decision_ms, mutated, schema_version
		FROM triage_run WHERE external_ref = $1 ORDER BY run_id`, ref)
	if err != nil {
		t.Fatal(err)
	}
	var all []got
	for rows.Next() {
		var g got
		if err := rows.Scan(&g.run, &g.outcome, &g.steps, &g.ms, &g.mutated, &g.sv); err != nil {
			t.Fatal(err)
		}
		all = append(all, g)
	}
	rows.Close()
	want := []got{{"run-a", "proposed", 4, 9100, true, 1}, {"run-b", "no-proposal:stop", 2, 9100, false, 1}}
	if len(all) != 2 || all[0] != want[0] || all[1] != want[1] {
		t.Fatalf("want one row per run (retry deduplicated), got %+v", all)
	}

	if err := s.RecordRun(ctx, "", row); err == nil {
		t.Fatal("an empty run id must be refused")
	}
}
