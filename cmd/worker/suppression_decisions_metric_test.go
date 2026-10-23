package main

import (
	"strings"
	"testing"
	"time"
)

// TG-380 (suppression half). TG exports 128 tg_* families and a substring probe over all of them finds
// NOTHING for suppress / dedup / correl / cascade / blast / storm / escalat / band. Alerts arriving and
// actions leaving are visible; everything the system DECIDES between them is not.
//
// The counter was never missing — it was UNREACHABLE. LiveSuppressGate.Counts() has tallied every
// outcome all along and modules/telemetry.SuppressionSamples renders it, but only from inside the
// observability export loop, which needs TG_OBSERVABILITY_EXPORT_INTERVAL (measured live: EMPTY) AND an
// enabled exporter ("no trace-capable exporter configured"). Zero tg_suppression* series exist in
// Prometheus as a result.

func suppSamplesByName(t *testing.T, counts map[string]int) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, s := range suppressionDecisionSamples(counts, time.Now()) {
		key := s.Name
		if o, ok := s.Labels["outcome"]; ok {
			key = s.Name + "{" + o + "}"
		}
		out[key] = s.Value
	}
	return out
}

// TestTheTotalIsEmittedEvenWithNoDecisions is the vacuity floor and the finding at once: a family that
// appears only once a decision is made makes "decided nothing yet" and "not wired" one observation.
func TestTheTotalIsEmittedEvenWithNoDecisions(t *testing.T) {
	got := suppSamplesByName(t, map[string]int{})
	if _, ok := got["tg_suppression_decisions_total"]; !ok {
		t.Fatal("no total series on an empty tally. The gate deciding nothing yet and the gate not " +
			"being wired would then be the same observation — which is the whole defect class this " +
			"ticket is an instance of.")
	}
	if got["tg_suppression_decisions_total"] != 0 {
		t.Errorf("empty tally reported %v decisions", got["tg_suppression_decisions_total"])
	}
}

// TestTheOutcomeBreakdownSeparatesDeadFromQuiet is why the per-outcome label exists. During the
// 2026-08-06 cascade the chain was offered 171 alerts and suppressed 0 (TG-377) — a broken stage and a
// chain that correctly found nothing produce the SAME suppressed count.
func TestTheOutcomeBreakdownSeparatesDeadFromQuiet(t *testing.T) {
	// A running chain that suppressed nothing: escalate is climbing.
	running := suppSamplesByName(t, map[string]int{"escalate": 171})
	// A dead chain: nothing at all.
	dead := suppSamplesByName(t, map[string]int{})

	if running["tg_suppression_decisions_total"] == dead["tg_suppression_decisions_total"] {
		t.Fatal("a chain that escalated 171 alerts and a chain that decided nothing report the same " +
			"total — the denominator is what separates 'nothing to suppress' from 'the stage is dead'")
	}
	if running["tg_suppression_decisions_by_outcome_total{escalate}"] != 171 {
		t.Errorf("escalate outcome = %v, want 171",
			running["tg_suppression_decisions_by_outcome_total{escalate}"])
	}
	// Both report zero SUPPRESSED, which is exactly the ambiguity the total resolves.
	if running["tg_suppression_decisions_by_outcome_total{suppressed}"] != 0 {
		t.Error("fixture is wrong: the running chain must report zero suppressed, that is the point")
	}
}

// TestOutcomesAreSortedForAStableScrape. An unsorted map iteration reorders the exposition on every
// scrape, which shows up as spurious churn in diffs and breaks byte-comparison of the endpoint.
func TestOutcomesAreSortedForAStableScrape(t *testing.T) {
	s := suppressionDecisionSamples(map[string]int{"z": 1, "a": 2, "m": 3}, time.Now())
	var seen []string
	for _, x := range s {
		if o, ok := x.Labels["outcome"]; ok {
			seen = append(seen, o)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("got %d outcome series, want 3", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i-1] > seen[i] {
			t.Fatalf("outcomes are not sorted: %v", seen)
		}
	}
}

// TestTheDecisionSamplerIsWiredAtTheCompositionRoot. The sampler existing and /metrics publishing it are
// different facts — and the ONE that failed here was publication, for the identical metric, for months.
func TestTheDecisionSamplerIsWiredAtTheCompositionRoot(t *testing.T) {
	src := stripGoComments(readWorkerMain(t))
	if len(src) < 10_000 {
		t.Fatalf("VACUITY FLOOR: main.go stripped to %d bytes", len(src))
	}
	if !strings.Contains(src, "withSuppressionDecisions(") {
		t.Fatal("the worker no longer registers withSuppressionDecisions. The sampler would exist and " +
			"emit nothing — which is precisely the state this ticket found: the metric was written, " +
			"rendered, and reachable only from an export loop that is unconfigured in production.")
	}
	if !strings.Contains(src, "suppressionDecisionSamples(g.Counts()") {
		t.Error("the registered closure does not read the gate's own Counts() — a decision series fed " +
			"from anywhere else would not be the gate's decisions")
	}
	// It must NOT emit when the gate is unarmed: zeros would assert a gate that is not there.
	k := strings.Index(src, "withSuppressionDecisions(")
	if k >= 0 && !strings.Contains(src[k:min(k+400, len(src))], "return nil") {
		t.Error("the closure does not return nil for an unarmed gate — publishing zeros before the gate " +
			"exists asserts a wired-and-quiet chain that has not been built yet")
	}
}
