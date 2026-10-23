package main

// THE MUTATION GATE'S INPUT, MEASURED (TG-343).
//
// cmd/worker/credential_plane.go states the rule the whole actuation plane rests on:
//
//	"a mutation gate reasoning over an empty graph is a gate that cannot refuse"
//
// The interceptor's host-match and blast-radius checks are evaluated against the estate graph. Counted
// 2026-08-06 on the running deployment: no tg_* series on either plane reports that graph's size. The one
// input the mutation gate cannot function without had no gauge at all.
//
// WHY THE ABSENCE MATTERS MORE HERE THAN FOR AN ORDINARY METRIC. Every other way of noticing is an ERROR,
// and the failure mode is the absence of one:
//
//   - a credential that is DENIED logs `estate: source librenms failed to seed: ... 403`. Loud. TG-331 was
//     found that way.
//   - a credential that RESOLVES and returns an empty result logs nothing. The refresh succeeds, the seam
//     reports live, the module self-test passes, and the graph is empty.
//
// The second case is not hypothetical: it is what the first version of TG-337's scoped LibreNMS role did.
// It returned 200 with zero devices, because LibreNMS filters the device list by per-device visibility
// separately from the route policy. Every signal available said healthy. Probing the token by hand and
// reading the count is what caught it.
//
// So the gates that depend on the graph cannot distinguish "no host matched, refuse" from "there were no
// hosts to match". Those are opposite facts with the same observable behaviour, and in the second one the
// gate has silently stopped being able to refuse anything.

import (
	"sort"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/metrics"
)

// estateGraphSamples publishes the size of the graph the mutation gate reasons over.
//
// ALWAYS EMITTED, INCLUDING AT ZERO. A gauge that appears only when the graph is non-empty makes "empty"
// and "the exporter stopped" identical, which is precisely the confusion this closes. Same discipline as
// tg_ingest_sources_known (TG-336) and tg_poll_queue_open (TG-173).
func estateGraphSamples(g *estate.Graph, failedSources int, now time.Time) []metrics.Sample {
	edges, nodes := 0, 0
	// WHAT the graph is made of, not just how big it is (TG-352). Measured on the live estate 2026-08-06:
	// 1,524 of 1,864 edges (82%) are incident-LEARNED rather than topology, and 1,481 of those (97.2%) sit
	// at EXACTLY estate.LearnedConfidenceCap. That saturation is why TG_PREDICT_MIN_CONFIDENCE cannot
	// discriminate: at 0.70 it removes 43 edges, and any value above the cap removes all 1,524. The knob has
	// two reachable outcomes, and the size gauge alone shows neither of them.
	bySource := map[string]int{}
	atCap := 0
	unknownTriples := 0 // TG-207: distinct undeclared (FromType,Rel,ToType) shapes the observe-only schema saw
	if g != nil {
		edges = g.Len()
		snap := g.Export()
		// tg_estate_nodes counts the LIVE node set — entities with at least one FRESH edge — not
		// len(snap.Nodes), which counts every entity that was ever an endpoint (Export never drops an expired
		// edge's endpoints). Off Export the gauge would hold high during exactly the source-goes-quiet
		// degradation it should reveal, while the graph the gate reasons over shrinks (TG-449). The snapshot
		// is still used below for the per-edge composition breakdown.
		nodes = g.FreshNodeCount()
		for _, e := range snap.Edges {
			bySource[e.Source]++
			// Ground-truth topology is graded ABOVE the cap (pve 0.95, netbox/librenms 0.90, declared 0.85),
			// so equality with the cap identifies a saturated LEARNED edge without needing the source name —
			// and comparing to the named constant is what keeps this from drifting if the cap ever moves.
			if e.Confidence == estate.LearnedConfidenceCap {
				atCap++
			}
		}
		// nil-safe: Schema() is nil on a graph built without WithDefaultEdgeSchema, and UnknownCount nil-guards
		// to 0 — so a worker on an older build that never wired the validator reads a HONEST 0, not a panic.
		unknownTriples = g.Schema().UnknownCount()
	}
	out := []metrics.Sample{
		{
			Name: "tg_estate_edges", Kind: metrics.Gauge,
			Help: "edges in the estate graph the mutation gate's host-match and blast-radius checks are " +
				"evaluated against. ZERO means the gate cannot refuse anything — it is not 'no impact " +
				"found', it is 'there was nothing to find impact in'. Always emitted, so an ABSENT series " +
				"is the worker being gone rather than the estate being empty.",
			Value: float64(edges),
		},
		{
			Name: "tg_estate_nodes", Kind: metrics.Gauge,
			Help: "distinct entities on at least one FRESH edge — the live node set the mutation gate reasons " +
				"over, not every entity ever seen (an expired edge's endpoints are excluded; TG-449). Read " +
				"beside tg_estate_edges: a graph with nodes and no edges is a seed that produced inventory but " +
				"no relationships, which blast-radius cannot walk.",
			Value: float64(nodes),
		},
		{
			Name: "tg_estate_sources_failed", Kind: metrics.Gauge,
			Help: "estate sources that failed to seed on the last refresh. A LOUD failure — the counterpart " +
				"to the silent one this family exists for, where a source succeeds and returns nothing.",
			Value: float64(failedSources),
		},
		{
			Name: "tg_estate_edges_at_learned_cap", Kind: metrics.Gauge,
			Help: "edges sitting at EXACTLY the learned-confidence cap (0.75). Read against tg_estate_edges: " +
				"a high fraction means the co-occurrence confidences have saturated, so they no longer RANK " +
				"anything and TG_PREDICT_MIN_CONFIDENCE has only two reachable outcomes — keep them all, or " +
				"cut the entire learned graph. Always emitted, including at 0.",
			Value: float64(atCap),
		},
		{
			Name: "tg_estate_edge_triples_unknown", Kind: metrics.Gauge,
			Help: "distinct (FromType,Rel,ToType) triples Upserted into the graph that are NOT in the declared " +
				"edge schema (TG-207). OBSERVE-ONLY: the edge is admitted either way — this counts what an " +
				"enforce flip WOULD reject. It must read ZERO over a real estate for a while before rejection " +
				"is safe; a non-zero means an adapter is emitting an undeclared shape OR the schema is missing " +
				"a legal one. Always emitted, including at 0. A permanently-0 series on a non-empty graph means " +
				"the validator was never wired (its pre-fix state — see TG-207).",
			Value: float64(unknownTriples),
		},
		{
			// DISTINCT from tg_estate_edge_triples_unknown above: that gauge counts undeclared (type,rel,type)
			// SHAPES sitting in the current graph; this COUNTER counts unrecognised relation STRINGS seen at
			// PARSE time (estate.ParseRelType), which never enter the graph as an unknown rel because they are
			// coerced to depends_on or rejected. It is the ontology's own boundary-violation signal: a relation
			// the two-type causal ontology (runs_on/depends_on, plus declared member_of/routes_via) cannot
			// represent. Process-lifetime monotonic total across the declared-config parser (ParseDeclared, the
			// live worker's declared-estate path) and the eval snapshot loader. It is the residual the Siblings
			// edge-type discovery loop consumes (TG-179). Counting does not change whether the edge is rejected
			// or coerced. Always emitted, including at 0, so an ABSENT series is the worker being gone, not the
			// absence of violations.
			Name: "tg_estate_unknown_relation_total", Kind: metrics.Counter,
			Help: "process-lifetime total of estate relations parsed OUTSIDE the declared ontology vocabulary " +
				"(runs_on, member_of, depends_on, routes_via) — an ontology boundary violation, counted at the " +
				"shared estate.ParseRelType chokepoint across the declared-config parser and the eval snapshot " +
				"loader. Non-zero means a source named a relation the causal ontology cannot represent; it is " +
				"the residual signal the Siblings edge-type discovery loop consumes (TG-179). Counting does not " +
				"change whether that edge is rejected or coerced. Always emitted, including at 0.",
			Value: float64(estate.UnknownRelationCount()),
		},
	}
	// One series per edge SOURCE, so "82% of this graph is guesses" is readable without exporting a
	// 77 MB snapshot and querying it. Emitted in sorted order for a byte-stable scrape.
	srcs := make([]string, 0, len(bySource))
	for s := range bySource {
		srcs = append(srcs, s)
	}
	sort.Strings(srcs)
	for _, src := range srcs {
		out = append(out, metrics.Sample{
			Name: "tg_estate_edges_by_source", Kind: metrics.Gauge,
			Help: "estate edges contributed by each source. TOPOLOGY (pve, netbox) is ground truth; " +
				"`incident` is learned co-occurrence and is capped below it. A graph that is mostly " +
				"`incident` is mostly inference, which is what a blast-radius reader needs to know.",
			Value: float64(bySource[src]), Labels: map[string]string{"source": src},
		})
	}
	return out
}

// startEstateSizeJob returns the reader the admin surface calls.
//
// No database and no ticker: the graph is held in memory behind an atomic Holder, so a scrape reads the
// live value directly and cannot go stale between refreshes. That is a deliberate difference from the
// ingest-freshness and poll-queue jobs, which must not query Postgres on the scrape path.
//
// A nil holder yields a reader that emits nothing and says so, because an unmeasured mutation-gate input
// is the condition this exists to end.
func startEstateSizeJob(holder *estate.Holder, failedSources func() int) func() []metrics.Sample {
	if holder == nil {
		return func() []metrics.Sample { return nil }
	}
	return func() []metrics.Sample {
		n := 0
		if failedSources != nil {
			n = failedSources()
		}
		return estateGraphSamples(holder.Graph(), n, time.Now().UTC())
	}
}
