package main

// Guards for the mutation gate's input gauge (TG-343).

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/metrics"
)

func estSample(ss []metrics.Sample, name string) (metrics.Sample, bool) {
	for _, s := range ss {
		if s.Name == name {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

// THE WHOLE POINT. An empty graph must publish an explicit zero, not an absent series — because a gate
// reasoning over an empty graph cannot refuse, and a rule written over an absent series goes quiet.
func TestAnEmptyGraphPublishesAnExplicitZero(t *testing.T) {
	ss := estateGraphSamples(estate.NewGraph(), 0, time.Now().UTC())
	edges, ok := estSample(ss, "tg_estate_edges")
	if !ok {
		t.Fatal("tg_estate_edges was not emitted for an empty graph. Absent and empty then render " +
			"identically, and the state where the mutation gate cannot refuse anything is unalertable.")
	}
	if edges.Value != 0 {
		t.Errorf("edges = %v for an empty graph, want 0", edges.Value)
	}
	if _, ok := estSample(ss, "tg_estate_nodes"); !ok {
		t.Error("tg_estate_nodes was not emitted — a graph with nodes and no edges is a seed that " +
			"produced inventory but no relationships, and only the pair can show that")
	}
}

// A nil graph must not panic and must still publish. A worker whose holder is empty is exactly the case
// where somebody needs to be told.
func TestANilGraphStillPublishesZeroRatherThanPanicking(t *testing.T) {
	ss := estateGraphSamples(nil, 0, time.Now().UTC())
	if e, ok := estSample(ss, "tg_estate_edges"); !ok || e.Value != 0 {
		t.Errorf("nil graph published edges=%v present=%v, want an explicit 0", e.Value, ok)
	}
}

// A populated graph must report REAL counts. Without this every assertion above passes against a sampler
// hard-wired to zero.
// TG-207: the observe-only edge-triple validator's count must be PUBLISHED, or wiring it changes nothing an
// operator can see. A populated graph carrying an undeclared triple reports it; an unwired (schema-less)
// graph still publishes the gauge, at 0 — never absent, never a panic.
func TestEstateGraphSamplesPublishesUndeclaredTripleCount(t *testing.T) {
	g := estate.NewGraph(estate.WithDefaultEdgeSchema())
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeHost, Name: "a"}, Rel: estate.RelDependsOn, To: estate.Entity{Type: estate.TypeHost, Name: "b"}, Source: "test"})  // declared
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeLXC, Name: "c"}, Rel: estate.RelDependsOn, To: estate.Entity{Type: estate.TypeTunnel, Name: "t"}, Source: "test"}) // UNDECLARED

	s, ok := estSample(estateGraphSamples(g, 0, time.Now().UTC()), "tg_estate_edge_triples_unknown")
	if !ok {
		t.Fatal("tg_estate_edge_triples_unknown is not published — the observe-only count is invisible")
	}
	if s.Value != 1 {
		t.Fatalf("tg_estate_edge_triples_unknown=%v, want 1", s.Value)
	}

	// An unwired graph (no schema) still publishes the gauge at 0 — the pre-fix state must read HONEST 0.
	s0, ok0 := estSample(estateGraphSamples(estate.NewGraph(), 0, time.Now().UTC()), "tg_estate_edge_triples_unknown")
	if !ok0 || s0.Value != 0 {
		t.Fatalf("unwired graph: gauge ok=%v value=%v, want present and 0", ok0, s0.Value)
	}
}

func TestAPopulatedGraphReportsItsRealSize(t *testing.T) {
	g := estate.NewGraph()
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: "host", Name: "dc1pve01"},
		To:   estate.Entity{Type: "host", Name: "dc1k8s01"},
		Rel:  "hosts", Confidence: 0.9, Source: "test",
	})
	ss := estateGraphSamples(g, 0, time.Now().UTC())
	edges, _ := estSample(ss, "tg_estate_edges")
	nodes, _ := estSample(ss, "tg_estate_nodes")
	if edges.Value < 1 {
		t.Errorf("a graph with an edge reported %v edges — the sampler is hard-wired and every other "+
			"assertion in this file is vacuous", edges.Value)
	}
	if nodes.Value < 2 {
		t.Errorf("a graph with one edge between two hosts reported %v nodes, want at least 2", nodes.Value)
	}
}

// The LOUD failure is published beside the silent one. A source that errors is already logged; a source
// that succeeds and returns nothing is not, and the two must be separable on the dashboard.
func TestFailedSourcesArePublishedBesideTheSize(t *testing.T) {
	ss := estateGraphSamples(estate.NewGraph(), 2, time.Now().UTC())
	f, ok := estSample(ss, "tg_estate_sources_failed")
	if !ok {
		t.Fatal("no failed-source gauge — an empty graph caused by denied credentials and one caused by " +
			"a source that returned nothing would then look identical")
	}
	if f.Value != 2 {
		t.Errorf("failed sources = %v, want 2", f.Value)
	}
}

// A nil holder degrades to silence rather than panicking on the scrape path.
func TestANilHolderEmitsNothing(t *testing.T) {
	if got := startEstateSizeJob(nil, nil)(); got != nil {
		t.Errorf("a nil holder published %d samples, want none", len(got))
	}
}

// THE COMPOSITION ROOT. Every test above exercises the sampler in isolation; none notices if the admin
// surface never calls it. That is this repo's standing failure shape.
func TestTheAdminSurfaceActuallyEmitsTheEstateSizeGauges(t *testing.T) {
	adm := &workerAdmin{}
	baseline := len(adm.samples())
	if baseline == 0 {
		t.Fatal("the bare admin surface emitted nothing, so the comparison below is meaningless")
	}
	g := estate.NewGraph()
	adm = adm.withEstateSize(startEstateSizeJob(estate.NewHolder(g), func() int { return 0 }))
	names := map[string]bool{}
	for _, s := range adm.samples() {
		names[s.Name] = true
	}
	if len(adm.samples()) == baseline {
		t.Fatal("wiring the estate-size reader changed NOTHING on /metrics — samples() does not call it")
	}
	for _, want := range []string{"tg_estate_edges", "tg_estate_nodes", "tg_estate_sources_failed", "tg_estate_unknown_relation_total"} {
		if !names[want] {
			t.Errorf("%s is computed and never reaches /metrics. The mutation gate's input stays "+
				"unmeasured while every unit test above passes.", want)
		}
	}
}

// TG-179: the unknown_relation counter must be PUBLISHED as a monotonic counter, always (including at 0), or
// the ontology's boundary violations stay invisible on /metrics. A nil graph still emits it — an ABSENT
// series must mean the worker is gone, not that no violation was ever seen.
func TestEstateGraphSamplesPublishesUnknownRelationCounter(t *testing.T) {
	s, ok := estSample(estateGraphSamples(nil, 0, time.Now().UTC()), "tg_estate_unknown_relation_total")
	if !ok {
		t.Fatal("tg_estate_unknown_relation_total is not published — the ontology boundary-violation signal is invisible")
	}
	if s.Kind != metrics.Counter {
		t.Errorf("tg_estate_unknown_relation_total kind = %q, want counter (it is a monotonic total)", s.Kind)
	}
}
