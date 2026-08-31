package estate

import "testing"

// IsGuest is the estate-derived guest signal the skill-domain classifier keys proxmox competence on (TG-78).
// A guest is the From of an AUTHORITATIVE runs_on edge; a learned (co-occurrence) runs_on is NOT trusted.
func TestIsGuest(t *testing.T) {
	g := NewGraph()
	g.Upsert(Edge{From: lxc("dc1bookwyrm01"), To: pveNode("dc1pve01"), Rel: RelRunsOn, Confidence: SourceConfidence[SourcePVE], Source: SourcePVE})
	// a member_of edge does NOT make its From a guest
	g.Upsert(Edge{From: Entity{Type: TypeHost, Name: "dc1rtr01"}, To: Entity{Type: TypeSite, Name: "dc1"}, Rel: RelMemberOf, Confidence: 0.9, Source: SourceNetbox})
	// a LEARNED (incident) runs_on must NOT count — guest-ness is a containment fact, not a co-alarm guess
	g.Upsert(Edge{From: lxc("dc1coincident01"), To: pveNode("dc1pve01"), Rel: RelRunsOn, Confidence: 0.5, Source: SourceIncident})

	if !g.IsGuest("dc1bookwyrm01") {
		t.Error("a host that is the From of an authoritative runs_on edge must be a guest")
	}
	// KILLING MUTATION: drop the Source != SourceIncident filter → the learned edge makes this a guest and reddens.
	if g.IsGuest("dc1coincident01") {
		t.Error("a host that is only a LEARNED runs_on From must NOT be a guest (co-alarm guess, not a fact)")
	}
	if g.IsGuest("dc1rtr01") {
		t.Error("a member_of From (a bare host on a site) is not a guest")
	}
	if g.IsGuest("dc1absent01") {
		t.Error("an unknown host is not a guest")
	}
	if (*Graph)(nil).IsGuest("x") {
		t.Error("a nil graph must fail closed to not-a-guest")
	}
	// The REAL blur from the live estate (TG-78 node-plane slice): netbox models dc1pve02 as
	// running on the Synology it boots from — an authoritative runs_on whose From is a HYPERVISOR.
	// KILLING MUTATION: drop the From.Type != TypePVENode exclusion → the node classifies as a guest
	// and a node-DOWN alert composes the guest-lifecycle frame.
	g.Upsert(Edge{From: Entity{Type: TypePVENode, Name: "dc1pve02"}, To: Entity{Type: TypePVENode, Name: "dc1-syno01"}, Rel: RelRunsOn, Confidence: 0.9, Source: SourceNetbox})
	if g.IsGuest("dc1pve02") {
		t.Error("a PVE node with an authoritative runs_on edge (boot-from-NAS modeling) must NOT be a guest")
	}
}

// IsPveNode is the node-plane half of the routing pair: an inventory fact from the entity TYPE at either
// edge end, learned edges excluded, nil graph fails closed (TG-78).
func TestIsPveNode(t *testing.T) {
	g := NewGraph()
	g.Upsert(Edge{From: lxc("dc1bookwyrm01"), To: pveNode("dc1pve01"), Rel: RelRunsOn, Confidence: SourceConfidence[SourcePVE], Source: SourcePVE})
	g.Upsert(Edge{From: pveNode("dc1pve02"), To: Entity{Type: TypePVENode, Name: "dc1-syno01"}, Rel: RelRunsOn, Confidence: 0.9, Source: SourceNetbox})
	// a LEARNED edge naming a pve-typed endpoint must NOT mint node-ness
	g.Upsert(Edge{From: lxc("dc1ghost01"), To: pveNode("dc1pve09"), Rel: RelRunsOn, Confidence: 0.5, Source: SourceIncident})

	if !g.IsPveNode("dc1pve01") {
		t.Error("the To of an authoritative guest runs_on (typed pve_node) is a node")
	}
	if !g.IsPveNode("dc1pve02") {
		t.Error("a pve-typed From (the boot-from-NAS edge) is a node")
	}
	if g.IsPveNode("dc1pve09") {
		t.Error("a node known only from a LEARNED edge is not a node — co-alarm guess, not inventory")
	}
	if g.IsPveNode("dc1bookwyrm01") {
		t.Error("a guest is not a node")
	}
	// KILLING MUTATION: make IsPveNode match by NAME containing "pve" instead of by type → this reddens.
	g.Upsert(Edge{From: lxc("dc1pvebackupviewer01"), To: pveNode("dc1pve01"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	if g.IsPveNode("dc1pvebackupviewer01") {
		t.Error("node-ness comes from the entity TYPE, never from 'pve' appearing in a name")
	}
	if (*Graph)(nil).IsPveNode("x") {
		t.Error("a nil graph must fail closed to not-a-node")
	}
}
