package modules

import (
	"errors"
	"reflect"
	"testing"
)

// a non-nil adapter sentinel; the registry only requires Adapter != nil.
var someAdapter = struct{ name string }{name: "x"}

func trackerReg(src string, enabled bool) Registration {
	return Registration{Surface: SurfaceTracker, SourceType: src, Capability: "tracker." + src, Enabled: enabled, Adapter: someAdapter}
}

// INV-17: an unregistered or disabled module has NO execution path; enabling it grants one.
func TestRegistryNoExecutionPathUnlessRegisteredAndEnabled(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Resolve(SurfaceTracker, "youtrack"); !errors.Is(err, ErrNoExecutionPath) {
		t.Fatalf("unregistered module must have no execution path, got %v", err)
	}
	if err := r.Register(trackerReg("youtrack", false)); err != nil {
		t.Fatalf("registering a disabled module must succeed, got %v", err)
	}
	if _, err := r.Resolve(SurfaceTracker, "youtrack"); !errors.Is(err, ErrNoExecutionPath) {
		t.Fatalf("a disabled module must have no execution path, got %v", err)
	}
	if err := r.SetEnabled(SurfaceTracker, "youtrack", true); err != nil {
		t.Fatalf("enabling a registered module must succeed, got %v", err)
	}
	if _, err := r.Resolve(SurfaceTracker, "youtrack"); err != nil {
		t.Fatalf("an enabled registered module must resolve, got %v", err)
	}
	// withdrawing enablement removes the path again.
	if err := r.SetEnabled(SurfaceTracker, "youtrack", false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(SurfaceTracker, "youtrack"); !errors.Is(err, ErrNoExecutionPath) {
		t.Fatalf("a disabled module must lose its execution path, got %v", err)
	}
}

// INV-18: exactly one registered implementation per (surface, source type).
func TestRegistryOneImplementationPerSource(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(trackerReg("youtrack", true)); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(trackerReg("youtrack", true)); !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("a duplicate (surface, source) must be rejected, got %v", err)
	}
	if err := r.Register(trackerReg("jira", true)); err != nil {
		t.Fatalf("a different source type must be accepted, got %v", err)
	}
}

// A registration missing its surface, source type, or adapter is rejected (nothing unidentifiable gets a path).
func TestRegistryRejectsUnidentifiedRegistration(t *testing.T) {
	r := NewRegistry()
	for name, reg := range map[string]Registration{
		"no surface": {SourceType: "youtrack", Adapter: someAdapter},
		"no source":  {Surface: SurfaceTracker, Adapter: someAdapter},
		"no adapter": {Surface: SurfaceTracker, SourceType: "youtrack"},
	} {
		if err := r.Register(reg); !errors.Is(err, ErrUnidentified) {
			t.Fatalf("%s: must be rejected as unidentified, got %v", name, err)
		}
	}
}

// SetEnabled on a module that was never registered cannot conjure an execution path.
func TestSetEnabledOnMissingModuleFails(t *testing.T) {
	r := NewRegistry()
	if err := r.SetEnabled(SurfaceTracker, "ghost", true); !errors.Is(err, ErrNoExecutionPath) {
		t.Fatalf("enabling a never-registered module must fail, got %v", err)
	}
}

// Manifest returns every registered surface/source key (enabled or not), sorted — the reconciler input.
func TestRegistryManifestListsAllRegisteredSorted(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(trackerReg("youtrack", true))
	_ = r.Register(trackerReg("jira", false)) // disabled still appears in the manifest
	_ = r.Register(Registration{Surface: SurfaceNotifier, SourceType: "matrix", Adapter: someAdapter, Enabled: true})
	got := r.Manifest()
	want := []string{"notifier/matrix", "tracker/jira", "tracker/youtrack"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest = %v, want %v (all registered, sorted)", got, want)
	}
}

// Capabilities is the status-aware operator view: it carries enablement so a DISABLED member is visibly
// distinct from a live one, and it sorts by surface then source.
func TestRegistryCapabilitiesCarryEnablementSorted(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(trackerReg("youtrack", true))
	_ = r.Register(Registration{Surface: SurfaceActuation, SourceType: "ssh", Capability: "actuation.ssh", Adapter: someAdapter, Enabled: false})
	got := r.Capabilities()
	want := []Capability{
		{Surface: SurfaceActuation, SourceType: "ssh", Capability: "actuation.ssh", Enabled: false},
		{Surface: SurfaceTracker, SourceType: "youtrack", Capability: "tracker.youtrack", Enabled: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capabilities = %+v, want %+v (enablement carried, sorted by surface then source)", got, want)
	}
}
