package runner

import (
	"github.com/territory-grounder/grounder/core/correlate"
	"github.com/territory-grounder/grounder/core/estate"
)

// GraphTopology adapts the live estate graph to correlate.Topology, the narrow oracle the causal
// cluster-collapse election reads (TG-385): estate in-degree (how many things fall with a host) and the
// runs_on parent (a host's cascading placement node). It is the ONE place estate topology is read into the
// correlation stage — kept out of core/correlate so that stage stays a pure, database-free rule.
//
// provider is read PER CALL, not captured once, so a topology refresh (a promotion/decay, a source coming
// back) reaches the next election without a restart — the same discipline the prediction gate's
// EstateProvider uses. A nil provider or a nil graph yields the zero topology (in-degree 0, no parent),
// which the election treats as "no causal signal" and falls back to earliest-arrival: correct for a
// deployment whose estate graph is not yet seeded, never a fabricated centrality claim.
func GraphTopology(provider func() *estate.Graph) correlate.Topology {
	return graphTopology{provider: provider}
}

type graphTopology struct{ provider func() *estate.Graph }

func (t graphTopology) graph() *estate.Graph {
	if t.provider == nil {
		return nil
	}
	return t.provider()
}

func (t graphTopology) InDegree(host string) int {
	g := t.graph()
	if g == nil || host == "" {
		return 0
	}
	return g.InDegree(estate.Entity{Name: host})
}

func (t graphTopology) RunsOnParent(host string) string {
	g := t.graph()
	if g == nil || host == "" {
		return ""
	}
	if p, ok := g.RunsOnParent(estate.Entity{Name: host}); ok {
		return p.Name
	}
	return ""
}
