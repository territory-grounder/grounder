package db

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/calibrate"
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

// ★ THE SECOND REFERENCE CLASS, against the REAL schema (TG-335).
//
// The published curve scored one outcome — blast-radius exactness — while the confidence alerts told the
// operator to compare it against diagnosis correctness. This drives the actual pgx join so a threshold that
// silently inverts, or a dimension filter that quietly matches every row, cannot pass on a fake.
func TestJudgedDiagnosisSamplesRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the diagnosis-calibration round-trip")
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

	uniq := fmt.Sprintf("diagcalib-it-%d", os.Getpid())
	good, bad, otherDim, zeroConf := uniq+"-good", uniq+"-bad", uniq+"-otherdim", uniq+"-zeroconf"
	refs := []string{good, bad, otherDim, zeroConf}
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = ANY($1)", refs)
		_, _ = p.Exec(ctx, "DELETE FROM session_judgment WHERE external_ref = ANY($1)", refs)
	}()

	tstore := NewTriageStore(p)
	seed := func(ref string, conf float64, dim string, score float64) {
		if err := tstore.RecordTriage(ctx, judge.TriageRow{
			ExternalRef: ref, Host: "h", AlertRule: "r", Outcome: "proposal", Proposed: true, Op: "restart", Confidence: conf,
		}); err != nil {
			t.Fatalf("triage %s: %v", ref, err)
		}
		if _, err := p.Exec(ctx,
			`INSERT INTO session_judgment (external_ref, dimension, score, comment, schema_version, rubric_version, action_id)
			 VALUES ($1,$2,$3,'',1,'t','')`, ref, dim, score); err != nil {
			t.Fatalf("judgment %s: %v", ref, err)
		}
	}
	seed(good, 0.9, "correct_diagnosis", 5)
	seed(bad, 0.8, "correct_diagnosis", 2)
	seed(otherDim, 0.7, "evidence_grounded", 5) // must NOT be scored as a diagnosis outcome
	seed(zeroConf, 0, "correct_diagnosis", 5)   // no stated confidence ⇒ nothing to calibrate

	got, err := NewCalibrationReadStore(p).JudgedDiagnosisSamples(ctx, 1000, 4)
	if err != nil {
		t.Fatalf("JudgedDiagnosisSamples: %v", err)
	}
	byConf := map[float64]bool{}
	for _, s := range got {
		byConf[s.Confidence] = s.Clean
	}
	if clean, ok := byConf[0.9]; !ok || !clean {
		t.Errorf("a score of 5 at the 4 threshold came back clean=%v present=%v, want clean", clean, ok)
	}
	if clean, ok := byConf[0.8]; !ok || clean {
		t.Errorf("a score of 2 at the 4 threshold came back clean=%v present=%v, want NOT clean — an "+
			"inverted threshold turns every wrong diagnosis into evidence the agent was right", clean, ok)
	}
	if _, ok := byConf[0.7]; ok {
		t.Error("an evidence_grounded row was scored as a diagnosis outcome. The dimension filter is what " +
			"makes this a reference CLASS; without it the curve mixes rubric dimensions and means nothing.")
	}
	if _, ok := byConf[0]; ok {
		t.Error("a session with no stated confidence entered the curve — there is no forecast to score")
	}
}

// The threshold is a judgement, so it must actually be honoured rather than being a decorative parameter.
func TestTheDiagnosisCleanThresholdIsHonoured(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database")
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

	ref := fmt.Sprintf("diagthresh-it-%d", os.Getpid())
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = $1", ref)
		_, _ = p.Exec(ctx, "DELETE FROM session_judgment WHERE external_ref = $1", ref)
	}()
	if err := NewTriageStore(p).RecordTriage(ctx, judge.TriageRow{
		ExternalRef: ref, Host: "h", AlertRule: "r", Outcome: "proposal", Proposed: true, Op: "restart", Confidence: 0.77,
	}); err != nil {
		t.Fatalf("triage: %v", err)
	}
	if _, err := p.Exec(ctx,
		`INSERT INTO session_judgment (external_ref, dimension, score, comment, schema_version, rubric_version, action_id)
		 VALUES ($1,'correct_diagnosis',4,'',1,'t','')`, ref); err != nil {
		t.Fatalf("judgment: %v", err)
	}
	store := NewCalibrationReadStore(p)
	find := func(ss []calibrate.Sample) (bool, bool) {
		for _, s := range ss {
			if s.Confidence == 0.77 {
				return s.Clean, true
			}
		}
		return false, false
	}
	at4, _ := store.JudgedDiagnosisSamples(ctx, 1000, 4)
	at5, _ := store.JudgedDiagnosisSamples(ctx, 1000, 5)
	c4, ok4 := find(at4)
	c5, ok5 := find(at5)
	if !ok4 || !ok5 {
		t.Fatal("the seeded row did not come back at both thresholds")
	}
	if !c4 || c5 {
		t.Errorf("score 4 read clean=%v at threshold 4 and clean=%v at threshold 5; want true then false. "+
			"A threshold that changes nothing is a knob an operator can set while believing it moved the "+
			"curve.", c4, c5)
	}
}
