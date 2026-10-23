package estate

import (
	"context"
	"testing"
)

// TG-207: !1044 defined the observe-only edge-triple validator but never attached it to a production graph
// (WithSchema was called only in a unit test). WithDefaultEdgeSchema is the wiring seam. Build must attach
// the schema when it is passed, count an UNDECLARED triple, and still ADMIT the edge (observe-only never
// drops). A graph built without the option carries no schema and counts nothing — the pre-fix behaviour.
func TestBuild_WithDefaultEdgeSchema_CountsUndeclaredTriples(t *testing.T) {
	src := fakeSource{src: SourceIncident, edges: []Edge{
		{From: Entity{Type: TypeHost, Name: "a"}, Rel: RelDependsOn, To: Entity{Type: TypeHost, Name: "b"}, Source: SourceIncident},  // declared
		{From: Entity{Type: TypeLXC, Name: "c"}, Rel: RelDependsOn, To: Entity{Type: TypeTunnel, Name: "t"}, Source: SourceIncident}, // UNDECLARED
	}}

	g, errs := Build(context.Background(), []EdgeSource{src}, WithDefaultEdgeSchema())
	if len(errs) != 0 {
		t.Fatalf("unexpected build errors: %v", errs)
	}
	if g.Schema() == nil {
		t.Fatal("WithDefaultEdgeSchema did not attach a schema — the validator is still unwired")
	}
	if got := g.Schema().UnknownCount(); got != 1 {
		t.Fatalf("UnknownCount=%d, want 1 — only the undeclared (lxc,depends_on,tunnel) triple, not the declared host->host", got)
	}
	if g.Len() != 2 {
		t.Fatalf("graph has %d edges, want 2 — observe-only must ADMIT the undeclared edge, only count it", g.Len())
	}

	// Pre-fix state: no option → nil schema → nothing counted, and Schema().UnknownCount() nil-guards to 0.
	g2, _ := Build(context.Background(), []EdgeSource{src})
	if g2.Schema() != nil {
		t.Fatal("a graph built without WithDefaultEdgeSchema must carry no schema")
	}
	if got := g2.Schema().UnknownCount(); got != 0 {
		t.Fatalf("nil-schema UnknownCount=%d, want 0", got)
	}
}
