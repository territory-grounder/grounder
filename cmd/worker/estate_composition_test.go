package main

// WHAT THE GRAPH IS MADE OF, NOT JUST HOW BIG IT IS (TG-352).
//
// tg_estate_edges shipped because an empty graph and a missing exporter looked identical, and it paid for
// itself immediately: 392 edges on triage against 17 on the actuation plane, with zero source failures on
// both. But a SIZE alone cannot answer the next question, and on 2026-08-06 that question turned out to
// govern the whole blast-radius predictor:
//
//	rel          source     edges   avg_conf
//	depends_on   incident    1524      0.745      <- 82% of the graph is INFERENCE
//	runs_on      netbox       180      0.900
//	runs_on      pve          143      0.950
//	depends_on   librenms      17      0.900
//
//	learned-edge confidences:  0.50×4  0.56×22  0.57×1  0.63×5  0.67×11  0.75×1481
//
// 1,481 of 1,524 learned edges — 97.2% — sit at EXACTLY estate.LearnedConfidenceCap. So
// TG_PREDICT_MIN_CONFIDENCE, whose own comment says "tune toward ~0.70 to cut the low-confidence
// far/learned-edge false positives", removes 43 edges at 0.70 and all 1,524 at anything above 0.75. Two
// reachable outcomes, no tail to cut, and neither fact visible from any published series.
//
// Getting those numbers required exporting a 77 MB snapshot and querying its JSON. These gauges make the
// same question a scrape.

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/metrics"
)

func sampleWithLabel(ss []metrics.Sample, name, label, val string) (float64, bool) {
	for _, s := range ss {
		if s.Name == name && s.Labels[label] == val {
			return s.Value, true
		}
	}
	return 0, false
}

func sampleNamed(ss []metrics.Sample, name string) (float64, bool) {
	for _, s := range ss {
		if s.Name == name {
			return s.Value, true
		}
	}
	return 0, false
}

// compositionGraph builds a graph in the live shape: ground-truth topology graded ABOVE the cap, and
// learned edges saturated AT it plus a short below-cap tail.
func compositionGraph(t *testing.T) *estate.Graph {
	t.Helper()
	g := estate.NewGraph()
	// topology — pve 0.95, netbox 0.90
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: estate.TypeLXC, Name: "guest-a"},
		To:   estate.Entity{Type: estate.TypePVENode, Name: "node-1"},
		Rel:  estate.RelRunsOn, Source: estate.SourcePVE, Confidence: 0.95})
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: estate.TypeVM, Name: "guest-b"},
		To:   estate.Entity{Type: estate.TypePVENode, Name: "node-1"},
		Rel:  estate.RelRunsOn, Source: estate.SourceNetbox, Confidence: 0.90})
	// learned — two saturated at the cap, one below it
	for _, n := range []string{"learn-1", "learn-2"} {
		g.Upsert(estate.Edge{
			From: estate.Entity{Type: estate.TypeHost, Name: n},
			To:   estate.Entity{Type: estate.TypeHost, Name: "primary"},
			Rel:  estate.RelDependsOn, Source: estate.SourceIncident,
			Confidence: estate.LearnedConfidenceCap})
	}
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: estate.TypeHost, Name: "learn-3"},
		To:   estate.Entity{Type: estate.TypeHost, Name: "primary"},
		Rel:  estate.RelDependsOn, Source: estate.SourceIncident, Confidence: 0.56})
	return g
}

// KILLING MUTATION: delete the per-source loop, or the at-cap counter. RED — the graph's size is published
// and its composition is not, which is the state that required a 77 MB snapshot export to answer.
func TestTheGraphPublishesItsCompositionNotJustItsSize(t *testing.T) {
	ss := estateGraphSamples(compositionGraph(t), 0, time.Now().UTC())

	atCap, ok := sampleNamed(ss, "tg_estate_edges_at_learned_cap")
	if !ok {
		t.Fatal("tg_estate_edges_at_learned_cap is absent — the saturation that makes " +
			"TG_PREDICT_MIN_CONFIDENCE undiscriminating is invisible from every published series")
	}
	if atCap != 2 {
		t.Errorf("edges at the learned cap = %v, want 2 — the counter is not identifying saturated "+
			"learned edges", atCap)
	}
	for _, c := range []struct {
		source string
		want   float64
	}{{"incident", 3}, {"pve", 1}, {"netbox", 1}} {
		got, ok := sampleWithLabel(ss, "tg_estate_edges_by_source", "source", c.source)
		if !ok {
			t.Errorf("no tg_estate_edges_by_source series for %q — \"82%% of this graph is inference\" is "+
				"not readable without exporting the snapshot", c.source)
			continue
		}
		if got != c.want {
			t.Errorf("source %q = %v, want %v", c.source, got, c.want)
		}
	}
}

// GROUND TRUTH MUST NOT BE COUNTED AS SATURATED. Topology is graded ABOVE the cap on purpose (pve 0.95,
// netbox 0.90) so a heuristic edge can never outrank it. A counter that used >= instead of == would fold
// every topology edge into the saturation number and report a graph that is entirely inference.
//
// KILLING MUTATION: change `e.Confidence == estate.LearnedConfidenceCap` to `>=`. RED.
func TestTopologyIsNotCountedAsASaturatedLearnedEdge(t *testing.T) {
	ss := estateGraphSamples(compositionGraph(t), 0, time.Now().UTC())
	atCap, _ := sampleNamed(ss, "tg_estate_edges_at_learned_cap")
	edges, _ := sampleNamed(ss, "tg_estate_edges")
	if atCap >= edges {
		t.Fatalf("every edge (%v of %v) counted as at the learned cap — ground-truth topology is graded "+
			"ABOVE the cap and must not be folded in, or the gauge reports a graph that is entirely inference", atCap, edges)
	}
}

// THE VACUITY FLOOR, matching the family this joins: an empty graph must still publish, at zero, so an
// absent series means the exporter is gone rather than the estate being empty.
//
// KILLING MUTATION: emit the composition series only when the graph is non-empty. RED.
func TestAnEmptyGraphStillPublishesItsComposition(t *testing.T) {
	ss := estateGraphSamples(estate.NewGraph(), 0, time.Now().UTC())
	if v, ok := sampleNamed(ss, "tg_estate_edges_at_learned_cap"); !ok || v != 0 {
		t.Fatalf("an empty graph did not publish tg_estate_edges_at_learned_cap at zero (ok=%v v=%v) — "+
			"absent and zero must never mean the same thing here", ok, v)
	}
	if v, ok := sampleNamed(ss, "tg_estate_edges"); !ok || v != 0 {
		t.Fatalf("an empty graph did not publish tg_estate_edges at zero (ok=%v v=%v)", ok, v)
	}
}
