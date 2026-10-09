package estate

import "testing"

// TG-379 — the pve03 cascade taught the graph BACKWARDS causality (pve03 depends_on its own guest) and
// cross-site dependencies (nllei pve03 depends_on grskg pve02), both at the fixed learned 0.75 which sits
// ABOVE the 0.70 prediction threshold, so they were live in blast-radius. reconcileInferredEdges drops them
// after Build, against the authoritative topology.
func TestReconcileDropsBackwardsAndCrossSiteLearnedEdges(t *testing.T) {
	g := NewGraph()

	// Authoritative containment (note the TYPE mismatch with the learner below): the guest is an LXC here,
	// but the learner stamps TypeHost — the reconcile must still recognise the inversion by canonical NAME.
	g.Upsert(Edge{From: Entity{Type: TypeLXC, Name: "dc1cl01iotarb01"}, To: Entity{Type: TypePVENode, Name: "dc1pve03"}, Rel: RelRunsOn, Source: SourcePVE, Confidence: 0.95})
	// Site membership for the cross-site pair.
	g.Upsert(Edge{From: Entity{Type: TypePVENode, Name: "dc1pve03"}, To: Entity{Type: TypeSite, Name: "nllei"}, Rel: RelMemberOf, Source: SourceNetbox, Confidence: 0.90})
	g.Upsert(Edge{From: Entity{Type: TypePVENode, Name: "dc2pve02"}, To: Entity{Type: TypeSite, Name: "grskg"}, Rel: RelMemberOf, Source: SourceNetbox, Confidence: 0.90})
	// A legitimate same-site learned pair (both nllei), no containment between them — must SURVIVE.
	g.Upsert(Edge{From: Entity{Type: TypeHost, Name: "dc1app1"}, To: Entity{Type: TypeSite, Name: "nllei"}, Rel: RelMemberOf, Source: SourceNetbox, Confidence: 0.90})
	g.Upsert(Edge{From: Entity{Type: TypeHost, Name: "dc1db1"}, To: Entity{Type: TypeSite, Name: "nllei"}, Rel: RelMemberOf, Source: SourceNetbox, Confidence: 0.90})

	// The three learned (incident) depends_on edges the storm wrote:
	backwards := Edge{From: Entity{Type: TypePVENode, Name: "dc1pve03"}, To: Entity{Type: TypeHost, Name: "dc1cl01iotarb01"}, Rel: RelDependsOn, Source: SourceIncident, Confidence: 0.75}
	crossSite := Edge{From: Entity{Type: TypePVENode, Name: "dc1pve03"}, To: Entity{Type: TypeHost, Name: "dc2pve02"}, Rel: RelDependsOn, Source: SourceIncident, Confidence: 0.75}
	good := Edge{From: Entity{Type: TypeHost, Name: "dc1app1"}, To: Entity{Type: TypeHost, Name: "dc1db1"}, Rel: RelDependsOn, Source: SourceIncident, Confidence: 0.75}
	g.Upsert(backwards)
	g.Upsert(crossSite)
	g.Upsert(good)

	if dropped := g.reconcileInferredEdges(); dropped != 2 {
		t.Fatalf("reconcile dropped %d learned edges, want 2 (backwards + cross-site)", dropped)
	}

	has := func(e Edge) bool { _, ok := g.edges[edgeKey(e.From, e.To, e.Rel)]; return ok }

	if has(backwards) {
		t.Error("the BACKWARDS edge (pve03 depends_on its own guest) survived — it inverts an authoritative " +
			"runs_on and would put the hypervisor in its guest's blast radius as a DEPENDENT (TG-379)")
	}
	if has(crossSite) {
		t.Error("the CROSS-SITE learned edge (nllei pve03 depends_on grskg pve02) survived — co-occurrence " +
			"across a site boundary is not a physical dependency")
	}
	if !has(good) {
		t.Error("a legitimate same-site learned edge with no containment was dropped — the reconcile is over-broad")
	}
	// Authoritative edges are never touched.
	if _, ok := g.edges[edgeKey(Entity{Type: TypeLXC, Name: "dc1cl01iotarb01"}, Entity{Type: TypePVENode, Name: "dc1pve03"}, RelRunsOn)]; !ok {
		t.Error("the authoritative runs_on was removed — reconcile must only drop SourceIncident edges")
	}
}

// TestReconcileLeavesLearnedEdgesInTheContainmentDIRECTION — a guest depends_on its host is CORRECT (it
// follows the containment, not inverts it), so a learned edge in that direction must survive even though a
// runs_on exists for the same pair.
func TestReconcileKeepsLearnedEdgesInContainmentDirection(t *testing.T) {
	g := NewGraph()
	g.Upsert(Edge{From: Entity{Type: TypeLXC, Name: "guestA"}, To: Entity{Type: TypePVENode, Name: "hostB"}, Rel: RelRunsOn, Source: SourcePVE, Confidence: 0.95})
	// learned: guestA depends_on hostB — SAME direction as runs_on (guest → host). Correct, keep.
	correct := Edge{From: Entity{Type: TypeHost, Name: "guestA"}, To: Entity{Type: TypeHost, Name: "hostB"}, Rel: RelDependsOn, Source: SourceIncident, Confidence: 0.75}
	g.Upsert(correct)
	if dropped := g.reconcileInferredEdges(); dropped != 0 {
		t.Fatalf("dropped %d, want 0 — a learned depends_on that FOLLOWS containment (guest→host) is correct", dropped)
	}
	if _, ok := g.edges[edgeKey(correct.From, correct.To, RelDependsOn)]; !ok {
		t.Error("a correct-direction learned edge was dropped")
	}
}
