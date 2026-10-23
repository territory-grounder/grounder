package estate

import (
	"testing"
	"time"
)

// edgeConf returns the confidence of the from→to edge in the graph's snapshot (−1 if absent).
func edgeConf(g *Graph, from, to string) float64 {
	for _, e := range g.Export().Edges {
		if e.FromName == from && e.ToName == to {
			return e.Confidence
		}
	}
	return -1
}

// dependsInBlast reports whether dependent is in target's (fresh) blast radius.
func dependsInBlast(g *Graph, target, dependent string) bool {
	for _, imp := range g.BlastRadius(Entity{Type: TypeHost, Name: target}, 3) {
		if imp.Entity.Name == dependent {
			return true
		}
	}
	return false
}

// A learned estate edge a fresh observation contradicts loses confidence; the ground-truth tier is never
// touched; the disproof works on a CLONE so the published graph is unchanged; and an edge decayed to the
// floor is AGED OUT of every traversal.
func TestDecayOnDisproofDecaysLearnedEdges(t *testing.T) {
	nowT := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	g := NewGraph(WithClock(func() time.Time { return nowT }))
	// a LEARNED edge (app01 depends on db01) at the capped learned confidence.
	g.Upsert(Edge{From: Entity{Type: TypeHost, Name: "app01"}, To: Entity{Type: TypeHost, Name: "db01"}, Rel: RelDependsOn, Confidence: 0.75, Source: SourceIncident})
	// a GROUND-TRUTH edge (vm depends on its pve node) — must never be decayed by a heuristic disproof.
	g.Upsert(Edge{From: Entity{Type: TypeVM, Name: "web01"}, To: Entity{Type: TypePVENode, Name: "pve1"}, Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})

	// A fresh verify observation names app01 (a surprise/mismatch host). The learned edge incident to it decays.
	newG, rep := g.DecayOnDisproof(Disproof{Hosts: []string{"app01"}, At: nowT}, DecayOptions{Factor: 0.5})
	if rep.Decayed != 1 || rep.AgedOut != 0 {
		t.Fatalf("exactly the one learned edge must decay (not age out yet): %+v", rep)
	}
	if c := edgeConf(newG, "app01", "db01"); c < 0.374 || c > 0.376 {
		t.Fatalf("the learned edge confidence must halve 0.75 → 0.375, got %.4f", c)
	}
	if c := edgeConf(newG, "web01", "pve1"); c != 0.95 {
		t.Fatalf("the ground-truth edge must be untouched at 0.95, got %.4f", c)
	}
	// the ORIGINAL graph is unchanged (decay worked on a clone) — no in-place mutation of a published graph.
	if c := edgeConf(g, "app01", "db01"); c != 0.75 {
		t.Fatalf("the receiver graph must be unmutated at 0.75, got %.4f", c)
	}

	// A disproof naming no in-graph host returns the receiver unchanged.
	same, rep2 := g.DecayOnDisproof(Disproof{Hosts: []string{"ghost99"}, At: nowT}, DecayOptions{})
	if rep2.Decayed != 0 || same != g {
		t.Fatalf("a disproof of an unknown host must be a no-op returning the receiver: %+v", rep2)
	}
}

// Repeated disproof drives a learned edge below the floor, aging it out of the blast-radius walk entirely.
func TestDecayOnDisproofAgesOutAtFloor(t *testing.T) {
	nowT := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	g := NewGraph(WithClock(func() time.Time { return nowT }))
	g.Upsert(Edge{From: Entity{Type: TypeHost, Name: "app01"}, To: Entity{Type: TypeHost, Name: "db01"}, Rel: RelDependsOn, Confidence: 0.75, Source: SourceIncident})
	if !dependsInBlast(g, "db01", "app01") {
		t.Fatal("precondition: app01 must be in db01's blast radius before decay")
	}
	// Floor 0.4 ages out the edge in one pass (0.75 → 0.375 <= 0.4).
	newG, rep := g.DecayOnDisproof(Disproof{Hosts: []string{"db01"}, At: nowT}, DecayOptions{Factor: 0.5, Floor: 0.4})
	if rep.AgedOut != 1 || len(rep.AgedKeys) != 1 {
		t.Fatalf("the disproved learned edge must be aged out: %+v", rep)
	}
	if dependsInBlast(newG, "db01", "app01") {
		t.Fatal("an aged-out learned edge must be excluded from the blast radius")
	}
	// the receiver still has it (clone semantics).
	if !dependsInBlast(g, "db01", "app01") {
		t.Fatal("the receiver graph must still carry the edge (decay is not in-place)")
	}
}
