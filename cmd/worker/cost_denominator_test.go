package main

// THE SPEND GUARD PUBLISHED NOTHING WHEN IT WAS OFF.
//
// core/cost/ is a complete implementation — daily and session ceilings, per-model rates, a shared
// cross-worker trip state so a breach in one worker force-Shadows its siblings, a hash-chained ledger note.
// Production runs it with every TG_COST_* set to 0, and the worker says so honestly at boot:
//
//	cost breaker: no TG_COST_* rate/budget configured — gateway left un-wrapped (cost tracking DISABLED)
//
// That line is true and nothing reads it. Every cost gauge sat inside `if a.cost != nil`, so the disabled
// posture emitted NO SERIES AT ALL. Measured against Prometheus on dc1tg01, 2026-08-06:
//
//	tg_cost_breaker_state    -> 0 series
//	tg_cost_usd_today        -> 0 series
//	tg_cost_spend_usd_total  -> 0 series
//
// while tg_model_tokens_total accounted 3.18M tokens across three tiers. Not "zero" — ABSENT. An operator
// looking at a dashboard sees no cost panel, which renders identically to a healthy one, and `absent()` is
// the only rule that could ever have raised it. Nobody wrote one, because there was no series to name.
//
// Three postures collapse into one reading without a denominator, which is why there are two gauges here and
// not one.

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/cost"
	"github.com/territory-grounder/grounder/core/metrics"
)

// costGauge finds a sample by name, reporting presence separately from value — the whole point here is that
// absent and zero are different answers.
func costGauge(ss []metrics.Sample, name string) (float64, bool) {
	for _, s := range ss {
		if s.Name == name {
			return s.Value, true
		}
	}
	return 0, false
}

// KILLING MUTATION: move the two gauges back inside `if a.cost != nil`. RED — the unconfigured deployment,
// which is exactly the one an operator cannot see, publishes nothing again.
func TestTheCostPostureIsPublishedEvenWithNothingConfigured(t *testing.T) {
	// The live production configuration: every TG_COST_* is 0, so Enabled() is false and there is no
	// accountant at all.
	a := newWorkerAdmin(nil, nil, nil, nil, "").withCostConfig(cost.Config{})
	ss := a.samples()

	metering, ok := costGauge(ss, "tg_cost_metering")
	if !ok {
		t.Fatal("tg_cost_metering is ABSENT on an unconfigured worker — the posture that most needs " +
			"publishing is the one that publishes nothing, and an absent series cannot be alerted on by " +
			"anything except a rule nobody writes for a metric that does not exist")
	}
	if metering != 0 {
		t.Errorf("tg_cost_metering = %v with no TG_COST_* set, want 0", metering)
	}
	enforcing, ok := costGauge(ss, "tg_cost_enforcing")
	if !ok {
		t.Fatal("tg_cost_enforcing is ABSENT on an unconfigured worker")
	}
	if enforcing != 0 {
		t.Errorf("tg_cost_enforcing = %v with no ceiling armed, want 0", enforcing)
	}
}

// THE THREE POSTURES MUST BE DISTINGUISHABLE. tg_cost_breaker_state = 0 means "closed" in all three, so the
// state gauge alone cannot separate "within budget" from "no budget exists".
//
// KILLING MUTATION: derive tg_cost_enforcing from Enabled() instead of Enforces(). RED on the middle row —
// rates configured with no ceiling would claim the breaker can trip when nothing can ever open it.
func TestMeteringAndEnforcingSeparateTheThreePostures(t *testing.T) {
	for _, c := range []struct {
		label               string
		cfg                 cost.Config
		metering, enforcing float64
	}{
		{"nothing configured — gateway not even wrapped", cost.Config{}, 0, 0},
		{"rates only — accrues, but the breaker CANNOT trip", cost.Config{DefaultRate: 0.002}, 1, 0},
		{"daily budget armed", cost.Config{DefaultRate: 0.002, DailyBudgetUSD: 25}, 1, 1},
		{"session ceiling armed, no daily", cost.Config{SessionCeilingUSD: 1.5}, 1, 1},
	} {
		ss := newWorkerAdmin(nil, nil, nil, nil, "").withCostConfig(c.cfg).samples()
		m, mok := costGauge(ss, "tg_cost_metering")
		e, eok := costGauge(ss, "tg_cost_enforcing")
		if !mok || !eok {
			t.Fatalf("%s: a posture gauge is absent (metering=%v enforcing=%v)", c.label, mok, eok)
		}
		if m != c.metering || e != c.enforcing {
			t.Errorf("%s: metering=%v enforcing=%v, want %v/%v", c.label, m, e, c.metering, c.enforcing)
		}
	}
}

// The help text has to carry the reading rule, because the number alone is misleading in exactly the case
// this whole change is about.
//
// KILLING MUTATION: replace the help with a bare restatement of the metric name. RED.
func TestTheEnforcingHelpSaysHowToReadTheBreakerStateBesideIt(t *testing.T) {
	ss := newWorkerAdmin(nil, nil, nil, nil, "").withCostConfig(cost.Config{}).samples()
	var help string
	for _, s := range ss {
		if s.Name == "tg_cost_enforcing" {
			help = s.Help
		}
	}
	if help == "" {
		t.Fatal("tg_cost_enforcing has no help text")
	}
	for _, want := range []string{"tg_cost_breaker_state", "meter-only", "ALWAYS emitted"} {
		if !strings.Contains(help, want) {
			t.Errorf("the help does not mention %q, so a reader sees a 0 and cannot tell that nothing can "+
				"open the breaker.\nGot: %s", want, help)
		}
	}
}
