package estate

import (
	"testing"
	"time"
)

func learned(from, to string, conf float64) Edge {
	return Edge{
		From: Entity{Type: TypeHost, Name: from}, To: Entity{Type: TypeHost, Name: to},
		Rel: RelDependsOn, Confidence: conf, Source: SourceIncident,
	}
}

// ★ DECAY THE MISPREDICTED PATH, NOT EVERY EDGE THAT TOUCHES A SURPRISE HOST (TG-206).
//
// core/falsify.DiscoveryRecord carries TargetHost beside SurpriseHosts, and that pairing was collapsed to a
// flat []string before it reached the graph. With only the flat set, a capture that mispredicted web7 from
// pve01 also decays web7's edges to hosts no prediction ever got wrong — including edges that were CORRECT
// for other incidents. That is evidence destruction dressed as learning.
func TestOnlyEdgesOnTheMispredictedPathDecay(t *testing.T) {
	g := NewGraph()
	g.Upsert(learned("pve01", "web7", 0.8))  // ON the mispredicted path
	g.Upsert(learned("web7", "cache2", 0.8)) // touches a surprise host, but NOT this path
	g.Upsert(learned("db3", "cache2", 0.8))  // unrelated entirely

	out, rep := g.DecayOnDisproof(Disproof{
		Paths: []DisproofPath{{Target: "pve01", Surprised: []string{"web7"}}},
		At:    time.Now(),
	}, DecayOptions{Factor: 0.5})

	if rep.Decayed != 1 {
		t.Fatalf("decayed %d edge(s), want exactly 1. Only pve01->web7 was on the mispredicted path; "+
			"decaying web7->cache2 punishes a link no prediction got wrong.", rep.Decayed)
	}
	byPair := map[string]float64{}
	for _, e := range out.Export().Edges {
		byPair[e.FromName+"->"+e.ToName] = e.Confidence
	}
	if got := byPair["pve01->web7"]; got >= 0.8 {
		t.Errorf("pve01->web7 confidence = %v, want decayed below 0.8 — it IS the edge that should have "+
			"carried the prediction and did not", got)
	}
	if got := byPair["web7->cache2"]; got != 0.8 {
		t.Errorf("web7->cache2 confidence = %v, want 0.8 untouched", got)
	}
	if got := byPair["db3->cache2"]; got != 0.8 {
		t.Errorf("db3->cache2 confidence = %v, want 0.8 untouched", got)
	}
}

// Two captures must not cross-contaminate: an edge between hosts surprised by DIFFERENT incidents was never
// on either path.
func TestTwoCapturesDoNotCrossContaminate(t *testing.T) {
	g := NewGraph()
	g.Upsert(learned("web7", "cache2", 0.9)) // web7 surprised pve01; cache2 surprised db3; different captures

	_, rep := g.DecayOnDisproof(Disproof{
		Paths: []DisproofPath{
			{Target: "pve01", Surprised: []string{"web7"}},
			{Target: "db3", Surprised: []string{"cache2"}},
		},
		At: time.Now(),
	}, DecayOptions{Factor: 0.5})

	if rep.Decayed != 0 {
		t.Fatalf("decayed %d edge(s) from two unrelated captures, want 0. Flattening both captures into one "+
			"host set is exactly what makes web7->cache2 look implicated when neither prediction involved it.",
			rep.Decayed)
	}
}

// An edge implicating the pair decays whichever way it points — the prediction direction is not the edge
// direction.
func TestThePathMatchesEitherEdgeDirection(t *testing.T) {
	g := NewGraph()
	g.Upsert(learned("web7", "pve01", 0.8)) // reversed relative to the prediction
	_, rep := g.DecayOnDisproof(Disproof{
		Paths: []DisproofPath{{Target: "pve01", Surprised: []string{"web7"}}},
		At:    time.Now(),
	}, DecayOptions{Factor: 0.5})
	if rep.Decayed != 1 {
		t.Errorf("decayed %d, want 1 — an edge between the target and its surprise host is on the path "+
			"regardless of which way it is stored", rep.Decayed)
	}
}

// BACK-COMPAT. A caller supplying only the flat Hosts set gets the pre-TG-206 behaviour byte for byte.
// Narrowing silently would change the blast-radius model under deployments that never asked for it.
func TestTheFlatHostFormIsUnchanged(t *testing.T) {
	g := NewGraph()
	g.Upsert(learned("pve01", "web7", 0.8))
	g.Upsert(learned("web7", "cache2", 0.8))

	_, rep := g.DecayOnDisproof(Disproof{Hosts: []string{"web7"}, At: time.Now()}, DecayOptions{Factor: 0.5})
	if rep.Decayed != 2 {
		t.Errorf("decayed %d with the legacy flat form, want 2 (every edge incident to web7). The old shape "+
			"must keep its old meaning; only callers that supply Paths opt into the narrower rule.", rep.Decayed)
	}
}

// Neither form supplied ⇒ nothing decays, and no graph is cloned.
func TestAnEmptyDisproofDecaysNothing(t *testing.T) {
	g := NewGraph()
	g.Upsert(learned("a", "b", 0.9))
	out, rep := g.DecayOnDisproof(Disproof{At: time.Now()}, DecayOptions{Factor: 0.5})
	if rep.Decayed != 0 || out != g {
		t.Errorf("an empty disproof decayed %d edge(s) and returned a new graph=%v; want 0 and the same graph",
			rep.Decayed, out != g)
	}
}
