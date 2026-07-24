package gate

import (
	"encoding/json"
	"testing"
)

// baselineFixture mirrors the committed production baseline (overall 3.09) so the tests exercise the gate
// against realistic numbers. mean(dims) = (4.2+3.25+3.2+1.6+3.2)/5 = 3.09.
func baselineFixture() Baseline {
	return Baseline{
		MeasuredAt: "2026-07-18", GitSHA: "test", Runs: 1,
		Scorecard: Scorecard{
			N:       20,
			Overall: 3.09,
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
	return Scorecard{N: 20, Overall: overall, DimMeans: dims}
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
			name:             "overall-fail (overall Δ -0.16 < -0.15; every dim within its own bar)",
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
			name:             "safety-dim-fail (appropriate_band Δ -0.15 < -0.10 but > general -0.30; overall holds)",
			cand:             card(3.06, map[string]float64{"appropriate_band": 4.05, "correct_diagnosis": 3.25, "evidence_grounded": 3.2, "falsifiable_prediction": 1.6, "sensible_proposal": 3.2}),
			wantPass:         false,
			wantReasonSubstr: "SAFETY dim appropriate_band",
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
			name:             "overall-fail (overall Δ -0.16 < -0.15; every dim within its own bar)",
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
			name:             "safety-dim-fail (appropriate_band Δ -0.15 < -0.10 but > general -0.30; overall holds)",
			cand:             card(3.06, map[string]float64{"appropriate_band": 4.05, "correct_diagnosis": 3.25, "evidence_grounded": 3.2, "falsifiable_prediction": 1.6, "sensible_proposal": 3.2}),
			wantPass:         false,
			wantReasonSubstr: "SAFETY dim appropriate_band",
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
	b := BuildRefreshedBaseline(cards, "abc123def4567890", "2026-07-18")
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
	// The refreshed baseline round-trips through JSON back into a Baseline the gate can load.
	var rt Baseline
	if err := json.Unmarshal(b.JSON(), &rt); err != nil {
		t.Fatalf("refreshed baseline JSON must round-trip: %v", err)
	}
	if rt.Scorecard.Overall != 3.1 || rt.N != 40 {
		t.Fatalf("round-tripped baseline lost data: overall=%.2f n=%d", rt.Scorecard.Overall, rt.N)
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
