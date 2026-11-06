package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
)

// TG-633: run-level A6 comes from triage_run over the window, with the session-level filters (step_count > 0,
// decision_ms > 0). The window is a unique stretch in 2001, so nothing else in the shared database lands in it.
// Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestAxisRunLevelA6ReadsTriageRun(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the run-level A6 round-trip")
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

	ref := fmt.Sprintf("tg633-a6-%d-%d", os.Getpid(), time.Now().UnixNano())
	since := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(os.Getpid()%10000) * time.Hour)
	until := since.Add(time.Hour)
	defer func() { _, _ = p.Exec(ctx, `DELETE FROM triage_run WHERE external_ref = $1`, ref) }()
	ts := NewTriageStore(p)
	for i, r := range []struct {
		steps int
		ms    int64
	}{{4, 1000}, {2, 3000}, {0, 0}} { // the third run never ran the loop: excluded from both measures
		if err := ts.RecordRun(ctx, fmt.Sprintf("run-%d", i), judge.TriageRow{ExternalRef: ref, StepCount: r.steps, DecisionMillis: r.ms}); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, `UPDATE triage_run SET recorded_at = $1 WHERE external_ref = $2 AND run_id = $3`,
			since.Add(time.Duration(i+1)*time.Minute), ref, fmt.Sprintf("run-%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	agg, err := NewAxisReadStore(p).AggregateScoped(ctx, AxisScope{Since: since, Until: until})
	if err != nil {
		t.Fatal(err)
	}
	if agg.RunStepsN != 2 || agg.RunMeanSteps != 3 || agg.RunDecisionN != 2 || agg.RunDecisionMedianMs != 2000 {
		t.Fatalf("want 2 runs with steps (mean 3) and 2 timed runs (median 2000ms), got steps n=%d mean=%v, timed n=%d median=%d",
			agg.RunStepsN, agg.RunMeanSteps, agg.RunDecisionN, agg.RunDecisionMedianMs)
	}
	// RunsSince is the table's earliest row: this test's first run (since+1m) unless an older row exists.
	if agg.RunsSince.IsZero() || agg.RunsSince.After(since.Add(time.Minute)) || !agg.RunsCover(agg.RunsSince) {
		t.Fatalf("want triage_run's earliest row at or before %v, covering a window that opens there, got %v",
			since.Add(time.Minute), agg.RunsSince)
	}
	if agg.RunsCover(agg.RunsSince.Add(-time.Second)) {
		t.Fatal("a window opening before the earliest run row is not covered")
	}
}
