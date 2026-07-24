package db

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/falsify"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/verify"
)

// TestCalibrationPairedSamplesRoundTrip drives the REAL pgx join for the confidence calibrator (spec/020
// T-020-15, REQ-2021): session_triage.confidence ⋈ infragraph_prediction (the scored falsify outcome) by
// external_ref. It guards the exact failure the in-memory fake hides — a SELECT/JOIN that drops the
// external_ref correlation or mis-derives the verified-clean flag (the pgx-fake-hides-field-drop lesson).
// Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestCalibrationPairedSamplesRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the calibration join round-trip test")
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

	uniq := fmt.Sprintf("calib-it-%d", os.Getpid())
	cleanRef, dirtyRef, unscoredRef, zeroRef := uniq+"-clean", uniq+"-dirty", uniq+"-unscored", uniq+"-zeroconf"
	refs := []string{cleanRef, dirtyRef, unscoredRef, zeroRef}
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = ANY($1)", refs)
		_, _ = p.Exec(ctx, "DELETE FROM infragraph_prediction WHERE external_ref = ANY($1)", refs)
	}()

	tstore := NewTriageStore(p)
	pstore := NewPredictionStore(p)
	fstore := NewFalsifiabilityStore(p)

	// Helper: a triage with a stated confidence + a prediction carrying the same external_ref.
	seed := func(ref string, conf float64, planHash, actionID string) {
		if err := tstore.RecordTriage(ctx, judge.TriageRow{
			ExternalRef: ref, Host: "h", AlertRule: "r", Outcome: "proposal", Proposed: true, Op: "restart", Confidence: conf,
		}); err != nil {
			t.Fatalf("triage %s: %v", ref, err)
		}
		if err := pstore.Commit(ctx, predict.PredictionRecord{
			Prediction: verify.Prediction{
				ActionID: actionID, PlanHash: planHash, TargetHost: "h", Site: "nl",
				PredictedHosts: map[string]struct{}{"h": {}},
			},
			ControlHosts: map[string]struct{}{"c": {}}, SchemaVersion: schema.Version(1),
			PredictionHash: planHash + "-hash", ExternalRef: ref,
		}); err != nil {
			t.Fatalf("prediction %s: %v", ref, err)
		}
	}

	seed(cleanRef, 0.9, uniq+"-clean-plan", uniq+"-clean-act") // will score clean (fp=0,fn=0)
	seed(dirtyRef, 0.4, uniq+"-dirty-plan", uniq+"-dirty-act") // will score dirty (fp>0)
	seed(unscoredRef, 0.7, uniq+"-uns-plan", uniq+"-uns-act")  // left UNSCORED (tp NULL) — excluded
	seed(zeroRef, 0.0, uniq+"-zero-plan", uniq+"-zero-act")    // confidence=0 = the pre-fix missing value — must be EXCLUDED even when scored

	// Score three of the four: clean (fp=0,fn=0), dirty (fp=1), and the zero-confidence row (clean).
	if _, err := fstore.WriteScore(ctx, uniq+"-clean-plan", falsify.Score{TP: 1, FP: 0, FN: 0}); err != nil {
		t.Fatalf("score clean: %v", err)
	}
	if _, err := fstore.WriteScore(ctx, uniq+"-dirty-plan", falsify.Score{TP: 1, FP: 1, FN: 0}); err != nil {
		t.Fatalf("score dirty: %v", err)
	}
	if _, err := fstore.WriteScore(ctx, uniq+"-zero-plan", falsify.Score{TP: 1, FP: 0, FN: 0}); err != nil {
		t.Fatalf("score zero-conf: %v", err)
	}

	samples, err := NewCalibrationReadStore(p).PairedSamples(ctx, 1000)
	if err != nil {
		t.Fatalf("paired samples: %v", err)
	}
	got := map[float64]bool{}
	for _, s := range samples {
		got[s.Confidence] = s.Clean
	}
	// The clean-scored pair (0.9) reads Clean=true; the dirty-scored pair (0.4) reads Clean=false; the
	// UNSCORED pair (0.7) must be ABSENT (no verified outcome yet — never a false "clean").
	if v, ok := got[0.9]; !ok || v != true {
		t.Fatalf("clean pair: got (%v,present=%v), want (true) — the external_ref join or clean-flag dropped", v, ok)
	}
	if v, ok := got[0.4]; !ok || v != false {
		t.Fatalf("dirty pair: got (%v,present=%v), want (false)", v, ok)
	}
	if _, ok := got[0.7]; ok {
		t.Fatalf("unscored prediction (0.7) must NOT be a sample — it has no verified outcome yet")
	}
	// The confidence=0 row (the pre-fix missing value) must be ABSENT even though it scored clean — else the
	// reliability curve gets a fabricated 0.0 bin (measured live: 197 such rows). See PairedSamples' t.confidence>0.
	if _, ok := got[0.0]; ok {
		t.Fatalf("confidence=0 (pre-fix missing value) must NOT be a sample — the t.confidence>0 filter should exclude it")
	}
}
