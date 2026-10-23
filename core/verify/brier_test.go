package verify

import "testing"

// TG-189 — the calibration score, and the distinction it exists to preserve.
//
// The estate graph already computed a per-host path-product confidence; core/predict flattened it to a
// set before verify/falsify could see it. The cost was not a missing number, it was an unanswerable
// question: a model claiming 0.95 about 44 hosts and one claiming 0.12 about the same 44 are IDENTICAL
// under the set, and are wildly different forecasts. Brier is what tells them apart.

func set(hosts ...string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, h := range hosts {
		m[h] = struct{}{}
	}
	return m
}

// A CONFIDENT AND CORRECT forecast scores 0; a CONFIDENT AND WRONG one scores 1. Without this the metric
// could be inverted and every other assertion here would still pass.
func TestBrierRewardsConfidentAndCorrectAndPunishesConfidentAndWrong(t *testing.T) {
	p := Prediction{
		PredictedHosts: set("a", "b"),
		HostConfidence: map[string]float64{"a": 1.0, "b": 0.0},
	}
	// "a" will alert (we said 1.0), "b" will not (we said 0.0) — perfect.
	if got, n, ok := p.Brier(set("a")); !ok || n != 2 || got != 0 {
		t.Fatalf("perfect forecast scored %v over %d (ok=%v), want 0 over 2", got, n, ok)
	}
	// Now invert reality: everything we were certain about was wrong.
	if got, _, ok := p.Brier(set("b")); !ok || got != 1 {
		t.Fatalf("maximally wrong forecast scored %v, want 1 — the metric may be inverted", got)
	}
}

// Hedging scores 0.25 whatever happens. This is the reference point that makes the number readable: a
// model that says 0.5 about everything can never beat 0.25, so any score below it is real information.
func TestHedgingAtHalfAlwaysScoresAQuarter(t *testing.T) {
	p := Prediction{
		PredictedHosts: set("a", "b", "c"),
		HostConfidence: map[string]float64{"a": 0.5, "b": 0.5, "c": 0.5},
	}
	for _, alerted := range []map[string]struct{}{set(), set("a"), set("a", "b"), set("a", "b", "c")} {
		got, _, ok := p.Brier(alerted)
		if !ok || got != 0.25 {
			t.Errorf("hedged forecast scored %v against %d alerting host(s), want 0.25 always", got, len(alerted))
		}
	}
}

// UNSCORED IS NOT ZERO. This is the load-bearing assertion. A flat-graph model and a pre-0070 database row
// both carry no confidence, and returning 0.0 for them would render as a PERFECT calibration score on
// every dashboard — the exact "an absent measurement reads as a clean result" failure this repo keeps
// rediscovering.
func TestNoConfidenceReportsUnscoredRatherThanAPerfectScore(t *testing.T) {
	cases := map[string]Prediction{
		"nil confidence (pre-0070 row, or a flat DependencyGraph)": {
			PredictedHosts: set("a", "b"),
		},
		"empty confidence map": {
			PredictedHosts: set("a", "b"),
			HostConfidence: map[string]float64{},
		},
		"no predicted hosts at all": {
			HostConfidence: map[string]float64{"a": 0.9},
		},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			got, n, ok := p.Brier(set("a"))
			if ok {
				t.Fatalf("%s reported a SCORE (%v over %d) — it must report unscored, or 'never measured' "+
					"renders identically to 'perfectly calibrated'", name, got, n)
			}
			if n != 0 {
				t.Errorf("unscored result claims to cover %d host(s)", n)
			}
		})
	}
}

// A host named WITHOUT a confidence is skipped, not counted as 0. Counting it would fabricate a claim the
// model never made — and would silently improve the score every time coverage got worse.
func TestAHostNamedWithoutAConfidenceIsSkippedNotScoredAsZero(t *testing.T) {
	p := Prediction{
		PredictedHosts: set("a", "b"),
		HostConfidence: map[string]float64{"a": 1.0}, // "b" has none
	}
	got, n, ok := p.Brier(set("a", "b"))
	if !ok {
		t.Fatal("scoring failed entirely when one host lacked a confidence")
	}
	if n != 1 {
		t.Errorf("scored over %d host(s), want 1 — the unconfident host must not enter the denominator", n)
	}
	// "a" was 1.0 and alerted ⇒ 0. If "b" had been folded in as 0.0-vs-alerted it would be (0-1)^2 = 1,
	// giving 0.5 and making the model look worse for a claim it never made.
	if got != 0 {
		t.Errorf("score %v, want 0 — a host with no stated confidence leaked into the mean", got)
	}
}

// It scores over the PREDICTED set, not the alerting set. A host that alerted and was never predicted is a
// recall failure the control scorer already counts as RealFN; folding it in here would blend two questions.
func TestScoringIsOverThePredictedSetNotTheAlertingSet(t *testing.T) {
	p := Prediction{
		PredictedHosts: set("a"),
		HostConfidence: map[string]float64{"a": 1.0},
	}
	got, n, ok := p.Brier(set("a", "unpredicted-1", "unpredicted-2"))
	if !ok || n != 1 || got != 0 {
		t.Errorf("score %v over %d (ok=%v) — hosts that alerted but were never predicted must not enter "+
			"this metric; that is recall, and RealFN already measures it", got, n, ok)
	}
}
