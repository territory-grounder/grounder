package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/falsify"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/verify"
)

// The verify-time falsifiability writeback round-trips against a real Postgres: a committed prediction is
// read back as DUE, the score columns are written back ONCE (idempotent tp-null-only), the scored row is no
// longer due, and a cascade-stats window appends. Skipped in CI (no DB); runs under compose when
// TG_TEST_POSTGRES_DSN points at a migrated database.
func TestFalsifiabilityWritebackIntegration(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the pgx falsifiability-writeback integration test")
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

	// A unique plan_hash per run so a shared/append-only database never collides with a prior run's scored row.
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	planHash := "plan-fals-" + uniq
	actionID := "act-fals-" + uniq

	preds := NewPredictionStore(p)
	fstore := NewFalsifiabilityStore(p)
	cstore := NewCascadeStatsStore(p)

	rec := predict.PredictionRecord{
		Prediction: verify.Prediction{
			ActionID: actionID, PlanHash: planHash, TargetHost: "pve01", Site: "nl",
			PredictedHosts: map[string]struct{}{"n8n01": {}, "litellm01": {}},
			PredictedRules: map[string]struct{}{verify.RuleKey("n8n01", "HostDown"): {}},
		},
		ControlHosts:   map[string]struct{}{"web09": {}},
		SchemaVersion:  schema.Version(1),
		PredictionHash: "hash-" + uniq,
	}
	if err := preds.Commit(ctx, rec); err != nil {
		t.Fatalf("commit prediction: %v", err)
	}

	// committed_at defaults to now(); a future cutoff makes the fresh row due for scoring.
	future := time.Now().Add(time.Hour)
	due, err := fstore.DueForScoring(ctx, future, 500)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	var found *falsify.DuePrediction
	for i := range due {
		if due[i].Record.Prediction.PlanHash == planHash {
			found = &due[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("the committed prediction must be due for scoring")
	}
	if len(found.Record.Prediction.PredictedHosts) != 2 || len(found.Record.ControlHosts) != 1 {
		t.Fatalf("due prediction did not round-trip its host/control sets: %+v", found.Record)
	}

	updated, err := fstore.WriteScore(ctx, planHash, falsify.Score{TP: 2, FP: 0, FN: 0, ControlTP: 0, ControlFP: 1})
	if err != nil {
		t.Fatalf("write score: %v", err)
	}
	if !updated {
		t.Fatalf("the first score write must update the unscored row")
	}
	// idempotent: a second write is a no-op (the row is now tp-non-null) — the score is written exactly once.
	updated2, err := fstore.WriteScore(ctx, planHash, falsify.Score{TP: 99, FP: 99, FN: 99, ControlTP: 99, ControlFP: 99})
	if err != nil {
		t.Fatalf("second write score: %v", err)
	}
	if updated2 {
		t.Fatalf("the second score write must be a no-op (verify-time write happens once)")
	}

	// a scored row is no longer due.
	due2, err := fstore.DueForScoring(ctx, future, 500)
	if err != nil {
		t.Fatalf("due after score: %v", err)
	}
	for _, d := range due2 {
		if d.Record.Prediction.PlanHash == planHash {
			t.Fatalf("a scored prediction must not be returned as due")
		}
	}

	// the cascade-stats window append.
	if err := cstore.AppendWindow(ctx, falsify.CascadeWindow{
		Start: time.Now().Add(-time.Hour), End: time.Now(),
		RealTP: 2, ControlTP: 0, ControlRatio: 0, Falsifiable: true,
	}); err != nil {
		t.Fatalf("append cascade window: %v", err)
	}
}
