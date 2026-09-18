package falsify

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
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

// emptyBaseline is an ESTABLISHED commit-time baseline with nothing already firing — the shape of a healthy
// estate at commit. Established (ok=true) is what licenses the forecast verdict; emptiness excludes nothing.
func emptyBaseline(context.Context, time.Time) ([]verify.ObservedAlert, map[string]bool, bool) {
	return nil, nil, true
}

func newScorer(store *MemStore, observed []verify.ObservedAlert) *Scorer {
	return &Scorer{
		Unscored: store, Scores: store, ForecastVerdicts: store, CascadeStats: store,
		Observe:  func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return observed, true },
		Baseline: emptyBaseline,
		// No Latency seam wired ⇒ every edge falls back to the WindowFloor (REQ-110's fail-safe direction), so
		// these oracles exercise the un-learned floor behavior. The learned-window oracles wire it explicitly.
		WindowFloor: DefaultWindowFloor,
		Now:         func() time.Time { return fixedNow },
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
	store.Seed(samplePrediction("plan-5", "act-5"), fixedNow.Add(-time.Minute)) // 1m old, floor is 900s
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

// ---- Phase C4 adjudication-repair oracles ----
// Every test below drives the REAL scoring path: the production Scorer over its designated oracle twin
// (MemStore), the production verdict author (verify.ComputeVerdictDetailScoped), the production confusion
// matrix (predict.ScoreControl), and — where sites matter — a live estate.Graph.SiteOf. No stub re-implements
// any of those.

// (i) A pre-existing alert — one already firing at CommittedAt, per the commit-time baseline — is NOT this
// prediction's failed cascade: the forecast verdict is match, not deviation. The confusion matrix, by
// contrast, still sees it (symmetrically for real and control) — the falsifiability measurement is deliberately
// NOT baselined.
func TestScoreDuePreexistingBaselineAlertIsNotADeviation(t *testing.T) {
	store := NewMemStore()
	store.Seed(samplePrediction("plan-c4-base", "act-c4-base"), fixedNow.Add(-time.Hour))
	ambient := []verify.ObservedAlert{{Host: "unrelated07", Rule: "DiskFull", Site: "nl"}} // firing since BEFORE commit
	sc := newScorer(store, ambient)
	sc.Baseline = func(_ context.Context, asOf time.Time) ([]verify.ObservedAlert, map[string]bool, bool) {
		if !asOf.Equal(fixedNow.Add(-time.Hour)) {
			t.Errorf("baseline must be anchored at CommittedAt, got %v", asOf)
		}
		return ambient, map[string]bool{"unrelated07": true}, true
	}
	res, err := sc.ScoreDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Scored != 1 || res.Deviations != 0 {
		t.Fatalf("expected 1 scored with 0 deviations (ambient alert is baselined out), got %+v", res)
	}
	if v, ok := store.VerdictOf("act-c4-base"); !ok || v != safety.VerdictMatch {
		t.Fatalf("forecast verdict = %q ok=%v, want match — the alert predates the prediction", v, ok)
	}
	// The falsifiability writeback still recorded the ambient host as a missed-cascade FN: the confusion
	// matrix is a symmetric graph measurement and the baseline must not launder it.
	if sc2, _ := store.ScoreOf("plan-c4-base"); sc2.FN != 1 {
		t.Fatalf("ScoreControl must stay un-baselined (FN=1 for the ambient host), got %+v", sc2)
	}
}

// (ii) The sink split: an EXECUTED prediction gets its falsifiability writeback but NO verdict from this
// scorer — its adjudication belongs to the interceptor lane — while the unexecuted twin in the same pass gets
// both. This is the category-error fix: ambient reality diffed against an unexecuted action's forecast used
// to write deviation rows downstream gates read.
func TestScoreDueExecutedPredictionGetsNoForecastVerdict(t *testing.T) {
	store := NewMemStore()
	store.SeedExecuted(samplePrediction("plan-c4-exec", "act-c4-exec"), fixedNow.Add(-2*time.Hour))
	store.Seed(samplePrediction("plan-c4-prop", "act-c4-prop"), fixedNow.Add(-time.Hour))
	observed := []verify.ObservedAlert{{Host: "web09", Rule: "HostDown", Site: "nl"}} // a surprise for both
	res, err := newScorer(store, observed).ScoreDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Scored != 2 || res.Executed != 1 {
		t.Fatalf("expected 2 scored of which 1 executed, got %+v", res)
	}
	if _, ok := store.ScoreOf("plan-c4-exec"); !ok {
		t.Fatal("the executed prediction must still get its falsifiability score (the graph is measurable on every prediction)")
	}
	if v, ok := store.VerdictOf("act-c4-exec"); ok {
		t.Fatalf("the scorer authored a verdict %q for an EXECUTED action — that adjudication belongs to the "+
			"interceptor lane; a forecast grade here can reach op-class graduation/demotion", v)
	}
	if v, ok := store.VerdictOf("act-c4-prop"); !ok || v != safety.VerdictDeviation {
		t.Fatalf("the never-executed twin must still get its forecast verdict (deviation), got %q ok=%v", v, ok)
	}
	if res.Deviations != 1 {
		t.Fatalf("Deviations counts FORECAST deviations only, got %+v", res)
	}
}

// (ii-b) No established baseline ⇒ no authored forecast verdict. An unwired baseline seam still measures
// (score lands); a wired-but-unreadable one SKIPS (retryable) so the verdict can still be authored later.
func TestScoreDueWithholdsForecastVerdictOutsideAnEstablishedBaseline(t *testing.T) {
	// Unwired seam: score written, verdict withheld.
	store := NewMemStore()
	store.Seed(samplePrediction("plan-c4-nobase", "act-c4-nobase"), fixedNow.Add(-time.Hour))
	sc := newScorer(store, []verify.ObservedAlert{{Host: "web09", Rule: "HostDown", Site: "nl"}})
	sc.Baseline = nil
	res, err := sc.ScoreDue(context.Background())
	if err != nil || res.Scored != 1 {
		t.Fatalf("unwired baseline must still score: %+v %v", res, err)
	}
	if _, ok := store.ScoreOf("plan-c4-nobase"); !ok {
		t.Fatal("score must land without a baseline seam (measurement is noise-symmetric)")
	}
	if v, ok := store.VerdictOf("act-c4-nobase"); ok {
		t.Fatalf("a forecast verdict %q was authored with NO baseline — that is the manufactured-deviation class", v)
	}
	// Wired but unreadable: the prediction is SKIPPED entirely (still unscored ⇒ retried later).
	store2 := NewMemStore()
	store2.Seed(samplePrediction("plan-c4-badbase", "act-c4-badbase"), fixedNow.Add(-time.Hour))
	sc2 := newScorer(store2, nil)
	sc2.Baseline = func(context.Context, time.Time) ([]verify.ObservedAlert, map[string]bool, bool) {
		return nil, nil, false
	}
	res2, err := sc2.ScoreDue(context.Background())
	if err != nil || res2.Scored != 0 || res2.Skipped != 1 {
		t.Fatalf("an unreadable baseline must skip (retryable), got %+v %v", res2, err)
	}
	if _, ok := store2.ScoreOf("plan-c4-badbase"); ok {
		t.Fatal("a skipped prediction must remain unscored so a later pass can author its verdict WITH a baseline")
	}
}

// (iii) Cross-site scoping through the REAL estate authority: an alert on a host whose estate-derived site
// differs from the target's is excluded from the forecast deviation evidence, while an unknown-site host in
// the same observation still deviates (fail closed).
func TestScoreDueCrossSiteScopingUsesTheEstateAuthority(t *testing.T) {
	g := estate.NewGraph()
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: estate.TypeNetworkDevice, Name: "dc1fw01"},
		To:   estate.Entity{Type: estate.TypeSite, Name: "dc1"},
		Rel:  estate.RelMemberOf, Source: estate.SourceDeclared, Confidence: 0.85,
	})
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: estate.TypeNetworkDevice, Name: "dc2fw01"},
		To:   estate.Entity{Type: estate.TypeSite, Name: "dc2"},
		Rel:  estate.RelMemberOf, Source: estate.SourceDeclared, Confidence: 0.85,
	})
	rec := predict.PredictionRecord{
		Prediction: verify.Prediction{
			ActionID: "act-c4-site", PlanHash: "plan-c4-site", TargetHost: "dc1mealie01", Site: "nl",
			PredictedHosts: map[string]struct{}{"dc1pve01": {}},
			PredictedRules: map[string]struct{}{verify.RuleKey("dc1pve01", "HostDown"): {}},
		},
		ControlHosts:   map[string]struct{}{"web09": {}},
		SchemaVersion:  schema.Version(1),
		PredictionHash: "hash-c4-site",
	}
	// First pass: only the OTHER-site flap fires — with both sites estate-known and different, it is excluded
	// and the forecast verdict is match.
	store := NewMemStore()
	store.Seed(rec, fixedNow.Add(-time.Hour))
	sc := newScorer(store, []verify.ObservedAlert{{Host: "dc2lte01", Rule: "Sensor under limit", Site: "gr"}})
	sc.HostSite = g.SiteOf
	if res, err := sc.ScoreDue(context.Background()); err != nil || res.Deviations != 0 {
		t.Fatalf("a proven-other-site flap must not deviate a forecast: %+v %v", res, err)
	}
	if v, _ := store.VerdictOf("act-c4-site"); v != safety.VerdictMatch {
		t.Fatalf("forecast verdict = %q, want match (the 59-second other-site sensor flap, scoped out)", v)
	}
	// Second pass, fresh prediction: an unknown-site host still deviates — scoping never widens to what the
	// estate cannot prove.
	rec2 := rec
	rec2.Prediction.ActionID, rec2.Prediction.PlanHash = "act-c4-site2", "plan-c4-site2"
	store2 := NewMemStore()
	store2.Seed(rec2, fixedNow.Add(-time.Hour))
	sc2 := newScorer(store2, []verify.ObservedAlert{{Host: "notrf01vps01", Rule: "HostDown", Site: "gr"}})
	sc2.HostSite = g.SiteOf
	if res, err := sc2.ScoreDue(context.Background()); err != nil || res.Deviations != 1 {
		t.Fatalf("an unknown-site surprise must still deviate (fail closed): %+v %v", res, err)
	}
	if v, _ := store2.VerdictOf("act-c4-site2"); v != safety.VerdictDeviation {
		t.Fatalf("forecast verdict = %q, want deviation for the unknown-site host", v)
	}
}

// (iv) Rule-family matching through the scorer's real path: the predicted host failing under a family SIBLING
// spelling (production rulefamily.json: Devices-up/down ~ HostDown) is the prediction HOLDING — match, and a
// real TP in the confusion matrix (host-level), not a deviation.
func TestScoreDueFamilySiblingRuleScoresMatch(t *testing.T) {
	store := NewMemStore()
	store.Seed(samplePrediction("plan-c4-fam", "act-c4-fam"), fixedNow.Add(-time.Hour))
	observed := []verify.ObservedAlert{{Host: "n8n01", Rule: "Devices-up/down", Site: "nl"}} // predicted host, sibling spelling
	res, err := newScorer(store, observed).ScoreDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Deviations != 0 {
		t.Fatalf("a family-sibling rule on a predicted host must not deviate, got %+v", res)
	}
	if v, _ := store.VerdictOf("act-c4-fam"); v != safety.VerdictMatch {
		t.Fatalf("forecast verdict = %q, want match — the predicted failure mode fired under another source's spelling", v)
	}
	if sc, _ := store.ScoreOf("plan-c4-fam"); sc.TP != 1 {
		t.Fatalf("the alerting predicted host is a host-level TP regardless of rule spelling, got %+v", sc)
	}
}

// (v) THE TRIPWIRE HOLDS: with baselines AND site scoping fully wired, the INV-22 shuffled-graph control is
// untouched — a pass whose only hit belongs to the CONTROL still records a NON-falsifiable window, and the
// baseline does not subtract the control's hit. If a future "scoping" change makes this window falsifiable,
// it scoped too far: it is laundering ambient noise out of the control instead of out of the verdict.
func TestScoreDueScopingNeverLaundersTheShuffledControl(t *testing.T) {
	store := NewMemStore()
	store.Seed(samplePrediction("plan-c4-ctrl", "act-c4-ctrl"), fixedNow.Add(-time.Hour))
	ambient := []verify.ObservedAlert{{Host: "web09", Rule: "HostDown", Site: "nl"}} // web09 IS the control host
	sc := newScorer(store, ambient)
	// The ambient alert predates the commit: the VERDICT must clean it out (match)...
	sc.Baseline = func(context.Context, time.Time) ([]verify.ObservedAlert, map[string]bool, bool) {
		return ambient, map[string]bool{"web09": true}, true
	}
	res, err := sc.ScoreDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := store.VerdictOf("act-c4-ctrl"); v != safety.VerdictMatch {
		t.Fatalf("forecast verdict = %q, want match (the alert predates the prediction)", v)
	}
	// ...but the CONTROL side of the confusion matrix must still see it: control_tp=1 vs real_tp=0 keeps the
	// window NON-falsifiable. That asymmetry-of-effect (verdict cleaned, control untouched) is the whole test.
	if scr, _ := store.ScoreOf("plan-c4-ctrl"); scr.ControlTP != 1 {
		t.Fatalf("the shuffled control's hit must survive every scoping filter, got %+v", scr)
	}
	if w := store.Windows(); len(w) != 1 || w[0].Falsifiable {
		t.Fatalf("a window the control won must stay NON-falsifiable under full scoping, got %+v", w)
	}
	if res.SumRealTP != 0 || res.SumControlTP != 1 {
		t.Fatalf("expected real_tp=0 control_tp=1, got %+v", res)
	}
}
