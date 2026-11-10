package estate

import (
	"strings"
	"testing"
)

// ★ AN UNLISTED TRIPLE IS RECORDED, NEVER DROPPED (TG-207).
//
// This is the single most important property here. The adjacency table is derived from what the running
// estate contains PLUS what the adapters construct, and neither is proof of completeness — measured
// 2026-08-06 the live graph held only three triples while adapters also build member_of and routes_via
// edges. A validator that refused on day one would silently delete site membership and tunnel routing from
// the blast-radius input, which is strictly worse than the defect it fixes.
func TestAnUnlistedTripleIsAdmittedAndCounted(t *testing.T) {
	g := NewGraph().WithSchema(NewEdgeSchema(DefaultEdgeSchema()))
	g.Upsert(Edge{
		From: Entity{Type: "totally_made_up", Name: "x"},
		To:   Entity{Type: TypeHost, Name: "y"},
		Rel:  RelDependsOn, Confidence: 0.9, Source: SourceDeclared,
	})

	// ADMITTED: the edge must still be in the graph.
	if got := len(g.Export().Edges); got != 1 {
		t.Fatalf("the graph holds %d edge(s) after an unlisted triple; want 1. Dropping edges against an "+
			"unproven table corrupts the blast-radius model this check exists to protect.", got)
	}
	// AND COUNTED: otherwise the enforce flip has no evidence and can never be made safely.
	if n := g.Schema().UnknownCount(); n != 1 {
		t.Errorf("UnknownCount = %d, want 1 — an unlisted triple that is neither refused nor recorded is "+
			"exactly the silent-acceptance defect TG-207 reports", n)
	}
	if got := g.Schema().UnknownTriples(); len(got) != 1 || !strings.Contains(got[0], "totally_made_up") {
		t.Errorf("UnknownTriples = %v, want the offending triple named", got)
	}
}

// The triples the live estate actually contains must not be reported as unknown, or the gauge is noise from
// the first scrape and gets ignored.
func TestTheLiveEstatesTriplesAreListed(t *testing.T) {
	s := NewEdgeSchema(DefaultEdgeSchema())
	for _, tc := range []struct {
		from EntityType
		rel  RelType
		to   EntityType
	}{
		{TypeHost, RelDependsOn, TypeHost},   // 1541 edges live
		{TypeVM, RelRunsOn, TypePVENode},     // 211
		{TypeLXC, RelRunsOn, TypePVENode},    // 112
		{TypeHost, RelMemberOf, TypeSite},    // adapters build these
		{TypeHost, RelRoutesVia, TypeTunnel}, // and these
	} {
		if !s.Check(tc.from, tc.rel, tc.to) {
			t.Errorf("%s|%s|%s is not listed, but the estate or an adapter produces it — this gauge would "+
				"read non-zero forever and stop being read", tc.from, tc.rel, tc.to)
		}
	}
	if n := s.UnknownCount(); n != 0 {
		t.Errorf("UnknownCount = %d after only legitimate triples, want 0", n)
	}
}

// An EMPTY table must allow everything. A schema nobody configured must not silently become a schema that
// rejects everything — that is the same fail-closed-on-an-unmigrated-config trap, inverted.
func TestAnEmptyTableAllowsEverything(t *testing.T) {
	s := NewEdgeSchema(nil)
	if !s.Check("anything", "at_all", "really") {
		t.Fatal("an unconfigured schema rejected a triple. Empty must mean 'no opinion', never 'deny all'")
	}
	if n := s.UnknownCount(); n != 0 {
		t.Errorf("an unconfigured schema recorded %d unknown triple(s); it has no basis to call anything "+
			"unknown", n)
	}
}

// A graph with no schema must behave exactly as before — every existing caller gets nil.
func TestANilSchemaChangesNothing(t *testing.T) {
	g := NewGraph()
	g.Upsert(Edge{
		From: Entity{Type: "weird", Name: "a"}, To: Entity{Type: TypeHost, Name: "b"},
		Rel: RelDependsOn, Confidence: 0.5, Source: SourceDeclared,
	})
	if got := len(g.Export().Edges); got != 1 {
		t.Errorf("a schemaless graph holds %d edge(s), want 1 — attaching no schema must be byte-identical "+
			"to the pre-TG-207 behaviour", got)
	}
	if g.Schema().UnknownCount() != 0 {
		t.Error("a nil schema counted something")
	}
}
