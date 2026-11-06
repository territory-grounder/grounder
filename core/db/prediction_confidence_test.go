package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/verify"
)

// TG-189 — the per-host confidence must SURVIVE THE DATABASE, and that is the whole risk.
//
// The Brier score is computed by the falsifiability scorer HOURS after the prediction was committed, from
// a row read back out of Postgres — the in-memory graph state is long gone. So a change that threads
// confidence through Go structs and stops at the SQL boundary would pass every unit test in core/verify
// and still produce "unscored" forever in production. That failure is invisible to a fake.
func TestPerHostConfidenceSurvivesTheCommitAndReadBack(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the prediction confidence round-trip")
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

	plan := fmt.Sprintf("tg189-%d", os.Getpid())
	action := plan + "-act"
	t.Cleanup(func() { _, _ = p.Exec(ctx, "DELETE FROM infragraph_prediction WHERE plan_hash = $1", plan) })

	rec := predict.PredictionRecord{
		Prediction: verify.Prediction{
			ActionID: action, PlanHash: plan, TargetHost: "web01", Site: "nl",
			PredictedHosts: map[string]struct{}{"web01": {}, "db01": {}},
			PredictedRules: map[string]struct{}{},
			HostConfidence: map[string]float64{"web01": 0.95, "db01": 0.42},
		},
		ControlHosts:   map[string]struct{}{},
		SchemaVersion:  schema.Version(1),
		PredictionHash: "h-" + plan,
		ExternalRef:    plan,
	}
	if err := NewPredictionStore(p).Commit(ctx, rec); err != nil {
		t.Fatalf("commit: %v", err)
	}

	due, err := NewFalsifiabilityStore(p).DueForScoring(ctx, time.Now().Add(time.Hour), 200)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	var got *verify.Prediction
	for i := range due {
		if due[i].Record.Prediction.PlanHash == plan {
			got = &due[i].Record.Prediction
		}
	}
	if got == nil {
		t.Fatalf("the committed prediction did not come back from DueForScoring — %d row(s) returned", len(due))
	}
	if len(got.HostConfidence) != 2 {
		t.Fatalf("HostConfidence read back as %v — it died at the SQL boundary, which is exactly where a "+
			"Go-only change would still pass every unit test and produce 'unscored' forever in production",
			got.HostConfidence)
	}
	if got.HostConfidence["web01"] != 0.95 || got.HostConfidence["db01"] != 0.42 {
		t.Errorf("confidences came back as %v, want web01=0.95 db01=0.42", got.HostConfidence)
	}
	// And the point of carrying it: the score is now computable from a row, not from memory.
	score, n, ok := got.Brier(map[string]struct{}{"web01": {}})
	if !ok || n != 2 {
		t.Fatalf("Brier over the read-back row reported ok=%v n=%d, want a score over 2 hosts", ok, n)
	}
	// web01: (0.95-1)^2 = 0.0025 ; db01: (0.42-0)^2 = 0.1764 ; mean = 0.08945
	if score < 0.089 || score > 0.090 {
		t.Errorf("Brier = %v, want ~0.08945 from the persisted confidences", score)
	}
}

// A prediction with NO confidence must persist as SQL NULL and read back as unscored — not as an empty
// object that later reads as a score of zero.
func TestAPredictionWithoutConfidencePersistsAsNullAndReadsBackUnscored(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the prediction confidence round-trip")
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

	plan := fmt.Sprintf("tg189-null-%d", os.Getpid())
	t.Cleanup(func() { _, _ = p.Exec(ctx, "DELETE FROM infragraph_prediction WHERE plan_hash = $1", plan) })

	if err := NewPredictionStore(p).Commit(ctx, predict.PredictionRecord{
		Prediction: verify.Prediction{
			ActionID: plan + "-act", PlanHash: plan, TargetHost: "web01", Site: "nl",
			PredictedHosts: map[string]struct{}{"web01": {}},
			PredictedRules: map[string]struct{}{},
			// HostConfidence deliberately absent — a flat DependencyGraph model.
		},
		ControlHosts: map[string]struct{}{}, SchemaVersion: schema.Version(1),
		PredictionHash: "h-" + plan, ExternalRef: plan,
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var isNull bool
	if err := p.QueryRow(ctx,
		"SELECT predicted_host_confidence IS NULL FROM infragraph_prediction WHERE plan_hash = $1", plan,
	).Scan(&isNull); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !isNull {
		t.Error("a prediction carrying no confidence stored a non-NULL value — '{}' in the column would " +
			"later be indistinguishable from a model that genuinely scored everything at zero")
	}
}
