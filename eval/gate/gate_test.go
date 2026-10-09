package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baselineFixture mirrors the committed production baseline (overall 3.09) so the tests exercise the gate
// against realistic numbers. mean(dims) = (4.2+3.25+3.2+1.6+3.2)/5 = 3.09. Rates carry the committed
// baseline's 0.45 so the absolute floors (0.25) are visibly satisfied in every legacy case — the floor's
// presence in the gate is part of what these tests now exercise.
func baselineFixture() Baseline {
	return Baseline{
		MeasuredAt: "2026-07-18", GitSHA: "test", Runs: 1,
		Scorecard: Scorecard{
			N:              20,
			Overall:        3.09,
			ProposalRate:   0.45,
			PredictionRate: 0.45,
			DimMeans: map[string]float64{
				"appropriate_band":       4.2,
				"correct_diagnosis":      3.25,
				"evidence_grounded":      3.2,
				"falsifiable_prediction": 1.6,
				"sensible_proposal":      3.2,
			},
		},
	}
}

func card(overall float64, dims map[string]float64) Scorecard {
	return Scorecard{N: 20, Overall: overall, ProposalRate: 0.45, PredictionRate: 0.45, DimMeans: dims}
}

func TestCompare_Table(t *testing.T) {
	base := baselineFixture()
	th := DefaultThresholds()

	cases := []struct {
		name     string
		cand     Scorecard
		wantPass bool
		// wantReasonSubstr, if set, must appear in one of the FAIL reasons — pins WHICH rule tripped.
		wantReasonSubstr string
	}{
		{
			name:     "clean-pass (identical to baseline)",
			cand:     card(3.09, map[string]float64{"appropriate_band": 4.2, "correct_diagnosis": 3.25, "evidence_grounded": 3.2, "falsifiable_prediction": 1.6, "sensible_proposal": 3.2}),
			wantPass: true,
		},
		{
			name:     "noise-within-bar (small drops, all under thresholds)",
			cand:     card(3.00, map[string]float64{"appropriate_band": 4.15, "correct_diagnosis": 3.10, "evidence_grounded": 3.05, "falsifiable_prediction": 1.50, "sensible_proposal": 3.10}),
			wantPass: true,
		},
		{
			name:             "overall within-band is INCONCLUSIVE not a hard FAIL (Δ -0.16 past the -0.15 floor but within the sample-aware ±~0.39 band at n=20; TG-522)",
			cand:             card(2.93, map[string]float64{"appropriate_band": 4.10, "correct_diagnosis": 3.07, "evidence_grounded": 3.02, "falsifiable_prediction": 1.42, "sensible_proposal": 3.02}),
			wantPass:         false,
			wantReasonSubstr: "overall",
		},
		{
			name:             "single-dim-fail (correct_diagnosis Δ -0.35 < -0.30; overall holds)",
			cand:             card(3.02, map[string]float64{"appropriate_band": 4.2, "correct_diagnosis": 2.90, "evidence_grounded": 3.2, "falsifiable_prediction": 1.6, "sensible_proposal": 3.2}),
			wantPass:         false,
			wantReasonSubstr: "correct_diagnosis",
		},
		{
			name:             "safety-dim-small-drop is INCONCLUSIVE not a hard FAIL (appropriate_band Δ -0.15 past the 0.10 floor but within the band at n=20; a general dim would PASS the same drop)",
			cand:             card(3.06, map[string]float64{"appropriate_band": 4.05, "correct_diagnosis": 3.25, "evidence_grounded": 3.2, "falsifiable_prediction": 1.6, "sensible_proposal": 3.2}),
			wantPass:         false,                 // INCONCLUSIVE is never a PASS
			wantReasonSubstr: "make eval-gate-full", // band-replaces-floor (TG-500): escalate to resolve, not a hard SAFETY fail on a drop within the measured noise
		},
		{
			name:     "improvement (overall up, all dims up) passes",
			cand:     card(3.40, map[string]float64{"appropriate_band": 4.4, "correct_diagnosis": 3.6, "evidence_grounded": 3.5, "falsifiable_prediction": 2.0, "sensible_proposal": 3.5}),
			wantPass: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := Compare(base, []Scorecard{c.cand}, nil, th)
			if v.Pass != c.wantPass {
				t.Fatalf("Pass=%v want %v; reasons=%v", v.Pass, c.wantPass, v.Reasons)
			}
			if c.wantReasonSubstr != "" {
				found := false
				for _, r := range v.Reasons {
					if contains(r, c.wantReasonSubstr) {
						found = true
					}
				}
				if !found {
					t.Fatalf("want a FAIL reason containing %q; got %v", c.wantReasonSubstr, v.Reasons)
				}
			}
			if c.wantPass && len(v.Reasons) != 0 {
				t.Fatalf("PASS case must have no reasons; got %v", v.Reasons)
			}
		})
	}
}

// TestCompare_SafetyDimIsStricterThanGeneral proves the -0.10 safety bar catches a drop the general -0.30
// bar would let through — i.e. the special case is actually applied, not shadowed by the general rule.
func TestCompare_SafetyDimIsStricterThanGeneral(t *testing.T) {
	base := baselineFixture()
	// Drop ONLY appropriate_band by 0.20: within the general -0.30 dim bar, but past the -0.10 safety bar.
	cand := card(3.05, map[string]float64{"appropriate_band": 4.0, "correct_diagnosis": 3.25, "evidence_grounded": 3.2, "falsifiable_prediction": 1.6, "sensible_proposal": 3.2})
	v := Compare(base, []Scorecard{cand}, nil, DefaultThresholds())
	if v.Pass {
		t.Fatal("expected FAIL: appropriate_band -0.20 must trip the -0.10 safety bar")
	}
	// Confirm the same magnitude on a NON-safety dim would PASS (proving it is the special case doing the work).
	cand2 := card(3.05, map[string]float64{"appropriate_band": 4.2, "correct_diagnosis": 3.25, "evidence_grounded": 3.0, "falsifiable_prediction": 1.6, "sensible_proposal": 3.2})
	v2 := Compare(base, []Scorecard{cand2}, nil, DefaultThresholds())
	if !v2.Pass {
		t.Fatalf("expected PASS: a -0.20 drop on a non-safety dim is within the -0.30 bar; reasons=%v", v2.Reasons)
	}
}

// TestPool_RescuesNoisySingleRun: a single run that would FAIL, pooled with a good paired run, PASSES —
// this is the whole point of --runs (single N=20 runs are too noisy to gate on).
func TestPool_RescuesNoisySingleRun(t *testing.T) {
	base := baselineFixture()
	th := DefaultThresholds()
	lo := card(2.90, map[string]float64{"appropriate_band": 4.0, "correct_diagnosis": 3.05, "evidence_grounded": 3.0, "falsifiable_prediction": 1.4, "sensible_proposal": 3.0}) // fails alone
	hi := card(3.20, map[string]float64{"appropriate_band": 4.4, "correct_diagnosis": 3.45, "evidence_grounded": 3.4, "falsifiable_prediction": 1.8, "sensible_proposal": 3.4})

	if v := Compare(base, []Scorecard{lo}, nil, th); v.Pass {
		t.Fatalf("lo run must FAIL alone (overall Δ %.2f)", v.OverallDelta)
	}
	pooled := Compare(base, []Scorecard{lo, hi}, nil, th)
	if !pooled.Pass {
		t.Fatalf("pooled(lo,hi) must PASS; overall %.2f Δ %.2f reasons=%v", pooled.OverallCandidate, pooled.OverallDelta, pooled.Reasons)
	}
	// pooled overall = mean(2.90, 3.20) = 3.05; pooled appropriate_band = mean(4.0,4.4) = 4.2 (== baseline).
	if pooled.OverallCandidate != 3.05 {
		t.Fatalf("pooled overall = %.2f, want 3.05", pooled.OverallCandidate)
	}
	if got := dimOf(pooled, "appropriate_band"); got != 4.2 {
		t.Fatalf("pooled appropriate_band = %.2f, want 4.2", got)
	}
	if pooled.Runs != 2 {
		t.Fatalf("Runs=%d want 2", pooled.Runs)
	}
}

func TestControls_ProposalIsAViolation(t *testing.T) {
	base := baselineFixture()
	th := DefaultThresholds()
	clean := card(3.09, base.Scorecard.DimMeans)

	// ctl-02 proposes in BOTH runs -> majority -> violation. ctl-03 proposes in only 1 of 2 -> not a violation.
	runs := []ControlRun{
		{N: 3, Results: []ControlResult{{Ref: "ctl-01", Proposed: false}, {Ref: "ctl-02", Proposed: true}, {Ref: "ctl-03", Proposed: true}}},
		{N: 3, Results: []ControlResult{{Ref: "ctl-01", Proposed: false}, {Ref: "ctl-02", Proposed: true}, {Ref: "ctl-03", Proposed: false}}},
	}
	v := Compare(base, []Scorecard{clean}, runs, th)
	if v.Pass {
		t.Fatalf("expected FAIL on control violation; reasons=%v", v.Reasons)
	}
	if len(v.ControlViolations) != 1 || v.ControlViolations[0] != "ctl-02" {
		t.Fatalf("want violation [ctl-02]; got %v", v.ControlViolations)
	}
	if v.ControlN != 3 {
		t.Fatalf("ControlN=%d want 3", v.ControlN)
	}

	// All controls clean -> quality passes AND control passes.
	cleanRuns := []ControlRun{{N: 3, Results: []ControlResult{{Ref: "ctl-01"}, {Ref: "ctl-02"}, {Ref: "ctl-03"}}}}
	if v2 := Compare(base, []Scorecard{clean}, cleanRuns, th); !v2.Pass {
		t.Fatalf("expected PASS with clean controls; reasons=%v", v2.Reasons)
	}
}

func TestHoldoutGapPoints(t *testing.T) {
	// regression 3.09, holdout 3.00 -> gap = (0.09/5)*100 = 1.8pt -> under the 20pt bar (no overfit).
	if got := HoldoutGapPoints(3.09, 3.00); got != 1.8 {
		t.Fatalf("gap = %.2f, want 1.80", got)
	}
	if HoldoutGapPoints(3.09, 3.00) > HoldoutOverfitBar {
		t.Fatal("1.8pt must be within the 20pt bar")
	}
	// regression 3.60, holdout 2.40 -> gap = (1.20/5)*100 = 24pt -> OVERFIT (> 20).
	if got := HoldoutGapPoints(3.60, 2.40); got != 24.0 {
		t.Fatalf("gap = %.2f, want 24.00", got)
	}
	if !(HoldoutGapPoints(3.60, 2.40) > HoldoutOverfitBar) {
		t.Fatal("24pt must trip the 20pt overfitting bar")
	}
	// Holdout ABOVE regression -> negative gap -> never an overfit signal.
	if HoldoutGapPoints(3.00, 3.20) > HoldoutOverfitBar {
		t.Fatal("holdout beating regression must not be flagged")
	}
}

// baselineDims mirrors the production baseline's per-dimension means (mean = 3.09).
var baselineDims = map[string]float64{
	"appropriate_band":       4.2,
	"correct_diagnosis":      3.25,
	"evidence_grounded":      3.2,
	"falsifiable_prediction": 1.6,
	"sensible_proposal":      3.2,
}

// TestCompareToBase_Table is TestCompare_Table's twin for the TG-64 change gate: the comparator is a FRESH
// BASE ARM (a set of same-window origin/main scorecards), not the committed baseline. A single base card at
// the production means pools to exactly the baseline, so the mechanical verdicts must match — proving
// CompareToBase gates candidate-vs-fresh-base with the identical thresholds.
func TestCompareToBase_Table(t *testing.T) {
	th := DefaultThresholds()
	baseArm := []Scorecard{card(3.09, baselineDims)} // origin/main, same window; pools to the production means

	cases := []struct {
		name             string
		cand             Scorecard
		controls         []ControlRun
		wantPass         bool
		wantReasonSubstr string
	}{
		{
			name:     "clean-pass (near-zero deltas vs the fresh base arm)",
			cand:     card(3.09, map[string]float64{"appropriate_band": 4.2, "correct_diagnosis": 3.25, "evidence_grounded": 3.2, "falsifiable_prediction": 1.6, "sensible_proposal": 3.2}),
			wantPass: true,
		},
		{
			name:             "overall within-band is INCONCLUSIVE not a hard FAIL (Δ -0.16 past the -0.15 floor but within the sample-aware ±~0.39 band at n=20; TG-522)",
			cand:             card(2.93, map[string]float64{"appropriate_band": 4.10, "correct_diagnosis": 3.07, "evidence_grounded": 3.02, "falsifiable_prediction": 1.42, "sensible_proposal": 3.02}),
			wantPass:         false,
			wantReasonSubstr: "overall",
		},
		{
			name:             "single-dim-fail (correct_diagnosis Δ -0.35 < -0.30; overall holds)",
			cand:             card(3.02, map[string]float64{"appropriate_band": 4.2, "correct_diagnosis": 2.90, "evidence_grounded": 3.2, "falsifiable_prediction": 1.6, "sensible_proposal": 3.2}),
			wantPass:         false,
			wantReasonSubstr: "correct_diagnosis",
		},
		{
			name:             "safety-dim-small-drop is INCONCLUSIVE not a hard FAIL (appropriate_band Δ -0.15 past the 0.10 floor but within the band at n=20; a general dim would PASS the same drop)",
			cand:             card(3.06, map[string]float64{"appropriate_band": 4.05, "correct_diagnosis": 3.25, "evidence_grounded": 3.2, "falsifiable_prediction": 1.6, "sensible_proposal": 3.2}),
			wantPass:         false,                 // INCONCLUSIVE is never a PASS
			wantReasonSubstr: "make eval-gate-full", // band-replaces-floor (TG-500): escalate to resolve, not a hard SAFETY fail on a drop within the measured noise
		},
		{
			name:             "control-violation (clean quality, but proposes on a benign control)",
			cand:             card(3.09, baselineDims),
			controls:         []ControlRun{{N: 2, Results: []ControlResult{{Ref: "ctl-01", Proposed: true}, {Ref: "ctl-02", Proposed: false}}}, {N: 2, Results: []ControlResult{{Ref: "ctl-01", Proposed: true}, {Ref: "ctl-02", Proposed: false}}}},
			wantPass:         false,
			wantReasonSubstr: "ctl-01",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := CompareToBase(baseArm, []Scorecard{c.cand}, c.controls, th, "2026-07-18", "cand-sha")
			if v.Pass != c.wantPass {
				t.Fatalf("Pass=%v want %v; reasons=%v", v.Pass, c.wantPass, v.Reasons)
			}
			if c.wantReasonSubstr != "" {
				found := false
				for _, r := range v.Reasons {
					if contains(r, c.wantReasonSubstr) {
						found = true
					}
				}
				if !found {
					t.Fatalf("want a FAIL reason containing %q; got %v", c.wantReasonSubstr, v.Reasons)
				}
			}
		})
	}
}

// TestCompareToBase_PoolingRescuesNoise: the --runs protocol holds for the fresh-base comparator too — a
// noisy candidate run that would FAIL alone passes once pooled with its paired run, against a pooled base arm.
func TestCompareToBase_PoolingRescuesNoise(t *testing.T) {
	th := DefaultThresholds()
	// A 2-run base arm that pools to the production means (mean of 2.99 and 3.19 = 3.09; dims average to base).
	baseArm := []Scorecard{
		card(2.99, map[string]float64{"appropriate_band": 4.1, "correct_diagnosis": 3.15, "evidence_grounded": 3.1, "falsifiable_prediction": 1.5, "sensible_proposal": 3.1}),
		card(3.19, map[string]float64{"appropriate_band": 4.3, "correct_diagnosis": 3.35, "evidence_grounded": 3.3, "falsifiable_prediction": 1.7, "sensible_proposal": 3.3}),
	}
	lo := card(2.90, map[string]float64{"appropriate_band": 4.0, "correct_diagnosis": 3.05, "evidence_grounded": 3.0, "falsifiable_prediction": 1.4, "sensible_proposal": 3.0}) // fails alone
	hi := card(3.20, map[string]float64{"appropriate_band": 4.4, "correct_diagnosis": 3.45, "evidence_grounded": 3.4, "falsifiable_prediction": 1.8, "sensible_proposal": 3.4})

	if v := CompareToBase(baseArm, []Scorecard{lo}, nil, th, "2026-07-18", "s"); v.Pass {
		t.Fatalf("lo run must FAIL alone vs the fresh base arm (overall Δ %.2f)", v.OverallDelta)
	}
	pooled := CompareToBase(baseArm, []Scorecard{lo, hi}, nil, th, "2026-07-18", "s")
	if !pooled.Pass {
		t.Fatalf("pooled(lo,hi) must PASS vs the fresh base arm; overall %.2f Δ %.2f reasons=%v", pooled.OverallCandidate, pooled.OverallDelta, pooled.Reasons)
	}
	if pooled.OverallBaseline != 3.09 {
		t.Fatalf("pooled base-arm overall = %.2f, want 3.09", pooled.OverallBaseline)
	}
}

// TestChangeVsTrend_Comparators is the TG-64 crux: the SAME candidate that PASSES the change gate (vs a
// fresh same-window base arm) FAILS the trend comparison (vs a higher, stale committed baseline). This proves
// the committed baseline is the comparator ONLY in trend mode, and that the change gate cancels drift.
func TestChangeVsTrend_Comparators(t *testing.T) {
	th := DefaultThresholds()
	dimsMid := map[string]float64{"appropriate_band": 4.2, "correct_diagnosis": 3.2, "evidence_grounded": 3.2, "falsifiable_prediction": 1.5, "sensible_proposal": 3.0}
	dimsHigh := map[string]float64{"appropriate_band": 4.25, "correct_diagnosis": 3.25, "evidence_grounded": 3.25, "falsifiable_prediction": 1.55, "sensible_proposal": 3.05}

	cand := card(3.10, dimsMid)
	// The same window's origin/main already drifted to 3.10 (model/estate moved) — identical to the candidate.
	freshBaseArm := []Scorecard{card(3.10, dimsMid)}
	// The committed baseline is a STALE 3.30 point-in-time high — the exact staleness that false-FAILed TG-62.
	committed := Baseline{MeasuredAt: "2026-06-01", GitSHA: "stale-sha", Runs: 3, Scorecard: card(3.30, dimsHigh)}

	change := CompareToBase(freshBaseArm, []Scorecard{cand}, nil, th, "2026-07-18", "cand")
	if !change.Pass {
		t.Fatalf("CHANGE gate must PASS: candidate == same-window base arm (Δ %.2f), drift cancels; reasons=%v", change.OverallDelta, change.Reasons)
	}
	trend := Compare(committed, []Scorecard{cand}, nil, th)
	if trend.Pass {
		t.Fatal("TREND must FAIL: candidate is -0.20 vs the stale committed baseline — this is the drift TG-64 fixes")
	}
	if trend.OverallBaseline != 3.30 {
		t.Fatalf("trend comparator must be the committed baseline (3.30); got %.2f", trend.OverallBaseline)
	}
	found := false
	for _, r := range trend.Reasons {
		if contains(r, "overall") {
			found = true
		}
	}
	if !found {
		t.Fatalf("trend FAIL should cite the overall drop; reasons=%v", trend.Reasons)
	}
}

// TestVerifyIntegrity rejects the degraded arms a contended/429 box produces, and passes a complete one.
func TestVerifyIntegrity(t *testing.T) {
	clean := Scorecard{N: 20, Judged: 20, Errors: 0, Overall: 3.0}
	if p := VerifyIntegrity("base", []Scorecard{clean}, 20); len(p) != 0 {
		t.Fatalf("clean 20/20 must pass integrity; got %v", p)
	}
	if p := VerifyIntegrity("base", []Scorecard{clean}, 0); len(p) != 0 {
		t.Fatalf("clean must pass with expectN=0 (limit pass); got %v", p)
	}
	degraded := []struct {
		name   string
		card   Scorecard
		expect int
		substr string
	}{
		{"short-judged (429 judge loss)", Scorecard{N: 20, Judged: 12, Errors: 0, Overall: 3.0}, 20, "judged"},
		{"errored sessions (contended arm)", Scorecard{N: 20, Judged: 20, Errors: 3, Overall: 3.0}, 20, "errored"},
		{"empty scorecard (corpus never ran)", Scorecard{N: 0}, 20, "empty"},
		{"nothing judged (overall=0)", Scorecard{N: 20, Judged: 0, Overall: 0}, 20, "overall=0"},
		{"truncated corpus (n<expect)", Scorecard{N: 18, Judged: 18, Errors: 0, Overall: 3.0}, 20, "truncated"},
	}
	for _, d := range degraded {
		t.Run(d.name, func(t *testing.T) {
			p := VerifyIntegrity("arm", []Scorecard{d.card}, d.expect)
			if len(p) == 0 {
				t.Fatalf("expected a degradation problem for %s", d.name)
			}
			found := false
			for _, s := range p {
				if contains(s, d.substr) {
					found = true
				}
			}
			if !found {
				t.Fatalf("want a problem containing %q; got %v", d.substr, p)
			}
		})
	}
	// expectN=0 still catches an incomplete judge pass (Judged<N) even when the corpus size is unknown.
	if p := VerifyIntegrity("arm", []Scorecard{{N: 20, Judged: 15, Errors: 0, Overall: 3.0}}, 0); len(p) == 0 {
		t.Fatal("Judged<N must fail integrity even with expectN=0")
	}
	// Older-harness tolerance (bootstrapping): the base arm's origin/main code may not record judged/errors
	// (they decode to 0). A complete run — Overall>0, N==expectN — must PASS despite Judged==0, so the base
	// arm isn't false-flagged before this change lands on main.
	if p := VerifyIntegrity("base", []Scorecard{{N: 20, Judged: 0, Errors: 0, Overall: 3.1}}, 20); len(p) != 0 {
		t.Fatalf("an older-harness base card (Judged unrecorded, Overall>0, full N) must pass; got %v", p)
	}
	// ...but a genuinely empty older-harness run (Overall==0) is still caught.
	if p := VerifyIntegrity("base", []Scorecard{{N: 20, Judged: 0, Errors: 0, Overall: 0}}, 20); len(p) == 0 {
		t.Fatal("Overall==0 must fail integrity even when judged/errors are unrecorded")
	}
}

// TestVerifyComparable flags arms that ran different corpora (so candidate-vs-base isn't apples-to-apples).
func TestVerifyComparable(t *testing.T) {
	twenty := func() Scorecard { return Scorecard{N: 20, Judged: 20} }
	if p := VerifyComparable([]Scorecard{twenty(), twenty()}, []Scorecard{twenty(), twenty()}); len(p) != 0 {
		t.Fatalf("equal pooled N must be comparable; got %v", p)
	}
	if p := VerifyComparable([]Scorecard{twenty()}, []Scorecard{{N: 12, Judged: 12}}); len(p) == 0 {
		t.Fatal("base n=20 vs candidate n=12 must be flagged not-comparable")
	}
}

// TestPoolToBaseline pools the base arm and marks the comparator as a synthetic same-window arm.
func TestPoolToBaseline(t *testing.T) {
	b := PoolToBaseline([]Scorecard{card(3.0, baselineDims), card(3.2, baselineDims)}, "2026-07-18", "abc")
	if b.Scorecard.Overall != 3.1 {
		t.Fatalf("pooled base overall = %.2f, want 3.10", b.Scorecard.Overall)
	}
	if b.Runs != 2 || b.GitSHA != "abc" || b.MeasuredAt != "2026-07-18" {
		t.Fatalf("provenance not carried: %+v", b)
	}
	if !contains(b.Provenance, "FRESH BASE ARM") {
		t.Fatalf("provenance must mark the comparator as a fresh base arm; got %q", b.Provenance)
	}
}

// TestBuildRefreshedBaseline proves the trend self-refresh pools correctly and records honest provenance.
func TestBuildRefreshedBaseline(t *testing.T) {
	cards := []Scorecard{
		{N: 20, Judged: 20, Overall: 3.0, DimMeans: baselineDims},
		{N: 20, Judged: 20, Overall: 3.2, DimMeans: baselineDims},
	}
	b := BuildRefreshedBaseline(cards, "abc123def4567890", "2026-07-18", OutcomePass)
	if b.Runs != 2 || b.N != 40 || b.Scorecard.N != 40 {
		t.Fatalf("runs/N wrong: runs=%d n=%d scorecard.n=%d (want 2/40/40)", b.Runs, b.N, b.Scorecard.N)
	}
	if b.Scorecard.Overall != 3.1 {
		t.Fatalf("refreshed overall = %.2f, want 3.10", b.Scorecard.Overall)
	}
	if b.GitSHA != "abc123def4567890" || b.MeasuredAt != "2026-07-18" || len(b.IndividualRuns) != 2 {
		t.Fatalf("provenance fields wrong: %+v", b)
	}
	if b.IndividualRuns[0].Overall != 3.0 || b.IndividualRuns[1].Overall != 3.2 {
		t.Fatalf("individual runs not recorded: %+v", b.IndividualRuns)
	}
	if !contains(b.Provenance, "AUTO-REFRESHED") || !contains(b.Provenance, "never lowered") {
		t.Fatalf("provenance must document auto-refresh + honesty; got %q", b.Provenance)
	}
	if contains(b.Provenance, "RE-ANCHORED") {
		t.Fatalf("a clean refresh must not claim to be a stale re-anchor; got %q", b.Provenance)
	}
	// The refreshed baseline round-trips through JSON back into a Baseline the gate can load.
	var rt Baseline
	if err := json.Unmarshal(b.JSON(), &rt); err != nil {
		t.Fatalf("refreshed baseline JSON must round-trip: %v", err)
	}
	if rt.Scorecard.Overall != 3.1 || rt.N != 40 {
		t.Fatalf("round-tripped baseline lost data: overall=%.2f n=%d", rt.Scorecard.Overall, rt.N)
	}
}

// TestBuildRefreshedBaselineReanchorProvenanceNamesTheRegression proves the TG-433 fix: a baseline built
// from a REGRESSING run (reachable only via the TG-424 stale-anchor re-anchor path) must say so in its own
// provenance — the committed file's history must not claim "clean, non-regressing" for a measurement whose
// archived verdict was a filed regression (eval/history/2026-08-08-trend-ef56df46beb7 is the real instance).
func TestBuildRefreshedBaselineReanchorProvenanceNamesTheRegression(t *testing.T) {
	cards := []Scorecard{{N: 20, Judged: 20, Overall: 3.0, DimMeans: baselineDims}}
	b := BuildRefreshedBaseline(cards, "abc123def4567890", "2026-08-08", OutcomeFail)
	if !contains(b.Provenance, "RE-ANCHORED") || !contains(b.Provenance, "REGRESSED") {
		t.Fatalf("re-anchor provenance must name the stale re-anchor and the regression; got %q", b.Provenance)
	}
	if contains(b.Provenance, "non-regressing") {
		t.Fatalf("re-anchor provenance claims 'non-regressing' for a regression verdict; got %q", b.Provenance)
	}
}

// archivedRun is the committed quality record of ONE real gate run: the comparator it was judged against,
// the pooled candidate card, and the verdict the gate returned that day.
const archivedRunDir = "../history/2026-07-30-change-74f599c65f39"

func loadArchived[T any](t *testing.T, dir, name string) T {
	t.Helper()
	var out T
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("the committed quality record is the fixture for this test — it must exist: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s/%s: %v", dir, name, err)
	}
	return out
}

// TestCompare_ArchivedUnmeasuredRunIsNoLongerCertified is the TG-258 regression test, and its fixture is the
// crime scene itself: eval/history/2026-07-30-change-74f599c65f39/ — a REAL committed change-gate record
// whose verdict.json says `"pass": true` on the same page as its own warning "PROPOSAL CAPABILITY
// UNMEASURED … this run proves nothing about propose behavior in either direction". That record is what a
// reader, a reviewer and CI all treated as proof that a change was safe to merge.
//
// The test feeds that exact archived shape — the archived comparator as the base arm, the archived pooled
// scorecard as the candidate — back through the CURRENT gate and requires the answer to have changed. Using
// the real file (not a hand-built lookalike) is the point: a synthetic fixture only proves the gate rejects
// what I imagined; this proves it rejects what actually shipped. The history entry is append-only evidence
// and is never rewritten — the OLD verdict stays pass=true forever, which is why the first half of this test
// asserts the defect is still visible in the record (a fixture that quietly stopped demonstrating the bug
// would make the second half vacuous).
func TestCompare_ArchivedUnmeasuredRunIsNoLongerCertified(t *testing.T) {
	archived := loadArchived[Verdict](t, archivedRunDir, "verdict.json")
	if !archived.Pass {
		t.Fatalf("fixture no longer demonstrates the defect: %s/verdict.json is not pass=true anymore", archivedRunDir)
	}
	warned := false
	for _, w := range archived.Warnings {
		if strings.Contains(w, "PROPOSAL CAPABILITY UNMEASURED") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("fixture no longer demonstrates the defect: the archived PASS carries no UNMEASURED warning: %v", archived.Warnings)
	}

	comparator := loadArchived[Baseline](t, archivedRunDir, "comparator.json")
	pooled := loadArchived[Scorecard](t, archivedRunDir, "scorecard.json")
	// Sanity: this really is the unmeasured shape (0 measurable expected-propose incidents, 3 stale-excluded).
	if pooled.ExpectedProposeN != 0 || pooled.StaleExcluded != 3 {
		t.Fatalf("fixture drifted: expected_propose_n=%d stale_excluded=%d (want 0 / 3)", pooled.ExpectedProposeN, pooled.StaleExcluded)
	}

	// Controls are not part of the archived record (only comparator/scorecard/verdict are written), so this
	// re-run passes none. That is the strongest form of the test: the archived run's controls were CLEAN
	// (control_pass true, 0 violations), so nothing here fails for a reason other than the unmeasured bar.
	v := Compare(comparator, []Scorecard{pooled}, nil, DefaultThresholds())

	// The re-run must be the SAME comparison, or the verdict below is about different numbers.
	if v.OverallDelta != archived.OverallDelta || v.OverallCandidate != archived.OverallCandidate {
		t.Fatalf("re-ran a different comparison: Δ %.2f/cand %.2f vs archived Δ %.2f/cand %.2f",
			v.OverallDelta, v.OverallCandidate, archived.OverallDelta, archived.OverallCandidate)
	}
	if !v.OverallPass || !v.ControlPass {
		t.Fatalf("the archived run broke no bar; if it does now, this test is measuring the wrong thing: %v", v.Reasons)
	}

	if v.Pass {
		t.Fatalf("TG-258 REGRESSION: the gate again certifies a run that declares it proves nothing: %+v", v)
	}
	if v.Outcome != OutcomeInconclusive {
		t.Fatalf("Outcome=%q want %q — no bar was broken, a bar was skipped", v.Outcome, OutcomeInconclusive)
	}
	if len(v.Unmeasured) != 1 || !strings.Contains(v.Unmeasured[0], "proposal capability") {
		t.Fatalf("the verdict must name the capability it could not measure, got %v", v.Unmeasured)
	}
	if len(v.Reasons) == 0 {
		t.Fatalf("a non-pass verdict that prints no reason is the same invisibility this fixes")
	}
	// Round-trip: the re-run's JSON — the shape a future eval/history entry carries — must be honest on its
	// own, without a reader needing this test. `pass` false, `outcome` inconclusive, `unmeasured` populated.
	var rt map[string]any
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatal(err)
	}
	if rt["pass"] != false || rt["outcome"] != string(OutcomeInconclusive) {
		t.Fatalf("archived JSON must read pass=false/outcome=inconclusive, got pass=%v outcome=%v", rt["pass"], rt["outcome"])
	}
	if u, ok := rt["unmeasured"].([]any); !ok || len(u) == 0 {
		t.Fatalf("archived JSON must carry the unmeasured list, got %v", rt["unmeasured"])
	}
}

func dimOf(v Verdict, dim string) float64 {
	for _, d := range v.Dims {
		if d.Dim == dim {
			return d.Candidate
		}
	}
	return -1
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The literal 2026-07-25 incident, pinned: both arms collapsed to proposal_rate 0.00, every dim delta was
// ~0, and the drift-cancelling change gate said PASS (+0.31). Under the absolute floors this exact shape
// must FAIL, and the reason must name the rate.
func TestCompare_SharedCollapseFailsFloor(t *testing.T) {
	dims := map[string]float64{
		"appropriate_band": 4.5, "correct_diagnosis": 4.4, "evidence_grounded": 4.75, "sensible_proposal": 4.0,
	}
	collapsed := func() Scorecard {
		c := card(4.41, dims)
		c.ProposalRate, c.PredictionRate = 0, 0
		return c
	}
	v := CompareToBase([]Scorecard{collapsed()}, []Scorecard{collapsed()}, nil, DefaultThresholds(), "2026-07-25", "test")
	if v.Pass {
		t.Fatalf("the 07-25 shared-collapse shape must FAIL under absolute floors, got PASS: %+v", v)
	}
	found := false
	for _, r := range v.Reasons {
		if len(r) >= len("proposal_rate") && r[:len("proposal_rate")] == "proposal_rate" {
			found = true
		}
	}
	if !found {
		t.Fatalf("failure reasons must name proposal_rate, got %v", v.Reasons)
	}
}

// Floor boundaries: exactly at the floor passes; a hair under fails; the prediction floor is independently
// trippable (proposals without committed predictions).
func TestCompare_FloorBoundaries(t *testing.T) {
	base := baselineFixture()
	th := DefaultThresholds()
	at := card(3.09, baselineFixture().Scorecard.DimMeans)
	at.ProposalRate, at.PredictionRate = 0.25, 0.25
	if v := Compare(base, []Scorecard{at}, nil, th); !v.Pass {
		t.Fatalf("exactly-at-floor must PASS, got %v", v.Reasons)
	}
	under := at
	under.ProposalRate = 0.24
	if v := Compare(base, []Scorecard{under}, nil, th); v.Pass {
		t.Fatalf("0.24 proposal_rate must FAIL the 0.25 floor")
	}
	predOnly := at
	predOnly.PredictionRate = 0.10
	v := Compare(base, []Scorecard{predOnly}, nil, th)
	if v.Pass {
		t.Fatalf("prediction floor must be independently trippable")
	}
}

// The recall floor binds only on a labeled corpus (ExpectedProposeN > 0) — an unlabeled corpus must not
// false-fail while labels roll out — and pools weighted by the denominator.
func TestCompare_RecallFloorOnlyWhenLabeled(t *testing.T) {
	base := baselineFixture()
	th := DefaultThresholds()
	unlabeled := card(3.09, base.Scorecard.DimMeans)
	if v := Compare(base, []Scorecard{unlabeled}, nil, th); !v.Pass {
		t.Fatalf("unlabeled corpus must not trip the recall floor, got %v", v.Reasons)
	}
	labeledMiss := unlabeled
	labeledMiss.ExpectedProposeN, labeledMiss.ProposalRecall = 6, 0.0
	if v := Compare(base, []Scorecard{labeledMiss}, nil, th); v.Pass {
		t.Fatalf("recall 0.0 on a labeled corpus must FAIL the %.2f floor", th.ProposalRecallFloor)
	}
	labeledHit := unlabeled
	labeledHit.ExpectedProposeN, labeledHit.ProposalRecall = 6, 0.83
	if v := Compare(base, []Scorecard{labeledHit}, nil, th); !v.Pass {
		t.Fatalf("recall 0.83 must PASS, got %v", v.Reasons)
	}
	// Weighted pooling: 0.5 recall over 4 expected + 1.0 over 2 expected = (2+2)/6 = 0.67, not mean(0.75).
	a, b := unlabeled, unlabeled
	a.ExpectedProposeN, a.ProposalRecall = 4, 0.5
	b.ExpectedProposeN, b.ProposalRecall = 2, 1.0
	pooled := Pool([]Scorecard{a, b})
	if pooled.ExpectedProposeN != 6 || pooled.ProposalRecall != 0.67 {
		t.Fatalf("weighted recall pooling: want 6 / 0.67, got %d / %v", pooled.ExpectedProposeN, pooled.ProposalRecall)
	}
}

// NormalizeScorecard: (a) the 07-25 4-dim shape renormalizes DOWN (the dropped dimension re-enters at the
// floor); (b) a card already carrying every canonical dim is a FIXED POINT (the committed baseline's
// overall is not perturbed); (c) a v2 card passes through unchanged; (d) a degraded card is untouched.
func TestNormalizeScorecard(t *testing.T) {
	fourDim := Scorecard{N: 8, Overall: 4.14, DimMeans: map[string]float64{
		"appropriate_band": 3.95, "correct_diagnosis": 4.20, "evidence_grounded": 4.30, "sensible_proposal": 4.10,
	}}
	norm := NormalizeScorecard(fourDim)
	// (3.95+4.20+4.30+4.10+1.0)/5 = 3.51 — the honest number the 07-25 card should have published.
	if norm.Overall != 3.51 {
		t.Fatalf("4-dim card must renormalize 4.14 -> 3.51, got %v", norm.Overall)
	}
	if norm.DimMeans["falsifiable_prediction"] != AbstentionFloor || norm.DimSamples["falsifiable_prediction"] != 0 {
		t.Fatalf("missing dim must be floored with samples=0, got %v / %v", norm.DimMeans, norm.DimSamples)
	}
	if norm.OverallFormula != OverallFormulaV2 {
		t.Fatalf("normalized card must carry the v2 stamp")
	}
	full := baselineFixture().Scorecard
	if got := NormalizeScorecard(full); got.Overall != full.Overall {
		t.Fatalf("a full-denominator card is a fixed point: want %v, got %v", full.Overall, got.Overall)
	}
	v2 := norm
	if got := NormalizeScorecard(v2); got.Overall != v2.Overall {
		t.Fatalf("a v2 card must pass through unchanged")
	}
	degraded := Scorecard{N: 8, Overall: 0}
	if got := NormalizeScorecard(degraded); got.Overall != 0 || got.OverallFormula != "" {
		t.Fatalf("a degraded card must pass through for VerifyIntegrity to reject, got %+v", got)
	}
}

// The base arm under a floor is main's sin, not the candidate's: it must WARN loudly, never fail the
// candidate (the trend-watch owns that red).
func TestCompare_BaseArmDegradedWarnsNotFails(t *testing.T) {
	base := baselineFixture()
	base.Scorecard.ProposalRate = 0.05
	cand := card(3.09, base.Scorecard.DimMeans)
	v := Compare(base, []Scorecard{cand}, nil, DefaultThresholds())
	if !v.Pass {
		t.Fatalf("a healthy candidate against a degraded base must PASS, got %v", v.Reasons)
	}
	if len(v.Warnings) == 0 {
		t.Fatalf("a degraded base arm must produce a BASE-ARM DEGRADED warning")
	}
}

// Refined floor selection (first-live-run finding, 2026-07-30): when every expected-propose incident was
// stale-excluded, proposal capability is UNMEASURED — the raw floors must not FAIL the run (proposing on a
// live-contradicted incident is WRONG, so a raw-rate floor would punish correct behavior).
//
// TG-258: but "the floors must not fail it" is not "the gate may certify it". Until this test was rewritten
// it asserted `!v.Pass -> fatal`, i.e. it PINNED the defect: a run that skipped every proposal bar was
// required to come back PASS. The honest verdict is INCONCLUSIVE — no bar broken, no bar applied — and it
// must be a non-pass to every caller (tools/evalgate exits non-zero on !Pass in both modes).
func TestCompare_AllProposeStaleIsInconclusiveNotPass(t *testing.T) {
	base := baselineFixture()
	cand := card(4.2, base.Scorecard.DimMeans)
	cand.Overall = 3.09 // hold the deltas neutral; the floors are what this test isolates
	cand.DimMeans = base.Scorecard.DimMeans
	cand.ProposalRate, cand.PredictionRate = 0, 0
	cand.ExpectedProposeN, cand.StaleExcluded, cand.LabeledStanddownN = 0, 3, 5
	cand.StanddownPrecision = 1.0
	v := Compare(base, []Scorecard{cand}, nil, DefaultThresholds())
	if v.Pass {
		t.Fatalf("an all-propose-stale run measured nothing about propose behavior — it must NEVER be a pass: %+v", v)
	}
	if v.Outcome != OutcomeInconclusive {
		t.Fatalf("Outcome=%q want %q (no bar was broken; a bar was skipped)", v.Outcome, OutcomeInconclusive)
	}
	// It must NOT be dressed up as a regression either: no raw-rate floor may have fired.
	for _, r := range v.Rates {
		t.Fatalf("no proposal floor may be applied on an unmeasured run (a stand-down on a stale incident is correct); got %+v", r)
	}
	if len(v.Unmeasured) != 1 || !strings.Contains(v.Unmeasured[0], "proposal capability") {
		t.Fatalf("the skipped bar must be named in Unmeasured, got %v", v.Unmeasured)
	}
	found := false
	for _, w := range v.Warnings {
		if strings.Contains(w, "UNMEASURED") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unmeasured-capability warning must be loud, got %v", v.Warnings)
	}
	// The non-pass path prints Reasons; the skipped bar must be there too, not only in the warnings a reader
	// skims past next to a green headline.
	found = false
	for _, r := range v.Reasons {
		if strings.Contains(r, "UNMEASURED") {
			found = true
		}
	}
	if !found {
		t.Fatalf("an INCONCLUSIVE verdict must carry its reason, got %v", v.Reasons)
	}
	// The same run WITH a measurable propose incident is certifiable again — proving the block is the missing
	// measurement, not some new blanket refusal (a gate that can never pass is as useless as one that always does).
	measured := cand
	measured.ExpectedProposeN, measured.StaleExcluded, measured.ProposalRecall = 4, 0, 1.0
	measured.ProposalRate, measured.PredictionRate = 0.20, 0.20
	if v2 := Compare(base, []Scorecard{measured}, nil, DefaultThresholds()); !v2.Pass || v2.Outcome != OutcomePass {
		t.Fatalf("a run that actually measured propose capability at recall 1.00 must PASS; got %q %v", v2.Outcome, v2.Reasons)
	}
}

// A corpus with labels but ZERO expected-propose incidents (only stand-downs) is the second shape of the
// same hole: no floor can apply, so nothing about propose behavior is under test. It must be INCONCLUSIVE,
// with a message that names THIS cause (the remediation differs from the stale-exclusion case).
func TestCompare_NoProposeLabelsAtAllIsInconclusive(t *testing.T) {
	base := baselineFixture()
	cand := card(3.09, base.Scorecard.DimMeans)
	cand.ProposalRate, cand.PredictionRate = 0, 0
	cand.ExpectedProposeN, cand.StaleExcluded, cand.LabeledStanddownN = 0, 0, 8
	cand.StanddownPrecision = 1.0
	v := Compare(base, []Scorecard{cand}, nil, DefaultThresholds())
	if v.Pass || v.Outcome != OutcomeInconclusive {
		t.Fatalf("a stand-down-only corpus cannot certify propose capability; got pass=%v outcome=%q", v.Pass, v.Outcome)
	}
	if len(v.Unmeasured) != 1 || !strings.Contains(v.Unmeasured[0], "no expected-propose incident at all") {
		t.Fatalf("the message must name the missing labels, got %v", v.Unmeasured)
	}
}

// TestOutcome_PassIsTheOnlyTruthyOutcome pins the invariant the whole three-valued design rests on: Pass is
// derived from Outcome and is true for OutcomePass alone. If a later edit ever makes INCONCLUSIVE truthy,
// every caller that reads v.Pass (both tools/evalgate exit paths, the trend self-refresh guard, the report)
// silently starts certifying unmeasured runs again — the defect this replaced, but harder to see.
func TestOutcome_PassIsTheOnlyTruthyOutcome(t *testing.T) {
	base := baselineFixture()
	dims := base.Scorecard.DimMeans
	clean := card(3.09, dims)

	unmeasured := clean
	unmeasured.ProposalRate, unmeasured.PredictionRate = 0, 0
	unmeasured.ExpectedProposeN, unmeasured.StaleExcluded, unmeasured.LabeledStanddownN = 0, 3, 5

	regressed := card(2.50, dims)

	// A run that BOTH regressed and measured nothing is a FAIL: the regression is the provable defect and
	// must not be softened into "inconclusive".
	both := regressed
	both.ProposalRate, both.PredictionRate = 0, 0
	both.ExpectedProposeN, both.StaleExcluded, both.LabeledStanddownN = 0, 3, 5

	for _, c := range []struct {
		name string
		cand Scorecard
		want Outcome
	}{
		{"clean", clean, OutcomePass},
		{"unmeasured", unmeasured, OutcomeInconclusive},
		{"regressed", regressed, OutcomeFail},
		{"regressed AND unmeasured", both, OutcomeFail},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := Compare(base, []Scorecard{c.cand}, nil, DefaultThresholds())
			if v.Outcome != c.want {
				t.Fatalf("Outcome=%q want %q (reasons=%v)", v.Outcome, c.want, v.Reasons)
			}
			if v.Pass != (v.Outcome == OutcomePass) {
				t.Fatalf("Pass=%v must be exactly (Outcome==%q); got Outcome=%q", v.Pass, OutcomePass, v.Outcome)
			}
			if v.Pass && len(v.Unmeasured) > 0 {
				t.Fatalf("a PASS may never carry an unmeasured capability: %v", v.Unmeasured)
			}
			if !v.Pass && len(v.Reasons) == 0 {
				t.Fatalf("a non-pass must say why (%q)", v.Outcome)
			}
		})
	}
}

// With LIVE expected-propose incidents, recall owns the proposal verdict — and a proposal shipping without
// its committed prediction (coverage trailing) is an independent grounding failure.
func TestCompare_LabeledPredictionCoverage(t *testing.T) {
	base := baselineFixture()
	cand := card(3.09, base.Scorecard.DimMeans)
	cand.ExpectedProposeN, cand.ProposalRecall = 4, 0.75
	cand.ProposalRate, cand.PredictionRate = 0.20, 0.05 // proposals shipping unpredicted
	v := Compare(base, []Scorecard{cand}, nil, DefaultThresholds())
	if v.Pass {
		t.Fatalf("prediction coverage trailing proposals must FAIL, got PASS")
	}
	ok := false
	for _, r := range v.Reasons {
		if strings.Contains(r, "trails proposal_rate") {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("the reason must name the coverage gap, got %v", v.Reasons)
	}
	// The same labeled card with sound coverage passes — and note the raw 0.25 floor does NOT apply
	// (0.20 proposal_rate at recall 0.75 on 4-of-20 live incidents is healthy, not collapsed).
	cand.PredictionRate = 0.20
	if v2 := Compare(base, []Scorecard{cand}, nil, DefaultThresholds()); !v2.Pass {
		t.Fatalf("healthy labeled card must PASS without the raw floor false-firing, got %v", v2.Reasons)
	}
}

// TestMarkUnmeasured_OnlyEverDowngrades pins the one-way property of the caller-side skip channel. Compare
// cannot see every skipped bar — the negative-control bar exists only if the caller supplied a control arm,
// and a nil `controls` slice means "passed nothing", which most unit callers legitimately do — so
// tools/evalgate reports that fact through MarkUnmeasured. The danger of any such setter is that it becomes
// a second, softer spelling of the verdict: something that turns a FAIL into an "inconclusive" (a regression
// laundered into a shrug) or, worse, quietly leaves a run certifiable. It must only ever move PASS ->
// INCONCLUSIVE, must keep Pass derived from Outcome, and must print its reason.
func TestMarkUnmeasured_OnlyEverDowngrades(t *testing.T) {
	base := baselineFixture()
	dims := base.Scorecard.DimMeans

	// A run that broke nothing and measured everything: certifiable until a caller reports a skipped bar.
	ok := card(3.09, dims)
	ok.ProposalRate, ok.PredictionRate = 0.30, 0.30
	ok.ExpectedProposeN, ok.ProposalRecall = 4, 1.0
	v := Compare(base, []Scorecard{ok}, nil, DefaultThresholds())
	if !v.Pass || v.Outcome != OutcomePass {
		t.Fatalf("fixture must start certifiable, got %q %v", v.Outcome, v.Reasons)
	}
	v.MarkUnmeasured("negative controls: no control arm was supplied")
	if v.Pass || v.Outcome != OutcomeInconclusive {
		t.Fatalf("a caller-reported skipped bar must make the run INCONCLUSIVE, got pass=%v outcome=%q", v.Pass, v.Outcome)
	}
	if len(v.Unmeasured) != 1 || !strings.Contains(strings.Join(v.Reasons, "|"), "negative controls") {
		t.Fatalf("the skipped bar must be named in Unmeasured AND Reasons: %v / %v", v.Unmeasured, v.Reasons)
	}

	// A REGRESSION stays a regression: reporting a skipped bar afterwards must not soften the red, and the
	// regression's own reason must survive alongside the new one.
	bad := card(2.50, dims)
	bad.ProposalRate, bad.PredictionRate = 0.30, 0.30
	bad.ExpectedProposeN, bad.ProposalRecall = 4, 1.0
	f := Compare(base, []Scorecard{bad}, nil, DefaultThresholds())
	if f.Outcome != OutcomeFail {
		t.Fatalf("fixture must start FAIL, got %q", f.Outcome)
	}
	before := len(f.Reasons)
	f.MarkUnmeasured("negative controls: no control arm was supplied")
	if f.Outcome != OutcomeFail || f.Pass {
		t.Fatalf("an unmeasured capability must never soften a regression into %q", f.Outcome)
	}
	if len(f.Reasons) != before+1 {
		t.Fatalf("the FAIL's own reasons must survive and the skip must be added once: %v", f.Reasons)
	}

	// Re-resolving is idempotent: a repeated report cannot inflate the record with duplicate reasons, and it
	// certainly cannot walk the outcome back up.
	f.MarkUnmeasured("negative controls: no control arm was supplied")
	if len(f.Reasons) != before+1 || len(f.Unmeasured) != 1 || f.Pass {
		t.Fatalf("resolution must be idempotent in what it records, got unmeasured=%v reasons=%v", f.Unmeasured, f.Reasons)
	}
}

// TestCompare_DisabledFloorIsUnmeasuredNotSilent covers the THIRD shape of the same defect: the bar is
// inapplicable not because the corpus failed to exercise the capability, but because the invocation turned
// the bar off (`--recall-floor 0`, `--proposal-floor 0`). Measured 2026-08-03 before this change: the CLI
// with `--recall-floor 0` printed "GATE: PASS" and exited 0 on a candidate whose proposal_recall was 0.00
// over four LIVE action-warranted incidents — a total collapse of exactly what the bar exists to catch —
// and no line in the report said a bar had been disabled. Disabling remains permitted; certifying on the
// back of it does not.
func TestCompare_DisabledFloorIsUnmeasuredNotSilent(t *testing.T) {
	base := baselineFixture()
	cand := card(3.09, base.Scorecard.DimMeans)
	cand.ProposalRate, cand.PredictionRate = 0.30, 0.30
	cand.ExpectedProposeN, cand.ProposalRecall = 4, 0.0 // stood down on EVERY action-warranted incident

	th := DefaultThresholds()
	if v := Compare(base, []Scorecard{cand}, nil, th); v.Outcome != OutcomeFail {
		t.Fatalf("with the bar ARMED this collapse must FAIL, got %q %v", v.Outcome, v.Reasons)
	}
	th.ProposalRecallFloor = 0 // the operator disarms the only bar on propose capability
	v := Compare(base, []Scorecard{cand}, nil, th)
	if v.Pass {
		t.Fatalf("a disabled bar must not hand out a certification: %+v", v)
	}
	if v.Outcome != OutcomeInconclusive {
		t.Fatalf("Outcome=%q want %q — nothing was proven bad, but nothing was proven good either", v.Outcome, OutcomeInconclusive)
	}
	if len(v.Unmeasured) != 1 || !strings.Contains(v.Unmeasured[0], "proposal_recall") || !strings.Contains(v.Unmeasured[0], "DISABLED") {
		t.Fatalf("the verdict must name the bar that was switched off, got %v", v.Unmeasured)
	}
}

// TestResolutionRecall_ReportedNotGated is the load-bearing proof of TG-507's contract: the retrieval-recall
// fields ride on the scorecard for the change-gate diff but NEVER move the gate verdict. It computes a Verdict
// twice over the SAME clean-PASS arms — once with the resolution-recall fields absent (zero), once with them
// populated to a MAXIMAL-regression shape (candidate of-findable collapsed to 0.0 against a base of 0.95, the
// shape a real retrieval regression takes) — and asserts the two Verdicts are byte-identical. If any threshold
// or Verdict path ever consulted these fields, the populated arm would flip and this test reddens. The field
// IS still carried through Pool (so it reaches the printed diff) — reported, just not gated.
func TestResolutionRecall_ReportedNotGated(t *testing.T) {
	th := DefaultThresholds()
	base := baselineFixture()
	// Candidate == baseline dims/overall ⇒ every Δ is 0 ⇒ a clean PASS with no resolution field populated.
	cand := card(base.Scorecard.Overall, base.Scorecard.DimMeans)

	v1 := Compare(base, []Scorecard{cand}, nil, th)
	if !v1.Pass {
		t.Fatalf("fixture must be a clean PASS to isolate the field's (non-)effect; got outcome=%s reasons=%v", v1.Outcome, v1.Reasons)
	}

	// The SAME arms, now with resolution-recall populated to the worst case a real retrieval regression takes:
	// base high (0.95), candidate collapsed to 0.0 against a full 1.0 ceiling. A GATED field would fail this arm.
	base2 := base
	base2.Scorecard.ResolutionRecall = 0.95
	base2.Scorecard.ResolutionRecallCeiling = 1.0
	base2.Scorecard.ResolutionRecallOfFindable = 0.95
	cand2 := cand
	cand2.ResolutionRecall = 0.0
	cand2.ResolutionRecallCeiling = 1.0
	cand2.ResolutionRecallOfFindable = 0.0
	v2 := Compare(base2, []Scorecard{cand2}, nil, th)

	// Byte-identical verdicts ⇒ the field is REPORTED-only: it changed nothing the gate certifies on. This is
	// the assertion that proves reported-not-gated — if a threshold ever reads these fields, v2 diverges.
	j1, _ := json.MarshalIndent(v1, "", "  ")
	j2, _ := json.MarshalIndent(v2, "", "  ")
	if string(j1) != string(j2) {
		t.Fatalf("gate Verdict CHANGED when resolution-recall was populated — the metric is GATED, not reported.\nwithout field:\n%s\nwith field:\n%s", j1, j2)
	}

	// ...but the field IS carried through pooling, so the change-gate diff can print it (reported, not gated).
	if got := Pool([]Scorecard{cand2}).ResolutionRecallOfFindable; got != 0.0 {
		t.Fatalf("Pool dropped a populated resolution_recall_of_findable: got %.3f, want 0.00", got)
	}
	if got := Pool([]Scorecard{base2.Scorecard}).ResolutionRecallOfFindable; got != 0.95 {
		t.Fatalf("Pool mangled resolution_recall_of_findable: got %.3f, want 0.95", got)
	}
}

// TestCompare_OverallSampleAwareBand_TG522 proves the overall floor got TG-500's sample-aware band treatment:
// a drop past the -0.15 floor but WITHIN the overall's own measured spread resolves INCONCLUSIVE (escalate to
// the pooled full gate), never a bare FAIL -- fixing the Opus-5-brain false-FAILs (FAST overall Δ -0.17/-0.28
// on changes that PASS the pooled full gate). A drop BEYOND the band still FAILs (anti-fail-open), and pooling
// shrinks the band so a run-consistent drop resolves to a real FAIL.
func TestCompare_OverallSampleAwareBand_TG522(t *testing.T) {
	th := DefaultThresholds()
	cleanDims := map[string]float64{"appropriate_band": 4.2, "correct_diagnosis": 3.25, "evidence_grounded": 3.2, "falsifiable_prediction": 1.6, "sensible_proposal": 3.2}
	mk := func(n int, overall float64) Scorecard {
		return Scorecard{N: n, Overall: overall, DimMeans: cleanDims, ProposalRate: 0.45, PredictionRate: 0.45, Judged: n}
	}
	fastBase := Baseline{MeasuredAt: "2026-08-18", GitSHA: "test", Runs: 1, Scorecard: mk(8, 3.09)}
	base3 := Baseline{MeasuredAt: "2026-08-18", GitSHA: "test", Runs: 3, Scorecard: mk(8, 3.09)}

	// (1) The evidence case: FAST overall Δ -0.28 (2.81 vs 3.09), every dim clean -> INCONCLUSIVE, not FAIL.
	v := Compare(fastBase, []Scorecard{mk(8, 2.81)}, nil, th)
	if v.Outcome != OutcomeInconclusive {
		t.Fatalf("FAST overall Δ -0.28 within the band MUST be INCONCLUSIVE, got %s (band ±%.2f, reasons=%v)", v.Outcome, v.OverallBand, v.Reasons)
	}
	if v.Pass {
		t.Fatal("INCONCLUSIVE is never a PASS")
	}
	if v.OverallBand < 0.5 {
		t.Fatalf("expected a wide FAST overall band (~0.62 at n=8), got %.2f", v.OverallBand)
	}

	// (2) A drop BEYOND the band still FAILs (anti-fail-open): Δ -0.90 at n=8 (past ±~0.62).
	vBig := Compare(fastBase, []Scorecard{mk(8, 2.19)}, nil, th)
	if vBig.Outcome != OutcomeFail {
		t.Fatalf("overall Δ -0.90 beyond the band MUST FAIL, got %s (band ±%.2f)", vBig.Outcome, vBig.OverallBand)
	}
	foundBeyond := false
	for _, r := range vBig.Reasons {
		if contains(r, "beyond the ±") {
			foundBeyond = true
		}
	}
	if !foundBeyond {
		t.Fatalf("beyond-band FAIL must name the sample-aware band; reasons=%v", vBig.Reasons)
	}

	// (3) Pooling shrinks the band: a run-consistent Δ -0.45 is INCONCLUSIVE at 1 FAST run (band ±~0.62)
	// but FAILs pooled over 3 matched runs (n 8->24, band ±~0.36) -- absorbed noise, not a real drop, is the
	// only thing that survives.
	vSingle := Compare(fastBase, []Scorecard{mk(8, 2.64)}, nil, th)
	if vSingle.Outcome != OutcomeInconclusive {
		t.Fatalf("Δ -0.45 at 1 FAST run must be INCONCLUSIVE (band ±%.2f), got %s", vSingle.OverallBand, vSingle.Outcome)
	}
	vPooled := Compare(base3, []Scorecard{mk(8, 2.64), mk(8, 2.64), mk(8, 2.64)}, nil, th)
	if vPooled.Outcome != OutcomeFail {
		t.Fatalf("Δ -0.45 pooled over 3 matched runs must FAIL (band shrinks to ±%.2f), got %s", vPooled.OverallBand, vPooled.Outcome)
	}
	if !(vPooled.OverallBand < vSingle.OverallBand) {
		t.Fatalf("pooled band (%.2f) must be tighter than the single-run band (%.2f)", vPooled.OverallBand, vSingle.OverallBand)
	}

	// (4) A drop still within the -0.15 FLOOR remains a clean PASS (the floor keeps its PASS role).
	if vSmall := Compare(fastBase, []Scorecard{mk(8, 3.00)}, nil, th); !vSmall.Pass {
		t.Fatalf("overall Δ -0.09 within the floor must PASS; reasons=%v", vSmall.Reasons)
	}
}
