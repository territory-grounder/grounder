package main

import (
	"sort"
	"time"

	"github.com/territory-grounder/grounder/core/metrics"
)

// THE DECISION PLANE HAD NO SERIES IN PROMETHEUS (TG-380, suppression half).
//
// TG exports 128 tg_* families and a substring probe over all of them finds NOTHING for suppress,
// dedup, correl, cascade, blast, storm, escalat or band. Alerts arriving and actions leaving are both
// visible; everything the system DECIDES in between is not.
//
// And the suppression counter is not missing — it is UNREACHABLE. LiveSuppressGate.Counts() has tallied
// every decision outcome all along, and modules/telemetry.SuppressionSamples already renders it as
// `tg_suppression_decisions`. That rendering is reached only from the observability export loop, nested:
//
//	if TG_OBSERVABILITY_EXPORT_INTERVAL != ""    <- measured live: EMPTY on dc1tg01
//	  if len(exporters) > 0                      <- "no trace-capable exporter configured"
//	    for range t.C
//
// so on this deployment nothing has ever emitted it. Confirmed: zero `tg_suppression*` series exist in
// Prometheus. The same nesting already stranded the wiring observation, and the comment at the
// observeSuppressionYield hoist says so — this is the metric half of the identical defect.
//
// Published here on the worker's OWN /metrics, which Prometheus actually scrapes, exactly as the estate
// size and pve-liveness registers are. The export loop keeps working when configured; this does not
// replace it, it stops the series depending on it.
//
// WHY IT MATTERS RATHER THAN BEING TIDY: during the 2026-08-06 pve03 cascade the tier-1 chain was
// offered 171 alerts and suppressed 0 (TG-377). A zero from a broken stage and a zero from a chain that
// correctly found nothing to suppress are the same number — and with no series at all, neither was
// visible in real time. The per-OUTCOME breakdown is what separates them: a chain that is running
// reports escalate counts climbing, a chain that is dead reports nothing at all.
func suppressionDecisionSamples(counts map[string]int, now time.Time) []metrics.Sample {
	// ALWAYS EMIT THE TOTAL, including zero. A family that appears only once a decision is made makes
	// "the gate decided nothing yet" and "the gate is not wired" one observation — which is the defect
	// class this whole ticket is an instance of.
	var total int
	outcomes := make([]string, 0, len(counts))
	for k, v := range counts {
		outcomes = append(outcomes, k)
		total += v
	}
	sort.Strings(outcomes) // byte-stable scrape

	out := make([]metrics.Sample, 0, len(outcomes)+1)
	out = append(out, metrics.Sample{
		Name: "tg_suppression_decisions_total", Kind: metrics.Counter,
		Help: "tier-1 suppression decisions the gate has made, all outcomes. The DENOMINATOR: a zero " +
			"suppressed count means nothing without it — during the 2026-08-06 cascade the chain was " +
			"offered 171 alerts and suppressed 0, and a broken stage and a chain with nothing to " +
			"suppress produce the same number.",
		Value: float64(total),
	})
	for _, o := range outcomes {
		out = append(out, metrics.Sample{
			Name: "tg_suppression_decisions_by_outcome_total", Kind: metrics.Counter,
			Help: "tier-1 suppression decisions by OUTCOME. Read against the total: a chain that is " +
				"running reports escalate climbing; a chain that is dead reports nothing at all. Until " +
				"now this existed only behind TG_OBSERVABILITY_EXPORT_INTERVAL, which is empty in " +
				"production, so no series was ever emitted.",
			Value:  float64(counts[o]),
			Labels: map[string]string{"outcome": o},
		})
	}
	return out
}
