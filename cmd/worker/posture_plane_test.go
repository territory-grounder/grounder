package main

// THE POSTURE SERIES MUST SAY WHICH PROCESS THEY DESCRIBE (TG-112).
//
// Measured on the running deployment 2026-08-06, before this change:
//
//	mutation_enabled{component="worker", instance="worker:8444"}          1
//	mutation_enabled{component="worker", instance="worker-actuate:8445"}  1
//	mutation_enabled{component="grounder", instance="grounder:8080"}      0
//
// Both worker processes claimed component="worker". One of them is the ONLY process that can mutate the
// estate; the other holds no actuation credential at all. The label whose entire purpose is naming the
// component named the wrong thing on the one process where the distinction matters, and only job/instance
// separated them — which a rule written on component alone does not see.

import (
	"testing"

	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/safety"
)

func postureSample(ss []metrics.Sample, name string) (metrics.Sample, bool) {
	for _, s := range ss {
		if s.Name == name {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

func adminOnPlane(plane string) *workerAdmin {
	// NewActuatingChokepoint, not NewChokepoint(FixedModeAuthority(true)): MayActuate requires an
	// actuating mode AND a GREEN PREFLIGHT, so the latter yields 0 and every "the two series agree"
	// assertion becomes 0 == 0. The drift mutation survived exactly that, which is why the test now has a
	// floor asserting the fixture is non-zero before it compares anything.
	return newWorkerAdmin(safety.NewActuatingChokepoint(), nil, nil, nil, "").withPlane(plane)
}

// The two planes must be distinguishable from the LABELS alone. A rule aggregating on component would
// otherwise fold the process that can mutate the estate together with the one that cannot.
func TestTheTwoWorkerPlanesAreDistinguishableFromTheirLabels(t *testing.T) {
	tri, _ := postureSample(adminOnPlane("triage").samples(), "tg_may_actuate")
	act, _ := postureSample(adminOnPlane("actuation").samples(), "tg_may_actuate")

	if tri.Labels["plane"] == act.Labels["plane"] {
		t.Fatalf("both planes publish plane=%q. Two processes with opposite actuation authority are then "+
			"one series to any rule that does not happen to join on instance — which is the state that "+
			"made the pre-TG-112 posture binary return two indistinguishable component=\"worker\" series "+
			"in production.",
			tri.Labels["plane"])
	}
	if tri.Labels["plane"] == "" || act.Labels["plane"] == "" {
		t.Error("a posture sample carries an EMPTY plane label; an unlabelled series is worse than an " +
			"honestly-unknown one because it silently matches every selector that omits the label")
	}
}

// An unknown plane must render as unknown, never be guessed. Guessing "triage" would be the reassuring
// direction and would hide a worker whose plane was never configured.
func TestAnUnsetPlaneIsReportedUnknownRatherThanGuessed(t *testing.T) {
	s, ok := postureSample(adminOnPlane("").samples(), "tg_may_actuate")
	if !ok {
		t.Fatal("no posture sample at all")
	}
	if got := s.Labels["plane"]; got != "unknown" {
		t.Errorf("an unset plane rendered as %q. Anything other than 'unknown' is an invented fact about "+
			"which process this is, and the safe-looking guess is the dangerous one.", got)
	}
}

// THE CURRENT NAME IS PUBLISHED AND THE RETIRED ALIAS IS NOT (TG-112, deprecation window CLOSED). Every
// consumer — alert.rules.yml, safety.json, the console, shadowbench — joins on tg_may_actuate /
// tg_policy_mode now, so reintroducing the old-name series is the killing mutation here: two names for one
// read is how the two drift, and no rule carries the old-name OR-leg any more.
func TestTheRetiredAliasIsNotPublished(t *testing.T) {
	ss := adminOnPlane("actuation").samples()
	may, ok := postureSample(ss, "tg_may_actuate")
	if !ok {
		t.Fatal("tg_may_actuate is not published. TG-112's whole point is that 'may actuate' is DERIVED " +
			"from the 4-mode chokepoint, and until the derived signal is emitted no rule can see this " +
			"process's posture at all.")
	}
	// VACUITY FLOOR: the fixture must actually permit actuation, or every posture assertion in this file
	// is comparing zeroes (the drift mutation survived exactly that once).
	if may.Value == 0 {
		t.Fatalf("the fixture produced tg_may_actuate=0 — construct an admin whose gate actually permits " +
			"actuation before asserting anything about the posture series.")
	}
	if _, present := postureSample(ss, "mutation"+"_enabled"); present {
		t.Error("the RETIRED mutation-binary alias is being published again (TG-112). The deprecation " +
			"window closed when alert.rules.yml/safety.json/the console migrated to tg_may_actuate; a " +
			"revived alias has no consumer and can only drift from the real signal.")
	}
}

// The mode context must be READABLE where an operator meets the posture gauge: tg_may_actuate's help must
// point at tg_policy_mode (the owner-set mode it is derived FROM). Help is what a dashboard and a
// `curl /metrics` show.
func TestTheMayActuateHelpPointsAtTheModeGauge(t *testing.T) {
	may, _ := postureSample(adminOnPlane("triage").samples(), "tg_may_actuate")
	if !containsFold(may.Help, "tg_policy_mode") {
		t.Errorf("tg_may_actuate's help does not point at tg_policy_mode: %q\nWhoever reads it next has "+
			"no pointer from the derived signal to the owner-set mode it derives from.", may.Help)
	}
}

func containsFold(hay, needle string) bool {
	h, n := []rune(hay), []rune(needle)
	lower := func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}
	for i := 0; i+len(n) <= len(h); i++ {
		ok := true
		for j := range n {
			if lower(h[i+j]) != lower(n[j]) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
