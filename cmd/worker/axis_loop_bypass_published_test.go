package main

import (
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
)

// TG-191 (epic TG-187) — REQ-2524 applied to the loop-bypass guardrail, exactly as TG-192 applied it to
// falsifiability.
//
// The G6 loop-bypass axis shipped first with ONE caller: cmd/axisscore. REQ-2524: "a measurement reachable
// only by a human running a command is not a measurement of a running system." A guardrail whose whole point
// is to be a CONTINUOUS audit ("don't erode the core", auditable not aspirational) is worth least in a CLI
// nobody runs. This publishes it as tg_axis_loop_bypass_* so a dashboard/alert can watch bypassing==0.
//
// The property is not merely "a total is published". It is that the total ships WITH the audited population
// (executed) that makes bypassing==0 distinguishable from "nothing to audit", and split into its two limbs.

func lbSamples(t *testing.T, lb db.LoopBypass) map[string]float64 {
	t.Helper()
	s := newAxisSampler(24 * time.Hour)
	s.agg, s.sampled, s.at = db.AxisAgg{}, true, time.Now()
	s.lb, s.lbSample = lb, true
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

// TestLoopBypassIsPublishedAtAll is the finding: the guardrail must reach /metrics, not only cmd/axisscore.
func TestLoopBypassIsPublishedAtAll(t *testing.T) {
	got := lbSamples(t, db.LoopBypass{Executed: 40, Bypassing: 0, NoPrediction: 0, NoVerdict: 0})
	if _, ok := got["tg_axis_loop_bypass_total"]; !ok {
		t.Fatal("the loop-bypass guardrail is not published. It is the anti-drift tripwire (TG-191) and was " +
			"reachable only by running cmd/axisscore by hand, which REQ-2524 states is not a measurement of a " +
			"running system.")
	}
}

// TestThePopulationShipsWithTheGuardrail is the property that matters: bypassing==0 over executed==0 is
// "nothing to audit", not a clean pass, and a consumer can only tell them apart if BOTH ship. This also
// pins the two-limb split so a dashboard names WHICH way a heal skipped the loop.
func TestThePopulationShipsWithTheGuardrail(t *testing.T) {
	// A window with real drift: 40 executed, 3 bypassing (2 un-predicted, 1 ungraded).
	got := lbSamples(t, db.LoopBypass{Executed: 40, Bypassing: 3, NoPrediction: 2, NoVerdict: 1})
	for name, want := range map[string]float64{
		"tg_axis_loop_bypass_executed_total":      40,
		"tg_axis_loop_bypass_total":               3,
		"tg_axis_loop_bypass_no_prediction_total": 2,
		"tg_axis_loop_bypass_no_verdict_total":    1,
	} {
		if got[name] != want {
			t.Errorf("%s = %v, want %v", name, got[name], want)
		}
	}
	// The distinction the population denominator exists to preserve: a guardrail of 0 is only good news when
	// something was actually audited. Publishing bypassing without executed would erase that.
	if _, ok := got["tg_axis_loop_bypass_executed_total"]; !ok {
		t.Fatal("tg_axis_loop_bypass_total shipped without tg_axis_loop_bypass_executed_total — a 0 guardrail " +
			"over an unknown population reads as 'audited, no drift' when it may be 'nothing was audited'")
	}
	// The two limbs must sum into the total's meaning: each is a distinct way to skip the loop.
	if got["tg_axis_loop_bypass_no_prediction_total"]+got["tg_axis_loop_bypass_no_verdict_total"] < got["tg_axis_loop_bypass_total"] {
		t.Errorf("the limbs (no_prediction=%v, no_verdict=%v) under-account the total (%v) — a bypassing heal "+
			"must be attributable to at least one named limb", got["tg_axis_loop_bypass_no_prediction_total"],
			got["tg_axis_loop_bypass_no_verdict_total"], got["tg_axis_loop_bypass_total"])
	}
}

// TestNothingIsPublishedBeforeTheFirstSuccessfulLoopBypassRead — the sampler's absent-is-not-zero rule
// applied to this axis. A 0 guardrail before any read would read as "audited, no drift" when nothing was
// audited at all; the siblings must still publish (the reads fail independently).
func TestNothingIsPublishedBeforeTheFirstSuccessfulLoopBypassRead(t *testing.T) {
	s := newAxisSampler(24 * time.Hour)
	s.agg, s.sampled, s.at = db.AxisAgg{}, true, time.Now() // A1 sampled, loop-bypass NOT
	for _, x := range s.Collect() {
		if strings.Contains(x.Name, "loop_bypass") {
			t.Fatalf("%s was emitted before any successful loop-bypass read (value %v). A 0 here reads as "+
				"'every executed heal traversed the loop' when the guardrail was never actually evaluated — "+
				"absent is not a pass.", x.Name, x.Value)
		}
	}
	var sawA1 bool
	for _, x := range s.Collect() {
		if x.Name == "tg_axis_faults_injected_total" {
			sawA1 = true
		}
	}
	if !sawA1 {
		t.Error("an unsampled loop-bypass axis suppressed the A1 series — the two reads must fail independently")
	}
}
