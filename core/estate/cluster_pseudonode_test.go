package estate

import "testing"

// TG-390 — a NetBox virtualization cluster used to be written as estate.TypePVENode, so a logical placement
// group (even a Synology DSM cluster) impersonated a physical hypervisor: it carried 133 children, became an
// eligible common-cause parent, and kept HasGroundTruth true at 0.90 when the real per-node edge was gone —
// defeating TG-202's "stay silent" state and re-hiding the true parent once a tombstoned edge (TG-375) decays.
//
// The cluster is now its own TypeCluster (a member_of grouping). This proves the two behaviours that matters:
// it is NOT a common-cause sibling parent, and it is NOT ground truth.
func TestClusterPseudoNodeIsNotSiblingParentNorGroundTruth(t *testing.T) {
	g := NewGraph()
	// Two guests on DIFFERENT real hypervisors, both members of the SAME NetBox cluster grouping.
	g.Upsert(Edge{From: Entity{Type: TypeVM, Name: "gA"}, To: Entity{Type: TypePVENode, Name: "dc1pve03"}, Rel: RelRunsOn, Source: SourcePVE, Confidence: 0.95})
	g.Upsert(Edge{From: Entity{Type: TypeVM, Name: "gB"}, To: Entity{Type: TypePVENode, Name: "dc1pve01"}, Rel: RelRunsOn, Source: SourcePVE, Confidence: 0.95})
	g.Upsert(Edge{From: Entity{Type: TypeVM, Name: "gA"}, To: Entity{Type: TypeCluster, Name: "dc1-pve"}, Rel: RelMemberOf, Source: SourceNetbox, Confidence: 0.90})
	g.Upsert(Edge{From: Entity{Type: TypeVM, Name: "gB"}, To: Entity{Type: TypeCluster, Name: "dc1-pve"}, Rel: RelMemberOf, Source: SourceNetbox, Confidence: 0.90})

	// VACUITY GUARD: the fixture must actually contain a cluster edge, or "no cluster siblings / no cluster
	// ground truth" would pass over a graph that never exercised the cluster path at all.
	clusterEdges := 0
	for _, ed := range g.edges {
		if ed.To.Type == TypeCluster {
			clusterEdges++
		}
	}
	if clusterEdges == 0 {
		t.Fatal("vacuity guard: fixture has no cluster edges — the assertions below would be meaningless")
	}

	// (i) The cluster grouping must NOT make gA and gB common-cause siblings: they are on DIFFERENT real
	// hypervisors and merely share a logical cluster. Killing mutation: add TypeCluster to siblingParentEligible
	// -> gB appears here -> RED.
	for _, s := range g.Siblings(Entity{Type: TypeVM, Name: "gA"}) {
		if canonName(s.Entity.Name) == canonName("gB") {
			t.Errorf("gB became a common-cause sibling of gA THROUGH the cluster pseudo-node — a logical grouping "+
				"is not a shared failure domain (TG-390); got %+v", s)
		}
	}

	// (ii) A guest whose ONLY parent is the cluster grouping has NO observed placement — HasGroundTruth must be
	// false so TG-202's stay-silent state is reachable. Killing mutation: drop the TypeCluster skip in
	// HasGroundTruth -> the 0.90 cluster edge counts -> RED.
	gc := Entity{Type: TypeVM, Name: "gC"}
	only := NewGraph()
	only.Upsert(Edge{From: gc, To: Entity{Type: TypeCluster, Name: "dc1-pve"}, Rel: RelMemberOf, Source: SourceNetbox, Confidence: 0.90})
	if only.HasGroundTruth(gc) {
		t.Error("a guest whose only edge is cluster membership must NOT have ground truth — a 0.90 cluster edge " +
			"masking the hole is exactly the TG-390 defect (11h of confident-but-blind placement)")
	}
	// CONTROL: a real 0.95 runs_on edge IS ground truth — proves the exclusion is scoped to clusters, not
	// vacuously always-false.
	only.Upsert(Edge{From: gc, To: Entity{Type: TypePVENode, Name: "dc1pve01"}, Rel: RelRunsOn, Source: SourcePVE, Confidence: 0.95})
	if !only.HasGroundTruth(gc) {
		t.Error("a guest with a real 0.95 runs_on edge must have ground truth — the cluster exclusion over-reached")
	}
}
