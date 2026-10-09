package resolve_test

import (
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/bootstrap"
	"github.com/territory-grounder/grounder/modules/resolve"
)

func newReg(t *testing.T) *modules.Registry {
	t.Helper()
	reg, err := bootstrap.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// TestTypedResolveReturnsConcreteAdapters proves the typed accessors return the concrete surface adapter for
// a registered+enabled source (the INV-17 positive path), with the source identity intact.
func TestTypedResolveReturnsConcreteAdapters(t *testing.T) {
	reg := newReg(t)

	ing, err := resolve.Ingester(reg, "crowdsec")
	if err != nil {
		t.Fatalf("Ingester(crowdsec): %v", err)
	}
	if ing.SourceType() != "crowdsec" {
		t.Errorf("ingester source type = %q, want crowdsec", ing.SourceType())
	}

	exp, err := resolve.Exporter(reg, "prometheus")
	if err != nil {
		t.Fatalf("Exporter(prometheus): %v", err)
	}
	if exp.SourceType() != "prometheus" {
		t.Errorf("exporter source type = %q, want prometheus", exp.SourceType())
	}

	prov, err := resolve.Provider(reg, "zai")
	if err != nil {
		t.Fatalf("Provider(zai): %v", err)
	}
	if prov.Name() != "zai" {
		t.Errorf("provider name = %q, want zai", prov.Name())
	}
}

// TestResolveUnregisteredFailsClosed proves an unregistered source propagates ErrNoExecutionPath (INV-17).
func TestResolveUnregisteredFailsClosed(t *testing.T) {
	reg := newReg(t)
	if _, err := resolve.Ingester(reg, "does-not-exist"); !errors.Is(err, modules.ErrNoExecutionPath) {
		t.Errorf("unregistered ingester must be ErrNoExecutionPath, got: %v", err)
	}
}

// TestResolveDisabledActuatorFailsClosed proves the load-bearing Phase-0/1 property THROUGH the typed seam:
// the actuation family is registered but DISABLED, so resolving any actuator yields no execution path.
func TestResolveDisabledActuatorFailsClosed(t *testing.T) {
	reg := newReg(t)
	for _, src := range []string{"ssh", "kubernetes", "proxmox", "mcp"} {
		if _, err := resolve.Actuator(reg, src); !errors.Is(err, modules.ErrNoExecutionPath) {
			t.Errorf("disabled actuator %q must be ErrNoExecutionPath (INV-17), got: %v", src, err)
		}
	}
}

// TestResolveWrongAdapterTypeFailsClosed proves a registration whose adapter does not implement its surface
// interface (a wiring bug) returns a typed error rather than panicking on the assertion.
func TestResolveWrongAdapterTypeFailsClosed(t *testing.T) {
	reg := modules.NewRegistry()
	if err := reg.Register(modules.Registration{
		Surface:    modules.SurfaceIngest,
		SourceType: "bogus",
		Capability: "ingest.bogus",
		Enabled:    true,
		Adapter:    "not an ingester", // wrong type on purpose
	}); err != nil {
		t.Fatalf("register bogus: %v", err)
	}
	_, err := resolve.Ingester(reg, "bogus")
	if err == nil {
		t.Fatal("resolving a wrong-typed adapter must error, not panic or succeed")
	}
	if errors.Is(err, modules.ErrNoExecutionPath) {
		t.Errorf("a wrong-type error must be distinct from ErrNoExecutionPath, got: %v", err)
	}
}
