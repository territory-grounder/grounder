package estate

import "testing"

// TG-188 — chaos edges are ground truth about WHICH host we broke, but the DEPENDENT is still inferred from
// what co-alarmed, so a chaos depends_on can INVERT authoritative containment: inject a fault on a guest and its
// HOST alarms (a disk/CPU they share), which co-alarm reads as "host depends_on guest". Admitting that at
// chaos's 0.90 would put the hypervisor in its guest's blast radius as a DEPENDENT. reconcileInferredEdges must
// drop it — the same backwards-causality guard the learned tier gets (TG-379), now extended to chaos.
func TestReconcileDropsBackwardsChaosEdge(t *testing.T) {
	g := NewGraph()
	// Authoritative containment: the guest runs on the host (TYPE mismatch on purpose — matched by canonical name).
	g.Upsert(Edge{From: Entity{Type: TypeLXC, Name: "guestX"}, To: Entity{Type: TypePVENode, Name: "hostY"}, Rel: RelRunsOn, Source: SourcePVE, Confidence: 0.95})
	// A chaos depends_on that INVERTS it: hostY depends_on guestX (we injected guestX; hostY alarmed).
	backwards := Edge{From: Entity{Type: TypeHost, Name: "hostY"}, To: Entity{Type: TypeHost, Name: "guestX"}, Rel: RelDependsOn, Source: SourceChaos, Confidence: 0.90}
	g.Upsert(backwards)

	if dropped := g.reconcileInferredEdges(); dropped != 1 {
		t.Fatalf("reconcile dropped %d, want 1 (the backwards chaos edge)", dropped)
	}
	if _, ok := g.edges[edgeKey(backwards.From, backwards.To, RelDependsOn)]; ok {
		t.Error("the backwards CHAOS edge (host depends_on its own guest) survived — it inverts an authoritative " +
			"runs_on and would put the hypervisor in its guest's blast radius at 0.90")
	}
	if _, ok := g.edges[edgeKey(Entity{Type: TypeLXC, Name: "guestX"}, Entity{Type: TypePVENode, Name: "hostY"}, RelRunsOn)]; !ok {
		t.Error("the authoritative runs_on was removed — reconcile must only drop inferred edges")
	}
}

// A chaos depends_on that FOLLOWS containment (guest depends_on its host) is correct — it must survive.
func TestReconcileKeepsContainmentDirectionChaosEdge(t *testing.T) {
	g := NewGraph()
	g.Upsert(Edge{From: Entity{Type: TypeLXC, Name: "guestX"}, To: Entity{Type: TypePVENode, Name: "hostY"}, Rel: RelRunsOn, Source: SourcePVE, Confidence: 0.95})
	correct := Edge{From: Entity{Type: TypeHost, Name: "guestX"}, To: Entity{Type: TypeHost, Name: "hostY"}, Rel: RelDependsOn, Source: SourceChaos, Confidence: 0.90}
	g.Upsert(correct)
	if dropped := g.reconcileInferredEdges(); dropped != 0 {
		t.Fatalf("dropped %d, want 0 — a chaos depends_on that FOLLOWS containment (guest→host) is correct", dropped)
	}
	if _, ok := g.edges[edgeKey(correct.From, correct.To, RelDependsOn)]; !ok {
		t.Error("a correct-direction chaos edge was dropped")
	}
}

// THE DELIBERATE DIFFERENCE FROM THE LEARNED TIER: a CROSS-SITE chaos edge is KEPT. Co-occurrence across a site
// boundary is a correlation-window artifact and is dropped, but a chaos cross-site cascade is a fault we
// actually injected and observed propagate — the ground-truth cross-site coupling (over a tunnel/route) that
// co-occurrence could never justify. Same graph, same reconcile pass: the learned cross-site edge drops, the
// chaos one survives.
func TestReconcileKeepsCrossSiteChaosEdge(t *testing.T) {
	g := NewGraph()
	g.Upsert(Edge{From: Entity{Type: TypePVENode, Name: "dc1pve03"}, To: Entity{Type: TypeSite, Name: "nllei"}, Rel: RelMemberOf, Source: SourceNetbox, Confidence: 0.90})
	g.Upsert(Edge{From: Entity{Type: TypePVENode, Name: "dc2pve02"}, To: Entity{Type: TypeSite, Name: "grskg"}, Rel: RelMemberOf, Source: SourceNetbox, Confidence: 0.90})
	// Same cross-site pair, opposite directions (so distinct edge keys), different provenance.
	learnedCross := Edge{From: Entity{Type: TypeHost, Name: "dc1pve03"}, To: Entity{Type: TypeHost, Name: "dc2pve02"}, Rel: RelDependsOn, Source: SourceIncident, Confidence: 0.75}
	chaosCross := Edge{From: Entity{Type: TypeHost, Name: "dc2pve02"}, To: Entity{Type: TypeHost, Name: "dc1pve03"}, Rel: RelDependsOn, Source: SourceChaos, Confidence: 0.90}
	g.Upsert(learnedCross)
	g.Upsert(chaosCross)

	if dropped := g.reconcileInferredEdges(); dropped != 1 {
		t.Fatalf("reconcile dropped %d, want 1 (only the LEARNED cross-site edge)", dropped)
	}
	if _, ok := g.edges[edgeKey(learnedCross.From, learnedCross.To, RelDependsOn)]; ok {
		t.Error("the learned cross-site edge survived — co-occurrence across a site boundary is not a dependency")
	}
	if _, ok := g.edges[edgeKey(chaosCross.From, chaosCross.To, RelDependsOn)]; !ok {
		t.Error("the CHAOS cross-site edge was dropped — an injected-and-observed cross-site cascade is ground " +
			"truth, exempt from the co-occurrence cross-site guard")
	}
}
