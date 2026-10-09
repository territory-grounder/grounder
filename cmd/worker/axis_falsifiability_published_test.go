package main

import (
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
)

// TG-192, and REQ-2524 applied to the axis it was never applied to.
//
// REQ-2524: "a measurement reachable only by a human running a command is not a measurement of a running
// system." It was written about axis A1 — whose only caller was cmd/axisscore — and fixed for A1; nine
// tg_axis_* series ship today. FALSIFIABILITY, which TG-192 exists to surface as a first-class published
// claim, was never added. Measured live 2026-08-07: 9 published tg_axis_* series, NONE of them this one,
// while the data sat in infragraph_prediction (2048 rows, 355 with a stored shuffled control, real TP 130
// against control TP 74).
//
// The property is not "a rate is published". It is that the rate ships WITH the denominators that make it
// honest — see core/db.Falsifiability, whose own comment records that a naive count publishes 87% PASS of
// which 82% is empty-vs-empty, against 61% over windows where a claim was actually made.

func falsSamples(t *testing.T, f db.Falsifiability) map[string]float64 {
	t.Helper()
	s := newAxisSampler(24 * time.Hour)
	s.agg, s.sampled, s.at = db.AxisAgg{}, true, time.Now()
	s.fals, s.falsSample = f, true
	out := map[string]float64{}
	for _, x := range s.Collect() {
		out[x.Name] = x.Value
	}
	if len(out) < 3 {
		t.Fatalf("VACUITY FLOOR: the sampler emitted %d sample(s); every assertion below would pass on an "+
			"empty set", len(out))
	}
	return out
}

// TestFalsifiabilityIsPublishedAtAll is the finding.
func TestFalsifiabilityIsPublishedAtAll(t *testing.T) {
	got := falsSamples(t, db.Falsifiability{Windows: 173, NoClaim: 129, Claimed: 44, ClaimedPassed: 27, Passed: 150, RealTP: 130, ControlTP: 74})

	if _, ok := got["tg_axis_falsifiability_claimed_passed_total"]; !ok {
		t.Fatal("the falsifiability axis is not published. It is TG's rarest asset — a degree-preserving " +
			"shuffled-graph control run against every prediction — and it was reachable only by running " +
			"cmd/axisscore by hand, which REQ-2524 states is not a measurement of a running system.")
	}
}

// TestTheHonestDenominatorsShipWithTheRate is the property that actually matters. Publishing the pass
// count without Claimed and NoClaim exports the 87% artifact as if it were the measurement.
func TestTheHonestDenominatorsShipWithTheRate(t *testing.T) {
	// The live shape, 2026-08-06: 173 windows, 129 with no claim, 44 claimed, 27 of those passed.
	got := falsSamples(t, db.Falsifiability{Windows: 173, NoClaim: 129, Claimed: 44, ClaimedPassed: 27, Passed: 150, RealTP: 130, ControlTP: 74})

	for name, want := range map[string]float64{
		"tg_axis_falsifiability_windows_total":        173,
		"tg_axis_falsifiability_claimed_total":        44,
		"tg_axis_falsifiability_noclaim_total":        129,
		"tg_axis_falsifiability_claimed_passed_total": 27,
		"tg_axis_falsifiability_passed_naive_total":   150,
	} {
		if got[name] != want {
			t.Errorf("%s = %v, want %v", name, got[name], want)
		}
	}
	// The arithmetic the denominators exist to expose: naive 150/173 = 87%, honest 27/44 = 61%.
	naive := got["tg_axis_falsifiability_passed_naive_total"] / got["tg_axis_falsifiability_windows_total"]
	honest := got["tg_axis_falsifiability_claimed_passed_total"] / got["tg_axis_falsifiability_claimed_total"]
	if !(naive > honest) {
		t.Fatalf("fixture does not reproduce the overstatement (naive %.2f vs honest %.2f) — the whole "+
			"reason these fields ship separately", naive, honest)
	}
	if got["tg_axis_falsifiability_noclaim_total"] == 0 {
		t.Error("NoClaim is published as 0 on a fixture where 129 windows made no claim — a consumer would " +
			"read the naive rate as a measurement with nothing to correct it")
	}
}

// TestTheControlArmIsPublishedBesideTheRealArm. "The real graph beat its control" is only meaningful with
// both numbers; publishing RealTP alone lets a reader assume a comparison that was never shown.
func TestTheControlArmIsPublishedBesideTheRealArm(t *testing.T) {
	got := falsSamples(t, db.Falsifiability{Windows: 10, Claimed: 10, ClaimedPassed: 8, RealTP: 130, ControlTP: 74})
	if got["tg_axis_falsifiability_real_tp_total"] != 130 || got["tg_axis_falsifiability_control_tp_total"] != 74 {
		t.Errorf("real/control arms = %v/%v, want 130/74 — the claim is 'the real graph beats its shuffled "+
			"control', which is unreadable without both", got["tg_axis_falsifiability_real_tp_total"],
			got["tg_axis_falsifiability_control_tp_total"])
	}
}

// TestNothingIsPublishedBeforeTheFirstSuccessfulRead. A 0.0 falsifiability rate is the most damaging false
// statement available about this system — it says the topology carries no information.
func TestNothingIsPublishedBeforeTheFirstSuccessfulRead(t *testing.T) {
	s := newAxisSampler(24 * time.Hour)
	s.agg, s.sampled, s.at = db.AxisAgg{}, true, time.Now() // A1 sampled, falsifiability NOT
	for _, x := range s.Collect() {
		if strings.Contains(x.Name, "falsifiability") {
			t.Fatalf("%s was emitted before any successful falsifiability read (value %v). Zeros here read "+
				"as 'the real graph never beats its shuffled control', i.e. the estate topology carries no "+
				"information — the most alarming possible false statement about a healthy system.",
				x.Name, x.Value)
		}
	}
	// And the sibling axes must still publish — one failing read must not silence the other.
	var sawA1 bool
	for _, x := range s.Collect() {
		if x.Name == "tg_axis_faults_injected_total" {
			sawA1 = true
		}
	}
	if !sawA1 {
		t.Error("an unsampled falsifiability axis suppressed the A1 series — the two reads fail independently")
	}
}
