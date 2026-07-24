package falsify

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/verify"
)

var fixedNow = time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

// samplePrediction is a committed prediction over pve01/nl: predicted cascade {n8n01, litellm01} (both with
// a HostDown rule), degree-preserving control {web09}.
func samplePrediction(planHash, actionID string) predict.PredictionRecord {
	return predict.PredictionRecord{
		Prediction: verify.Prediction{
			ActionID: actionID, PlanHash: planHash, TargetHost: "pve01", Site: "nl",
			PredictedHosts: map[string]struct{}{"n8n01": {}, "litellm01": {}},
			PredictedRules: map[string]struct{}{
				verify.RuleKey("n8n01", "HostDown"):     {},
				verify.RuleKey("litellm01", "HostDown"): {},
			},
		},
		ControlHosts:   map[string]struct{}{"web09": {}},
		SchemaVersion:  schema.Version(1),
		PredictionHash: "hash-" + planHash,
	}
}

func newScorer(store *MemStore, observed []verify.ObservedAlert) *Scorer {
	return &Scorer{
		Unscored: store, Scores: store, Verdicts: store, CascadeStats: store,
		Observe: func(context.Context, string, string) []verify.ObservedAlert { return observed },
		Window:  10 * time.Minute,
		Now:     func() time.Time { return fixedNow },
	}
}

// The real prediction catches the observed cascade and beats its control: tp/fp/fn are written back, the
// verdict is match, and the accumulated cascade window is falsifiable (control caught none of the real hits).
func TestScoreDueScoresRealPredictionAndBeatsControl(t *testing.T) {
	store := NewMemStore()
	store.Seed(samplePrediction("plan-1", "act-1"), fixedNow.Add(-time.Hour))
	observed := []verify.ObservedAlert{
		{Host: "n8n01", Rule: "HostDown", Site: "nl"},
		{Host: "litellm01", Rule: "HostDown", Site: "nl"},
		{Host: "pve01", Rule: "HostDown", Site: "nl"}, // the target's own alert — excluded, never a cascade hit
	}
	res, err := newScorer(store, observed).ScoreDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Scored != 1 || res.SumRealTP != 2 || res.SumControlTP != 0 || res.Deviations != 0 {
		t.Fatalf("expected 1 scored, real_tp=2 control_tp=0 no deviations, got %+v", res)
	}
	sc, ok := store.ScoreOf("plan-1")
	if !ok || sc != (Score{TP: 2, FP: 0, FN: 0, ControlTP: 0, ControlFP: 1}) {
		t.Fatalf("score writeback wrong: %+v ok=%v", sc, ok)
	}
	if v, _ := store.VerdictOf("act-1"); v != safety.VerdictMatch {
		t.Fatalf("expected match verdict, got %q", v)
	}
	w := store.Windows()
	if len(w) != 1 || w[0].RealTP != 2 || w[0].ControlTP != 0 || !w[0].Falsifiable {
		t.Fatalf("expected one falsifiable cascade window real_tp=2 control_tp=0, got %+v", w)
	}
}

// A surprise host (the prediction never named it) is a DEVIATION — and a deviation is never-auto by
// construction (verify.AutoResolvable is false). Here the control host happens to be the one that alerted, so
// the window is correctly flagged NON-falsifiable (only the random control "caught" it).
func TestScoreDueSurpriseHostIsDeviationAndNeverAuto(t *testing.T) {
	store := NewMemStore()
	store.Seed(samplePrediction("plan-2", "act-2"), fixedNow.Add(-time.Hour))
	observed := []verify.ObservedAlert{{Host: "web09", Rule: "HostDown", Site: "nl"}} // surprise (also the control)
	res, err := newScorer(store, observed).ScoreDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Scored != 1 || res.Deviations != 1 {
		t.Fatalf("expected 1 scored with 1 deviation, got %+v", res)
	}
	// The surprise host is consumed straight off the typed verify.VerdictDetail (not re-derived here) and
	// surfaced for the worker log.
	if len(res.SurpriseHosts) != 1 || res.SurpriseHosts[0] != "web09" {
		t.Fatalf("expected the deviation's surprise host web09 in the result, got %v", res.SurpriseHosts)
	}
	v, ok := store.VerdictOf("act-2")
	if !ok || v != safety.VerdictDeviation {
		t.Fatalf("expected a persisted deviation verdict, got %q ok=%v", v, ok)
	}
	if verify.AutoResolvable(v) {
		t.Fatal("a deviation must be never-auto (AutoResolvable=false) — the never-auto rule")
	}
	sc, _ := store.ScoreOf("plan-2")
	if sc != (Score{TP: 0, FP: 2, FN: 1, ControlTP: 1, ControlFP: 0}) {
		t.Fatalf("deviation score wrong: %+v", sc)
	}
	if w := store.Windows(); len(w) != 1 || w[0].Falsifiable {
		t.Fatalf("a window the control won must be NON-falsifiable, got %+v", w)
	}
}

// A quiet post-state (nothing observed) is a MATCH with zero real hits — the honest "no cascade" case, never
// a fabricated hit. The prediction's named hosts are false positives (predicted but did not alert).
func TestScoreDueQuietPostStateIsMatch(t *testing.T) {
	store := NewMemStore()
	store.Seed(samplePrediction("plan-3", "act-3"), fixedNow.Add(-time.Hour))
	res, err := newScorer(store, nil).ScoreDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Scored != 1 || res.SumRealTP != 0 {
		t.Fatalf("expected 1 scored with real_tp=0, got %+v", res)
	}
	if v, _ := store.VerdictOf("act-3"); v != safety.VerdictMatch {
		t.Fatalf("a quiet post-state is a match, got %q", v)
	}
	if sc, _ := store.ScoreOf("plan-3"); sc != (Score{TP: 0, FP: 2, FN: 0, ControlTP: 0, ControlFP: 1}) {
		t.Fatalf("quiet score wrong: %+v", sc)
	}
}

// Scoring is idempotent: a second pass scores nothing (the first won the tp-null-only write), so a prediction
// is never double-counted into the cascade windows.
func TestScoreDueIsIdempotent(t *testing.T) {
	store := NewMemStore()
	store.Seed(samplePrediction("plan-4", "act-4"), fixedNow.Add(-time.Hour))
	observed := []verify.ObservedAlert{{Host: "n8n01", Rule: "HostDown", Site: "nl"}}
	sc := newScorer(store, observed)
	if res, err := sc.ScoreDue(context.Background()); err != nil || res.Scored != 1 {
		t.Fatalf("first pass must score 1: %+v %v", res, err)
	}
	res2, err := sc.ScoreDue(context.Background())
	if err != nil || res2.Scored != 0 {
		t.Fatalf("second pass must score 0 (idempotent): %+v %v", res2, err)
	}
	if w := store.Windows(); len(w) != 1 {
		t.Fatalf("a re-scored prediction must not append a second window, got %d", len(w))
	}
}

// A prediction committed INSIDE the observation window is not yet due — the cascade has not had time to
// manifest, so it must not be scored prematurely.
func TestScoreDueRespectsObservationWindow(t *testing.T) {
	store := NewMemStore()
	store.Seed(samplePrediction("plan-5", "act-5"), fixedNow.Add(-time.Minute)) // 1m old, window is 10m
	res, err := newScorer(store, []verify.ObservedAlert{{Host: "n8n01", Rule: "HostDown", Site: "nl"}}).ScoreDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Scored != 0 {
		t.Fatalf("a too-recent prediction must not be scored, got %+v", res)
	}
	if _, ok := store.ScoreOf("plan-5"); ok {
		t.Fatal("an in-window prediction must remain unscored")
	}
}

// Unwired collaborators make the scorer inert (honest zeros, no panic) — a missing observer or store never
// fabricates a score and never crashes the worker loop.
func TestScoreDueInertWhenUnwired(t *testing.T) {
	if res, err := (&Scorer{}).ScoreDue(context.Background()); err != nil || res.Scored != 0 {
		t.Fatalf("a fully-unwired scorer must be inert, got %+v %v", res, err)
	}
	store := NewMemStore()
	store.Seed(samplePrediction("plan-6", "act-6"), fixedNow.Add(-time.Hour))
	// store wired but no observer ⇒ still inert (we can never observe, so we never score).
	s := &Scorer{Unscored: store, Scores: store, Now: func() time.Time { return fixedNow }}
	if res, err := s.ScoreDue(context.Background()); err != nil || res.Scored != 0 {
		t.Fatalf("no observer ⇒ inert, got %+v %v", res, err)
	}
}
