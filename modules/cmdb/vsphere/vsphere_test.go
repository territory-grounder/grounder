package vsphere

import (
	"context"
	"testing"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/estate"
)

// TestEdges_AgainstVcsim exercises the REAL govmomi client path against vcsim — the official in-process
// vCenter simulator — so connect→login→container-view→retrieve→map runs against a genuine vSphere SOAP API,
// not a hand-rolled mock. vcsim's default VPX model seeds a datacenter with hosts and placed VMs; every
// placed, non-template VM must become a vm→runs_on→physical_host edge stamped with the vsphere provenance.
// A ZERO here means the chain is broken (the whole reason to test against a real simulated API, not fixtures).
func TestEdges_AgainstVcsim(t *testing.T) {
	model := simulator.VPX()
	defer model.Remove()
	if err := model.Create(); err != nil {
		t.Fatalf("vcsim create: %v", err)
	}
	server := model.Service.NewServer()
	defer server.Close()

	user := server.URL.User.Username()
	pw, _ := server.URL.User.Password()
	t.Setenv("TG_VSPHERE_TEST_PW", pw)

	src := New(server.URL.Scheme+"://"+server.URL.Host, user,
		config.SecretRef("env:TG_VSPHERE_TEST_PW"), WithInsecureTLS(true))

	edges, err := src.Edges(context.Background())
	if err != nil {
		t.Fatalf("Edges against vcsim failed: %v", err)
	}
	if len(edges) == 0 {
		t.Fatal("vcsim's default inventory has placed VMs, yet the source produced ZERO runs_on edges — " +
			"the connect→view→retrieve→map chain is broken")
	}
	for _, e := range edges {
		if e.From.Type != estate.TypeVM || e.To.Type != estate.TypePhysicalHost ||
			e.Rel != estate.RelRunsOn || e.Source != estate.SourceVsphere {
			t.Fatalf("malformed edge %+v — want vm→runs_on→physical_host stamped vsphere", e)
		}
		if e.From.Name == "" || e.To.Name == "" {
			t.Fatalf("edge with an empty endpoint name: %+v — a blank/guessed node must never be emitted", e)
		}
	}
}

// TestEdgesFrom_SkipsTemplatesAndUnresolved is the pure-mapping killing oracle: only a placed, named,
// non-template VM on a KNOWN host becomes an edge. It drives the exact code the refresh loop runs (edgesFrom),
// so a live vCenter is not needed to pin the four drop conditions.
//
// KILLING MUTATIONS (each turns this RED): remove the template skip → "golden" emits an edge (len 2); remove
// the name check → "" emits; remove the host==nil guard → "orphan" emits (or panics); remove the host==""
// guard → "ghost" emits an edge with a blank To.Name.
func TestEdgesFrom_SkipsTemplatesAndUnresolved(t *testing.T) {
	h1 := types.ManagedObjectReference{Type: "HostSystem", Value: "host-1"}
	unknown := types.ManagedObjectReference{Type: "HostSystem", Value: "host-x"}
	hostByRef := map[types.ManagedObjectReference]string{h1: "esxi-01"}

	mkVM := func(name string, host *types.ManagedObjectReference, template bool) mo.VirtualMachine {
		var v mo.VirtualMachine
		v.Name = name
		v.Runtime = types.VirtualMachineRuntimeInfo{Host: host}
		if template {
			v.Config = &types.VirtualMachineConfigInfo{Template: true}
		}
		return v
	}
	vms := []mo.VirtualMachine{
		mkVM("web-01", &h1, false),     // survives → the one expected edge
		mkVM("golden", &h1, true),      // TEMPLATE — does not run → skipped
		mkVM("", &h1, false),           // no resolvable name → skipped
		mkVM("orphan", nil, false),     // no runtime.host → skipped
		mkVM("ghost", &unknown, false), // host ref absent from the map → skipped (would be a blank host)
	}

	var s EstateSource
	s.expected = []string{"HostDown"}
	edges := s.edgesFrom(vms, hostByRef)

	if len(edges) != 1 {
		t.Fatalf("edgesFrom produced %d edges, want exactly 1 (only web-01 qualifies): %+v", len(edges), edges)
	}
	e := edges[0]
	if e.From.Type != estate.TypeVM || e.From.Name != "web-01" {
		t.Errorf("From = %+v, want vm/web-01", e.From)
	}
	if e.To.Type != estate.TypePhysicalHost || e.To.Name != "esxi-01" {
		t.Errorf("To = %+v, want physical_host/esxi-01", e.To)
	}
	if e.Rel != estate.RelRunsOn || e.Source != estate.SourceVsphere {
		t.Errorf("edge rel/source = %s/%s, want runs_on/vsphere", e.Rel, e.Source)
	}
	if len(e.ExpectedAlerts) != 1 || e.ExpectedAlerts[0] != "HostDown" {
		t.Errorf("ExpectedAlerts = %v, want [HostDown] — per-edge verifier content must pass through", e.ExpectedAlerts)
	}
}
