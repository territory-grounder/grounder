package modules

import (
	"errors"
	"testing"
)

// TestDiscoverySurfaceIsRegistrableAndGated pins the INV-17 property for the estate-discovery surface
// (spec/027 REQ-2701): a discovery module that is unregistered OR disabled has no execution path, so an
// operator who switches a source off can be CERTAIN it stopped observing — rather than trusting that
// nothing happens to call it.
func TestDiscoverySurfaceIsRegistrableAndGated(t *testing.T) {
	adapter := struct{ name string }{"systemd-discovery"}
	r := NewRegistry()

	if _, err := r.Resolve(SurfaceDiscovery, "systemd-discovery"); !errors.Is(err, ErrNoExecutionPath) {
		t.Fatalf("an UNREGISTERED discovery source must have no execution path, got %v", err)
	}
	if err := r.Register(Registration{
		Surface: SurfaceDiscovery, SourceType: "systemd-discovery",
		Capability: "discovery.systemd", Adapter: adapter, Enabled: false,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := r.Resolve(SurfaceDiscovery, "systemd-discovery"); !errors.Is(err, ErrNoExecutionPath) {
		t.Fatalf("a DISABLED discovery source must have no execution path, got %v", err)
	}
	if err := r.SetEnabled(SurfaceDiscovery, "systemd-discovery", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := r.Resolve(SurfaceDiscovery, "systemd-discovery"); err != nil {
		t.Fatalf("an enabled discovery source must resolve, got %v", err)
	}
	// INV-18 holds on this surface too: exactly one implementation per (surface, source type).
	if err := r.Register(Registration{
		Surface: SurfaceDiscovery, SourceType: "systemd-discovery", Adapter: adapter, Enabled: true,
	}); !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("a second systemd-discovery registration must be refused (INV-18), got %v", err)
	}
	// Both discovery sources coexist — INV-18 scopes to the (surface, source type) pair, not the surface.
	if err := r.Register(Registration{
		Surface: SurfaceDiscovery, SourceType: "docker-discovery",
		Capability: "discovery.docker", Adapter: adapter, Enabled: true,
	}); err != nil {
		t.Fatalf("the docker discovery source must register alongside systemd, got %v", err)
	}
}
