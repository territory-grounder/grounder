package axis

import (
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
)

// TestScoreDerivesAxes proves the pure mapping computes each axis with its documented definition:
// A4 = (AUTO+AUTO_NOTICE)/total, A5 = distinct proposed ops, A2 = judge means sorted worst-first with a
// sample-WEIGHTED overall (a thinly-scored dimension must not swing it).
func TestScoreDerivesAxes(t *testing.T) {
	agg := db.AxisAgg{
		Since:              time.Unix(0, 0).UTC(),
		Total:              100,
		Judged:             90,
		Proposed:           30,
		Predicted:          25,
		Bands:              map[string]int{"AUTO": 15, "AUTO_NOTICE": 5, "POLL_PAUSE": 60, "<none>": 20},
		AutonomousStops:    18,                                                                // of the 20 <none>, 18 were autonomous no-proposal stops (the other 2 escalated)
		OpClasses:          []string{"disk-grow", "reboot", "restart-service", "start-guest"}, // faithful A5 unit (EXERCISED)
		GraduatedOpClasses: []string{"reload-service", "restart-container", "start-guest"},    // A5 CAPABILITY breadth (graduated to auto)
		Ops:                []string{"grow", "reboot", "restart", "start"},                    // legacy raw verbs (context)
		AlertRules:         []string{"a", "b", "c"},
		// falsifiable_prediction is the worst mean but thinly sampled; evidence_grounded is best, widely sampled.
		DimMeans: map[string]float64{"falsifiable_prediction": 3.0, "evidence_grounded": 4.6, "sensible_proposal": 4.2},
		DimN:     map[string]int{"falsifiable_prediction": 10, "evidence_grounded": 80, "sensible_proposal": 40},
		// Verified-outcome (ground truth): 40 of 50 committed predictions held; blast-radius tp/fp/fn.
		Verdicts:   map[string]int{"match": 40, "deviation": 8, "partial": 2},
		PredScored: 100, PredTP: 90, PredFP: 10, PredFN: 30, PredControlTP: 20, PredControlFP: 80,
		MeanSteps: 3.5, StepsN: 60, // axis A6a (steps)
		DecisionN: 55, DecisionMedianMs: 9000, DecisionP95Ms: 41000, // axis A6b (wall-clock) — a different fact about the same decisions
	}
	sc := Score(agg, 48*time.Hour)

	// A4 raw autonomy = (15+5)/100 = 0.20.
	if sc.AutonomyA4 != 0.20 {
		t.Fatalf("A4 autonomy: want 0.20, got %v", sc.AutonomyA4)
	}
	// A4 among-actionable = (15+5)/proposals(30) = 0.6667 — the honest actuation-autonomy rate.
	if sc.AutonomyActionable < 0.666 || sc.AutonomyActionable > 0.667 {
		t.Fatalf("A4 among-actionable: want ≈0.667, got %v", sc.AutonomyActionable)
	}
	// A4 handled-without-human = (20 auto + 18 autonomous stops)/100 = 0.38.
	if sc.HandledWithoutHuman != 0.38 {
		t.Fatalf("A4 handled-without-human: want 0.38, got %v", sc.HandledWithoutHuman)
	}
	// A5 breadth = 4 distinct ops.
	if sc.BreadthA5 != 4 {
		t.Fatalf("A5 breadth: want 4, got %d", sc.BreadthA5)
	}
	// A5 capability breadth = the graduated (auto-capable) op-classes, distinct from the exercised set.
	if sc.GraduatedBreadthA5 != 3 || !strings.Contains(sc.Text(), "auto-capable (graduated): 3") {
		t.Fatalf("A5 graduated breadth: want 3 rendered, got %d\n%s", sc.GraduatedBreadthA5, sc.Text())
	}
	// A2 worst-first: falsifiable_prediction (3.0) must be the first row.
	if len(sc.DimMeans) != 3 || sc.DimMeans[0].Dimension != "falsifiable_prediction" {
		t.Fatalf("A2 dims must sort worst-first, got %+v", sc.DimMeans)
	}
	// Weighted overall = (3.0*10 + 4.6*80 + 4.2*40)/(10+80+40) = (30+368+168)/130 = 566/130 ≈ 4.354.
	if sc.OverallA2 < 4.35 || sc.OverallA2 > 4.36 {
		t.Fatalf("A2 overall must be sample-weighted ≈4.354, got %v", sc.OverallA2)
	}
	// Rates.
	if sc.ProposalRate != 0.30 || sc.PredictionA2 != 0.25 {
		t.Fatalf("rates: want 0.30/0.25, got %v/%v", sc.ProposalRate, sc.PredictionA2)
	}
	// Bands sorted descending by count → POLL_PAUSE (60) first.
	if sc.Bands[0].Key != "POLL_PAUSE" || sc.Bands[0].N != 60 {
		t.Fatalf("bands must sort by count desc, got %+v", sc.Bands)
	}
	// A6a (decision steps) is LIVE-measurable (migration 0037) — mean carried through, and A6a is no longer a gap.
	if sc.MeanStepsA6 != 3.5 || sc.StepsN != 60 {
		t.Fatalf("A6a mean steps: want 3.5/n=60, got %v/%d", sc.MeanStepsA6, sc.StepsN)
	}
	// A6b (wall-clock) is the OTHER half, live from migration 0058 (TG-205). Both must survive the mapping:
	// this window recorded 60 looped triages and timed 55 of them.
	if sc.DecisionMedianMsA6b != 9000 || sc.DecisionN != 55 {
		t.Fatalf("A6b time-to-decision: want 9000ms/n=55, got %dms/n=%d", sc.DecisionMedianMsA6b, sc.DecisionN)
	}
	// The unmeasurable axes are named (honest coverage boundary). A6 then A8 became measurable, leaving A1/A3/A7
	// (A1 needs an injected fault, A3 needs a mutation — neither is set in this aggregate; A7 is still a gap).
	if len(sc.Unmeasurable) != 3 {
		t.Fatalf("want 3 named coverage gaps (A1/A3/A7) after A8 became measurable, got %d: %+v", len(sc.Unmeasurable), sc.Unmeasurable)
	}
	// Verified-outcome A2 (ground truth): match rate = 40/(40+8+2) = 0.80; verdicts sorted match-first.
	if sc.VerifiedN != 50 || sc.VerifiedA2 != 0.80 {
		t.Fatalf("A2 verified: want n=50 rate=0.80, got n=%d rate=%v", sc.VerifiedN, sc.VerifiedA2)
	}
	if sc.Verdicts[0].Key != "match" || sc.Verdicts[0].N != 40 {
		t.Fatalf("verdicts must sort by count desc (match first), got %+v", sc.Verdicts)
	}
	// Blast-radius hit-rate = 90/(90+10) = 0.90; control = 20/(20+80) = 0.20; recall = 90/(90+30) = 0.75.
	if sc.BlastPrecision != 0.90 || sc.BlastControl != 0.20 || sc.BlastRecall != 0.75 {
		t.Fatalf("blast hit/control/recall: want 0.90/0.20/0.75, got %v/%v/%v", sc.BlastPrecision, sc.BlastControl, sc.BlastRecall)
	}
}

// TestA1DetectionRecall: with injected faults recorded, A1 (detection recall) is measured (detected/injected)
// and A1 leaves the unmeasurable list; with none, A1 stays a named coverage gap (not a false 0).
func TestA1DetectionRecall(t *testing.T) {
	// 8 of 10 injected faults detected ⇒ recall 0.80, A1 measured (dropped from the 4 gaps to 3).
	sc := Score(db.AxisAgg{Since: time.Unix(0, 0).UTC(), Total: 10, InjectedFaults: 10, DetectedFaults: 8}, time.Hour)
	if sc.DetectionA1 != 0.80 {
		t.Fatalf("A1 recall: want 0.80, got %v", sc.DetectionA1)
	}
	for _, g := range sc.Unmeasurable {
		if g.Axis == "A1" {
			t.Fatalf("A1 must NOT be an unmeasurable gap once faults are injected, got %+v", sc.Unmeasurable)
		}
	}
	if !strings.Contains(sc.Text(), "A1  Detection recall") {
		t.Fatalf("measured A1 must render, got:\n%s", sc.Text())
	}
	// No injected faults ⇒ A1 stays a named gap (not 0/0 shown as a real 0).
	sc0 := Score(db.AxisAgg{Since: time.Unix(0, 0).UTC(), Total: 10}, time.Hour)
	found := false
	for _, g := range sc0.Unmeasurable {
		if g.Axis == "A1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("with no injected faults A1 must be a named coverage gap, got %+v", sc0.Unmeasurable)
	}
}

// TestA3HealSuccess: with actuated mutations recorded, A3 (heal success) is measured (confirmed-clear/mutated)
// and A3 leaves the unmeasurable list; with none, A3 stays a named coverage gap (not a false 0).
func TestA3HealSuccess(t *testing.T) {
	// 6 of 8 actuated heals confirmed clear ⇒ A3 = 0.75, A3 measured (dropped from the gaps).
	sc := Score(db.AxisAgg{Since: time.Unix(0, 0).UTC(), Total: 8, MutatedCount: 8, HealConfirmedCount: 6}, time.Hour)
	if sc.HealSuccessA3 != 0.75 {
		t.Fatalf("A3 heal success: want 0.75, got %v", sc.HealSuccessA3)
	}
	for _, g := range sc.Unmeasurable {
		if g.Axis == "A3" {
			t.Fatalf("A3 must NOT be an unmeasurable gap once mutations are actuated, got %+v", sc.Unmeasurable)
		}
	}
	if !strings.Contains(sc.Text(), "A3  Heal success") {
		t.Fatalf("measured A3 must render, got:\n%s", sc.Text())
	}
	// No actuated mutations ⇒ A3 stays a named gap (0/0 is not-yet-measured, never a real 0).
	sc0 := Score(db.AxisAgg{Since: time.Unix(0, 0).UTC(), Total: 8}, time.Hour)
	if sc0.HealSuccessA3 != 0 {
		t.Fatalf("A3 with no mutations must be 0 (unmeasured), got %v", sc0.HealSuccessA3)
	}
	found := false
	for _, g := range sc0.Unmeasurable {
		if g.Axis == "A3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("with no actuated mutations A3 must be a named coverage gap, got %+v", sc0.Unmeasurable)
	}
}

// TestA8SafetyMeasured: A8 is measured from the governance_ledger — breaches = deleted-audit-row gaps, plus
// guardrail enforcements — and A8 is NEVER a coverage gap (the ledger always exists), even on an empty window.
func TestA8SafetyMeasured(t *testing.T) {
	// An intact ledger (0 gaps) with safety interventions: breaches=0, trips/demotions carried; A8 renders + not a gap.
	sc := Score(db.AxisAgg{Since: time.Unix(0, 0).UTC(), Total: 20, LedgerEntries: 300, LedgerBreaches: 0, BreakerTrips: 1, Demotions: 2}, time.Hour)
	if sc.BreachesA8 != 0 || sc.BreakerTrips != 1 || sc.Demotions != 2 {
		t.Fatalf("A8: want breaches=0 trips=1 demotions=2, got %d/%d/%d", sc.BreachesA8, sc.BreakerTrips, sc.Demotions)
	}
	for _, g := range sc.Unmeasurable {
		if g.Axis == "A8" {
			t.Fatalf("A8 must NOT be an unmeasurable gap — the ledger always exists, got %+v", sc.Unmeasurable)
		}
	}
	txt := sc.Text()
	if !strings.Contains(txt, "A8  Safety-violation count") || !strings.Contains(txt, "breaches: 0") {
		t.Fatalf("measured A8 must render breaches, got:\n%s", txt)
	}
	// A tampered ledger (a deleted audit row → a seq gap) surfaces as a nonzero breach count — the whole point.
	scBreach := Score(db.AxisAgg{Since: time.Unix(0, 0).UTC(), Total: 5, LedgerEntries: 99, LedgerBreaches: 1}, time.Hour)
	if scBreach.BreachesA8 != 1 || !strings.Contains(scBreach.Text(), "1 deleted audit rows") {
		t.Fatalf("A8 breach must surface: want 1, got %d\n%s", scBreach.BreachesA8, scBreach.Text())
	}
	// An empty ledger (fresh install) is a clean measured 0, NOT an unmeasured gap.
	scEmpty := Score(db.AxisAgg{Since: time.Unix(0, 0).UTC()}, time.Hour)
	for _, g := range scEmpty.Unmeasurable {
		if g.Axis == "A8" {
			t.Fatalf("empty ledger: A8 is measured-clean (0/0), never a gap, got %+v", scEmpty.Unmeasurable)
		}
	}
}

// TestA7FalseActuation: A7 = suspicious-actor actuations / mutations (the security gate should withhold these),
// with the uncleared count as an ineffective-actuation upper bound; A7 shares A3's mutated denominator and is a
// named gap only when no mutation occurred.
func TestA7FalseActuation(t *testing.T) {
	// 4 mutations, 1 on a suspicious actor ⇒ A7 = 0.25; 3 confirmed-clear ⇒ 1 uncleared. Measured, not a gap.
	sc := Score(db.AxisAgg{Since: time.Unix(0, 0).UTC(), Total: 10, MutatedCount: 4, HealConfirmedCount: 3, SuspiciousActuations: 1}, time.Hour)
	if sc.FalseActuationA7 != 0.25 {
		t.Fatalf("A7 false-actuation: want 0.25 (1/4), got %v", sc.FalseActuationA7)
	}
	if sc.UnclearedA7 != 1 {
		t.Fatalf("A7 uncleared: want 1 (4 mutated - 3 confirmed), got %d", sc.UnclearedA7)
	}
	for _, g := range sc.Unmeasurable {
		if g.Axis == "A7" {
			t.Fatalf("A7 must NOT be a gap once a mutation exists, got %+v", sc.Unmeasurable)
		}
	}
	if !strings.Contains(sc.Text(), "A7  False-actuation rate") {
		t.Fatalf("measured A7 must render, got:\n%s", sc.Text())
	}
	// With a mutation, an injected fault AND a timed decision, EVERY axis is measured — no coverage gaps at all
	// (the milestone). DecisionN joined this list with TG-205: A6b's wall-clock leg needs a triage recorded by a
	// build carrying migration 0058, so a window of pre-0058 sessions is a named gap like any other missing input.
	scAll := Score(db.AxisAgg{Since: time.Unix(0, 0).UTC(), Total: 10, MutatedCount: 1, HealConfirmedCount: 1,
		InjectedFaults: 2, DetectedFaults: 2, DecisionN: 3, DecisionMedianMs: 9000, DecisionP95Ms: 20000}, time.Hour)
	if len(scAll.Unmeasurable) != 0 {
		t.Fatalf("with a mutation + an injected fault + a timed decision every axis measures — want 0 gaps, got %+v", scAll.Unmeasurable)
	}
	// No mutations ⇒ A7 (and A3) are named gaps, not a false 0.
	sc0 := Score(db.AxisAgg{Since: time.Unix(0, 0).UTC(), Total: 5}, time.Hour)
	if sc0.FalseActuationA7 != 0 {
		t.Fatalf("A7 with no mutations must be 0 (unmeasured), got %v", sc0.FalseActuationA7)
	}
	foundA7 := false
	for _, g := range sc0.Unmeasurable {
		if g.Axis == "A7" {
			foundA7 = true
		}
	}
	if !foundA7 {
		t.Fatalf("no mutations ⇒ A7 is a named coverage gap, got %+v", sc0.Unmeasurable)
	}
}

// TestScoreEmptyWindowNoPanic: an empty window divides by zero nowhere and renders cleanly.
func TestScoreEmptyWindowNoPanic(t *testing.T) {
	sc := Score(db.AxisAgg{Since: time.Unix(0, 0).UTC()}, time.Hour)
	if sc.AutonomyA4 != 0 || sc.OverallA2 != 0 || sc.BreadthA5 != 0 {
		t.Fatalf("empty aggregate must yield zero axes, got %+v", sc)
	}
	txt := sc.Text()
	if !strings.Contains(txt, "no judged incidents in window") {
		t.Fatalf("empty scorecard must say so, got:\n%s", txt)
	}
}

// TestScoreEmptyBlastNotRendered: a scored prediction row with NO positive forecast (tp+fp=0) must not
// render a "hit-rate 0.0% vs control 0.0%" line — 0/0 is undefined, not a computed zero (review finding).
func TestScoreEmptyBlastNotRendered(t *testing.T) {
	sc := Score(db.AxisAgg{
		Since: time.Unix(0, 0).UTC(), Total: 5, Judged: 5, Bands: map[string]int{"AUTO": 5},
		PredScored: 3, PredTP: 0, PredFP: 0, PredFN: 4, // rows exist, but the forecast named zero hosts
	}, time.Hour)
	if sc.BlastMeasurable {
		t.Fatalf("tp+fp=0 must be non-measurable, got measurable")
	}
	if strings.Contains(sc.Text(), "blast-radius prediction") {
		t.Fatalf("an undefined blast-radius must not render as 0.0%%, got:\n%s", sc.Text())
	}
}

// TestTextRendersAxisLabels: the human rendering names every measured axis and the coverage gaps, so the
// scorecard is legible without the JSON.
func TestTextRendersAxisLabels(t *testing.T) {
	sc := Score(db.AxisAgg{
		Since: time.Unix(0, 0).UTC(), Total: 10, Judged: 10, Bands: map[string]int{"AUTO": 5, "POLL_PAUSE": 5},
		Ops: []string{"restart"}, DimMeans: map[string]float64{"correct_diagnosis": 4.1}, DimN: map[string]int{"correct_diagnosis": 10},
	}, time.Hour)
	txt := sc.Text()
	for _, want := range []string{"A2", "A4", "A5", "correct_diagnosis", "Autonomy rate", "Fault-class breadth", "A1", "A8"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("rendered scorecard must contain %q, got:\n%s", want, txt)
		}
	}
}

// A6b TIME TO DECISION MUST REACH THE SCORECARD, AND AN UNMEASURED WINDOW MUST SAY SO (TG-205).
//
// docs/BENCHMARK-AXES.md defined A6 as MTTR while this command, eval/gate and session_triage.step_count all
// measured decision STEPS. The consequence was not a missing column: the scorecard silently answered a
// different question from the one the axis vocabulary asked, so "how long does TG take to decide" had no
// answer anywhere — including for TG's own measured ~39s-vs-~11min detection result, which was therefore
// unpublishable as a latency claim.
//
// Both halves are asserted here because they fail in opposite directions:
//   - a measured window must PUBLISH the percentiles (otherwise the axis is still unreported);
//   - an unmeasured window must NAME THE GAP and print no number (otherwise the 537-incident corpus recorded
//     before migration 0058 — every row decision_ms = 0 — publishes "TG decides in 0.0s", the most flattering
//     possible falsehood about this axis).
//
// KILLING MUTATION (executed): drop `DecisionMedianMsA6b: a.DecisionMedianMs, DecisionP95MsA6b: a.DecisionP95Ms,
// DecisionN: a.DecisionN` from Score(). RED — "A6b time-to-decision median/p95/n: want 12400/96500/41, got 0/0/0".
func TestA6bTimeToDecisionIsPublished(t *testing.T) {
	agg := db.AxisAgg{
		Since: time.Unix(0, 0).UTC(), Total: 57, Judged: 50,
		Bands:     map[string]int{"AUTO": 20, "POLL_PAUSE": 37},
		MeanSteps: 3.5, StepsN: 41, // A6a — the steps half, which was never the time half
		DecisionN: 41, DecisionMedianMs: 12400, DecisionP95Ms: 96500,
	}
	sc := Score(agg, 48*time.Hour)

	if sc.DecisionMedianMsA6b != 12400 || sc.DecisionP95MsA6b != 96500 || sc.DecisionN != 41 {
		t.Fatalf("A6b time-to-decision median/p95/n: want 12400/96500/41, got %d/%d/%d — the aggregate measured "+
			"the wall-clock and the scorecard dropped it, which is the TG-205 defect one layer up",
			sc.DecisionMedianMsA6b, sc.DecisionP95MsA6b, sc.DecisionN)
	}
	txt := sc.Text()
	// Rendered in SECONDS for a human: 12400ms ⇒ 12.4s, 96500ms ⇒ 96.5s.
	for _, want := range []string{"A6b Time to decision", "median 12.4s", "p95 96.5s", "n=41"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("rendered scorecard must contain %q, got:\n%s", want, txt)
		}
	}
	// A6a and A6b are DIFFERENT axes and both must appear: publishing steps alone IS the drift TG-205 closes.
	if !strings.Contains(txt, "A6a Decision efficiency") {
		t.Fatalf("the steps half (A6a) vanished from the scorecard — the split must keep both, got:\n%s", txt)
	}
	// A measured window must not also be declared a coverage gap.
	for _, g := range sc.Unmeasurable {
		if g.Axis == "A6b" {
			t.Fatalf("a window with 41 measured decisions was declared an A6b coverage gap: %+v", g)
		}
	}
}

// The mirror: nothing measured ⇒ a NAMED gap and no number. This is the half that protects the historical
// corpus, every row of which carries decision_ms = 0 because it was recorded before the column existed. A
// median over that population is 0, and 0 here does not mean "instant" — it means "unmeasured".
//
// KILLING MUTATION (executed): delete the `if sc.DecisionN == 0` gap append in Score(). RED — "an unmeasured
// A6b must be a NAMED coverage gap".
func TestA6bUnmeasuredWindowNamesTheGapInsteadOfPublishingZero(t *testing.T) {
	sc := Score(db.AxisAgg{
		Since: time.Unix(0, 0).UTC(), Total: 537, Judged: 500,
		Bands: map[string]int{"POLL_PAUSE": 537}, MeanSteps: 4.0, StepsN: 537,
	}, 168*time.Hour)

	named := false
	for _, g := range sc.Unmeasurable {
		if g.Axis == "A6b" {
			named = true
			if g.Missing == "" {
				t.Errorf("the A6b gap names no missing input: %+v", g)
			}
		}
	}
	if !named {
		t.Errorf("an unmeasured A6b must be a NAMED coverage gap — 537 pre-migration rows all read decision_ms=0, "+
			"and publishing their median would state that TG decides instantly. Gaps: %+v", sc.Unmeasurable)
	}
	txt := sc.Text()
	if strings.Contains(txt, "median 0.0s") {
		t.Errorf("an unmeasured A6b rendered a 0.0s median — absent is not zero, got:\n%s", txt)
	}
	if !strings.Contains(txt, "migration 0058") {
		t.Errorf("the unmeasured branch must say WHY there is no number (which migration populates it), got:\n%s", txt)
	}
}
