package estate

import (
	"context"
	"errors"
	"testing"
)

// fakeSource yields fixed edges (or an error) for a given provenance.
type fakeSource struct {
	src   Source
	edges []Edge
	err   error
}

func (f fakeSource) Source() Source                        { return f.src }
func (f fakeSource) Edges(context.Context) ([]Edge, error) { return f.edges, f.err }

func TestBuildSeedsFromAllSources(t *testing.T) {
	pve := fakeSource{src: SourcePVE, edges: []Edge{
		{From: Entity{TypeLXC, "n8n01"}, To: Entity{TypePVENode, "pve01"}, Rel: RelRunsOn, Source: SourcePVE}, // conf defaulted from policy
	}}
	netbox := fakeSource{src: SourceNetbox, edges: []Edge{
		{From: Entity{TypePhysicalHost, "pve01"}, To: Entity{TypeSite, "nl"}, Rel: RelMemberOf, Source: SourceNetbox},
	}}
	g, errs := Build(context.Background(), []EdgeSource{pve, netbox})
	if len(errs) != 0 {
		t.Fatalf("no source errors expected, got %v", errs)
	}
	if g.Len() != 2 {
		t.Fatalf("both sources' edges must be present, got %d", g.Len())
	}
	// the policy default confidence was stamped for the edge that left it unset
	e := g.edges[edgeKey(Entity{TypeLXC, "n8n01"}, Entity{TypePVENode, "pve01"}, RelRunsOn)]
	if e.Confidence != 0.95 {
		t.Fatalf("pve edge must take the policy confidence 0.95, got %v", e.Confidence)
	}
}

// A failing source is isolated — the others still seed the graph, and the failure is reported loudly.
func TestBuildIsolatesAFailingSource(t *testing.T) {
	good := fakeSource{src: SourcePVE, edges: []Edge{
		{From: Entity{TypeLXC, "a"}, To: Entity{TypePVENode, "p"}, Rel: RelRunsOn, Source: SourcePVE},
	}}
	bad := fakeSource{src: SourceNetbox, err: errors.New("netbox unreachable")}
	g, errs := Build(context.Background(), []EdgeSource{good, bad})
	if g.Len() != 1 {
		t.Fatalf("the healthy source must still seed despite the failing one, got %d edges", g.Len())
	}
	if len(errs) != 1 || errs[0].Source != SourceNetbox {
		t.Fatalf("the failing source must be reported loudly, got %v", errs)
	}
}

// Cross-source merge uses the MAX-ratchet regardless of source order.
func TestBuildMergesWithRatchetRegardlessOfOrder(t *testing.T) {
	weak := fakeSource{src: SourceNetbox, edges: []Edge{
		{From: Entity{TypeLXC, "x"}, To: Entity{TypePVENode, "p"}, Rel: RelDependsOn, Confidence: 0.85, Source: SourceNetbox},
	}}
	strong := fakeSource{src: SourceLibreNMS, edges: []Edge{
		{From: Entity{TypeLXC, "x"}, To: Entity{TypePVENode, "p"}, Rel: RelDependsOn, Confidence: 0.90, Source: SourceLibreNMS},
	}}
	// weak-then-strong and strong-then-weak must both end at 0.90/librenms.
	for _, order := range [][]EdgeSource{{weak, strong}, {strong, weak}} {
		g, _ := Build(context.Background(), order)
		e := g.edges[edgeKey(Entity{TypeLXC, "x"}, Entity{TypePVENode, "p"}, RelDependsOn)]
		if e.Confidence != 0.90 || e.Source != SourceLibreNMS {
			t.Fatalf("merge must ratchet to 0.90/librenms regardless of order, got %v/%s", e.Confidence, e.Source)
		}
	}
}
