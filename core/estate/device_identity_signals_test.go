package estate

import "testing"

// TG-78 network+storage slices: the device-identity accessors, and THE ALIASING PIN. NetBox models the
// hypervisor boot-from-NAS relationship against the HYPHENATED name (dc1pve02 runs_on dc1-syno01,
// typed pve_node — a Synology impersonating a hypervisor in netbox's model), while LibreNMS alerts on the
// UNHYPHENATED dc1syno01. canonName only strips DNS suffixes, so the two names never merge — and every
// signal below depends on that staying true: were the names ever normalized together, every DSM alert
// would silently flip from DomainStorage to DomainProxmox.
func deviceIdentityGraph() *Graph {
	g := NewGraph()
	up := func(from Entity, to Entity, rel RelType) {
		g.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceLibreNMS})
	}
	// The LibreNMS dependency topology, typed by os (the new topology stamping).
	up(Entity{Type: TypeStorageAppliance, Name: "dc1syno01"}, Entity{Type: TypeNetworkDevice, Name: "dc1sw01"}, RelDependsOn)
	up(Entity{Type: TypeNetworkDevice, Name: "dc1ap01"}, Entity{Type: TypeNetworkDevice, Name: "dc1sw01"}, RelDependsOn)
	// The netbox hyphenated alias: a pve node "runs on" the NAS under a DIFFERENT spelling.
	g.Upsert(Edge{From: Entity{Type: TypePVENode, Name: "dc1pve02"}, To: Entity{Type: TypePVENode, Name: "dc1-syno01"},
		Rel: RelRunsOn, Source: SourceNetbox})
	// A plain server for the negative arm.
	up(Entity{Type: TypeHost, Name: "dc1plain01"}, Entity{Type: TypeNetworkDevice, Name: "dc1sw01"}, RelDependsOn)
	return g
}

func TestDeviceIdentityAccessors(t *testing.T) {
	g := deviceIdentityGraph()
	if !g.IsNetworkDevice("dc1sw01") || !g.IsNetworkDevice("dc1ap01") {
		t.Error("typed network devices must read IsNetworkDevice=true (either edge end)")
	}
	if !g.IsStorageAppliance("dc1syno01") {
		t.Error("the typed DSM must read IsStorageAppliance=true")
	}
	if g.IsNetworkDevice("dc1plain01") || g.IsStorageAppliance("dc1plain01") {
		t.Error("a plain host must carry neither device identity")
	}
	if g.IsStorageAppliance("dc1sw01") || g.IsNetworkDevice("dc1syno01") {
		t.Error("the two identities must not bleed into each other")
	}
	var nilGraph *Graph
	if nilGraph.IsNetworkDevice("x") || nilGraph.IsStorageAppliance("x") {
		t.Error("a nil graph fails closed on both signals")
	}
}

// The aliasing pin (the storage scout's hazard #1): the hyphenated netbox pve_node alias must NEVER leak
// onto the alerting hostname, in either direction. If a netbox normalization ever merges the spellings,
// this reddens instead of silently re-routing the DSM to Proxmox.
func TestSynoAliasNeverLeaksAcrossNames(t *testing.T) {
	g := deviceIdentityGraph()
	if g.IsPveNode("dc1syno01") {
		t.Fatal("dc1syno01 (the alerting DSM hostname) reads as a PVE node — the netbox hyphenated alias " +
			"(dc1-syno01) has leaked across canonName; every DSM alert would silently route DomainProxmox")
	}
	if !g.IsPveNode("dc1-syno01") {
		t.Error("the hyphenated netbox alias itself must still read as the pve_node netbox typed it")
	}
	if g.IsStorageAppliance("dc1-syno01") {
		t.Error("the appliance identity must not leak onto the hyphenated alias either")
	}
}
