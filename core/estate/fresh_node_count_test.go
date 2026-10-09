package estate

// TG-449: tg_estate_nodes must count the LIVE node set (endpoints of fresh edges), not len(Export().Nodes),
// which counts every entity that was ever an endpoint — Upsert never physically removes an expired edge, so
// a node whose every edge has aged out lingers in Export().Nodes forever. During the source-goes-quiet
// degradation this metric exists to reveal, an Export-derived count holds high while the fresh graph shrinks.

import (
	"testing"
	"time"
)

func TestFreshNodeCountExcludesExpiredEdgeEndpoints(t *testing.T) {
	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	g := NewGraph(WithClock(func() time.Time { return now }))

	// One FRESH edge (no expiry) and one EXPIRED edge (ValidUntil already passed). Distinct endpoints so the
	// two counts are unambiguous: Export sees all four, only two are on a live edge.
	g.Upsert(Edge{From: Entity{Type: TypeVM, Name: "live-vm"}, To: Entity{Type: TypePVENode, Name: "live-node"},
		Rel: RelRunsOn, Source: SourcePVE, Confidence: 0.95})
	g.Upsert(Edge{From: Entity{Type: TypeVM, Name: "stale-vm"}, To: Entity{Type: TypePVENode, Name: "stale-node"},
		Rel: RelRunsOn, Source: SourcePVE, Confidence: 0.95, ValidUntil: now.Add(-time.Hour)})

	// The premise: Export().Nodes counts the stale endpoints too (this is the bug the gauge inherited).
	if got := len(g.Export().Nodes); got != 4 {
		t.Fatalf("Export().Nodes = %d, want 4 — the stale edge's endpoints are still projected", got)
	}

	// KILLING MUTATION: derive the count from len(g.Export().Nodes) (or drop the g.fresh guard in
	// FreshNodeCount) → this returns 4 and the assertion reds.
	if got := g.FreshNodeCount(); got != 2 {
		t.Fatalf("FreshNodeCount = %d, want 2 — only the fresh edge's two endpoints are live; the expired "+
			"edge's endpoints must be excluded (TG-449)", got)
	}
}

// The all-stale case (mutate toward EMPTINESS, TG-365): a graph whose every edge has expired reads 0 live
// nodes, not the endpoint count Export still projects — so a source going fully quiet reads as an empty live
// graph, distinct from a healthy one.
func TestFreshNodeCountIsZeroWhenEveryEdgeExpired(t *testing.T) {
	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	g := NewGraph(WithClock(func() time.Time { return now }))
	g.Upsert(Edge{From: Entity{Type: TypeVM, Name: "gone-vm"}, To: Entity{Type: TypePVENode, Name: "gone-node"},
		Rel: RelRunsOn, Source: SourcePVE, Confidence: 0.95, ValidUntil: now.Add(-time.Hour)})

	if got := len(g.Export().Nodes); got != 2 {
		t.Fatalf("Export().Nodes = %d, want 2 (the expired edge's endpoints are still projected)", got)
	}
	if got := g.FreshNodeCount(); got != 0 {
		t.Fatalf("FreshNodeCount = %d, want 0 — no fresh edge, so no live node (Export would read 2)", got)
	}
}
