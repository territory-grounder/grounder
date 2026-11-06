package main

import (
	"math"
	"testing"

	"github.com/territory-grounder/grounder/core/db"
)

// THE FALSIFIABILITY SIGNAL WAS DIVIDED BY A DENOMINATOR THAT HAD BEEN ROUNDED UP TO 1.
//
// The floor was borrowed from core/predict.ControlScore.Ratio(), where it is CORRECT: there the operand is
// a per-prediction integer count, 0 and 1 are genuinely adjacent, and the floor only prevents a
// divide-by-zero. Here both operands are MEANS over the whole scored population, and on this estate they
// live in (0,1) — so the "floor" was not guarding an edge case, it was rewriting every result in the range.
//
// Measured live on 2026-07-29 across 321 scored predictions: sum(tp)=129, sum(control_tp)=74, giving
// AvgRealTP=0.4019 and AvgControlTP=0.2305 — the committed prediction beat its degree-preserving
// shuffled-graph control by 1.744x. The floored arithmetic published 0.4019/1 = 0.40, and the console
// captioned TG's own differentiator "at or below chance". The verdict was not merely understated; it was
// INVERTED, and it had been for as long as the surface existed.
//
// These cases are the closed set of shapes the aggregate can take: a control that fires, a control that is
// silent, an empty spine, and the boundary where the old floor and the correct arithmetic agree.
func TestSignalRatioDividesByTheControlAsMeasured(t *testing.T) {
	cases := []struct {
		name        string
		agg         db.GroundingAgg
		wantRatio   float64 // -1 = do not check (silent control)
		wantSilent  bool
		wantBeatsAA bool // does the surface's >=1.15 bar read as beaten?
	}{
		{
			// THE LIVE POPULATION. This is the case the floor inverted.
			name:        "live 2026-07-29: 321 scored, 129 real tp, 74 control tp",
			agg:         db.GroundingAgg{Predictions: 321, SumTP: 129, SumControlTP: 74},
			wantRatio:   (129.0 / 321.0) / (74.0 / 321.0),
			wantBeatsAA: true,
		},
		{
			// Both means below 1 and the real one LOSES: the ratio must be able to say so. Under the old
			// floor this also read as 0.20 — the same number a 5x win could produce — so the floor did not
			// merely understate, it collapsed distinguishable outcomes onto one value.
			name:      "control outperforms the prediction",
			agg:       db.GroundingAgg{Predictions: 100, SumTP: 20, SumControlTP: 80},
			wantRatio: 0.25,
		},
		{
			// A genuinely silent control leaves the ratio unbounded. No finite number is honest, so none is
			// published and the surface is told why.
			name:       "silent control, real signal present",
			agg:        db.GroundingAgg{Predictions: 50, SumTP: 40, SumControlTP: 0},
			wantRatio:  0,
			wantSilent: true,
		},
		{
			// Nothing scored at all is an absence of evidence. It must NOT set ControlSilent, or the surface
			// would announce an unbounded signal computed from nothing.
			name:      "empty spine",
			agg:       db.GroundingAgg{Predictions: 0},
			wantRatio: 0,
		},
		{
			// Neither side hit anything. Not a silent control beating nothing — nothing at all.
			name:      "scored predictions, both sides zero",
			agg:       db.GroundingAgg{Predictions: 30, SumTP: 0, SumControlTP: 0},
			wantRatio: 0,
		},
		{
			// The one place the old floor and the correct arithmetic agree: AvgControlTP exactly 1.
			// A regression that reinstated the floor would still pass HERE, which is why it is not the
			// only case.
			name:        "control mean exactly 1 — floor and truth coincide",
			agg:         db.GroundingAgg{Predictions: 10, SumTP: 30, SumControlTP: 10},
			wantRatio:   3,
			wantBeatsAA: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc := scorecardFrom(c.agg)
			if math.Abs(sc.SignalRatio-c.wantRatio) > 1e-9 {
				t.Errorf("SignalRatio = %v, want %v (avg real %v / avg control %v)",
					sc.SignalRatio, c.wantRatio, sc.AvgRealTP, sc.AvgControlTP)
			}
			if sc.ControlSilent != c.wantSilent {
				t.Errorf("ControlSilent = %v, want %v", sc.ControlSilent, c.wantSilent)
			}
			if beats := sc.SignalRatio >= 1.15; beats != c.wantBeatsAA {
				t.Errorf("surface verdict (ratio>=1.15) = %v, want %v (ratio %v)", beats, c.wantBeatsAA, sc.SignalRatio)
			}
			// The ratio must always be the two published means divided — a reader who does the arithmetic
			// from the bars on screen must land on the headline. That equality IS the defect's absence.
			if sc.AvgControlTP > 0 {
				want := sc.AvgRealTP / sc.AvgControlTP
				if math.Abs(sc.SignalRatio-want) > 1e-9 {
					t.Errorf("headline %v disagrees with its own published bars (%v / %v = %v)",
						sc.SignalRatio, sc.AvgRealTP, sc.AvgControlTP, want)
				}
			}
		})
	}
}

// The live population must not merely be "above 1" — it must reproduce the exact figure the surface prints,
// so a future change that quietly re-rounds the denominator is caught by a number, not by a direction.
func TestSignalRatioReproducesTheLiveFigure(t *testing.T) {
	sc := scorecardFrom(db.GroundingAgg{Predictions: 321, SumTP: 129, SumControlTP: 74})
	if got := math.Round(sc.SignalRatio*100) / 100; got != 1.74 {
		t.Fatalf("live signal ratio = %v (rounded %v), want 1.74 — the floored denominator published 0.40", sc.SignalRatio, got)
	}
	if got := math.Round(sc.AvgRealTP*10000) / 10000; got != 0.4019 {
		t.Errorf("AvgRealTP = %v, want 0.4019", got)
	}
	if got := math.Round(sc.AvgControlTP*10000) / 10000; got != 0.2305 {
		t.Errorf("AvgControlTP = %v, want 0.2305", got)
	}
}

// A FIXED PREDICTOR MUST BE ABLE TO SHOW AS FIXED (TG-92).
//
// The all-time sums cannot recover from a corrected defect. TG-61's blast-radius miscalibration left ~26
// rows summing tp=1, fp=730 — one leaf guest's local fault predicting ~130 co-hosted siblings. Because the
// scorecard summed all-time with no window, the SignalRatio kept describing the CURRENT predictor as badly
// calibrated, and would keep doing so until 730+ well-calibrated predictions outweighed the history. A
// readiness signal that cannot say "fixed" is not a signal, it is a permanent accusation.
//
// This pins BOTH halves of the intended behaviour: the all-time figure still reports the poisoned history
// (the audit trail is not rewritten by a fix), and the rolling figure reports the model that is running.
func TestARepairedPredictorShowsAsRepairedInTheRollingViewWhileTheRecordKeeps(t *testing.T) {
	// All-time: the poisoned corpus (26 pre-fix rows, tp=1 fp=730) plus 40 well-calibrated ones.
	// Rolling: only the 40 good ones, which is what "is it calibrated now" must answer over.
	agg := db.GroundingAgg{
		Predictions: 66, SumTP: 21, SumFP: 734, SumControlTP: 12,
		RecentPredictions: 40, RecentSumTP: 20, RecentSumFP: 4, RecentSumControlTP: 11,
		RecentWindow: db.GroundingRecentWindow,
	}
	sc := scorecardFrom(agg)

	// The record keeps its shape: over-prediction is catastrophic all-time (734/66 ≈ 11.1 FPs per
	// prediction). If this ever starts reading like the rolling number, the audit trail has been rewritten.
	if sc.AvgFalsePositives < 10 {
		t.Fatalf("all-time AvgFalsePositives = %.2f, want the poisoned history preserved (~11.1) — a "+
			"calibration fix must not retro-clean the record", sc.AvgFalsePositives)
	}
	// The rolling view shows the repaired model: 4/40 = 0.1 FPs per prediction.
	if sc.RecentAvgFalsePositives > 0.5 {
		t.Fatalf("rolling AvgFalsePositives = %.2f, want ~0.1 — the rolling view is still being dragged "+
			"down by pre-fix rows, which is the whole defect TG-92 describes", sc.RecentAvgFalsePositives)
	}
	// And the rolling signal ratio must clear chance on the repaired population: (20/40)/(11/40) ≈ 1.82.
	if sc.RecentSignalRatio <= 1 {
		t.Fatalf("rolling SignalRatio = %.3f, want > 1 (real beats the shuffled control on the repaired "+
			"population) — a fixed predictor that still reports at-or-below chance cannot pass a readiness "+
			"gate no matter how well it performs", sc.RecentSignalRatio)
	}
	if sc.RecentWindow != db.GroundingRecentWindow {
		t.Fatalf("RecentWindow = %d, want %d — the surface must say how many predictions the rolling "+
			"number covers, so a reader can tell a stable result from three rows of noise",
			sc.RecentWindow, db.GroundingRecentWindow)
	}
}

// An EMPTY rolling window must not manufacture a verdict: zero recent predictions means "no current
// evidence", which is a different statement from "calibrated" or "miscalibrated".
func TestAnEmptyRollingWindowReportsNothingRatherThanZero(t *testing.T) {
	sc := scorecardFrom(db.GroundingAgg{Predictions: 26, SumTP: 1, SumFP: 730, RecentPredictions: 0})
	if sc.RecentSignalRatio != 0 || sc.RecentControlSilent {
		t.Fatalf("empty window produced RecentSignalRatio=%.3f ControlSilent=%t — with no recent scored "+
			"predictions the honest output is an absence, never a computed figure",
			sc.RecentSignalRatio, sc.RecentControlSilent)
	}
}
