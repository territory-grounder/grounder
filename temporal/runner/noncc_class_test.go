package runner

import (
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
)

// nonCCSlug names a test-only op-class with start-service's exact SSH shape and NO commit_confirmed declaration.
//
// Since TG-648 every embedded low-reversible SSH class declares a commit-confirm window, so the oracles that need
// "an SSH class without one" — it is credited at the terminus, it never arms, the canary mandate refuses it at
// AUTO_NOTICE — have no embedded class left to stand on. An OVERLAY class is that case in production: a runtime
// ratified row carries no commit_confirmed unless its author declared one. The registry's other classes without a
// window (disk-grow, k8s-set-replicas, stop-guest) do not run the SSH workflow these end-to-end oracles drive.
const nonCCSlug = "tg648-start-unit"

// registerNonCCServiceClass installs nonCCSlug into the overlay for one test (start-service's spec, renamed, with
// the commit-confirm declaration removed) and tears it down after.
func registerNonCCServiceClass(t *testing.T) string {
	t.Helper()
	spec, ok := opschema.Lookup("start-service")
	if !ok {
		t.Fatal("start-service must be registered")
	}
	spec.OpClass = nonCCSlug
	spec.CommitConfirmed = nil
	hash, err := opschema.CanonicalHash(spec)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if accepted, rejected := opschema.SetOverlay([]opschema.OverlayEntry{{Spec: spec, Hash: hash}}); accepted != 1 {
		t.Fatalf("overlay register: accepted=%d rejected=%v", accepted, rejected)
	}
	t.Cleanup(func() { opschema.SetOverlay(nil) })
	if s, ok := opschema.Lookup(nonCCSlug); !ok || s.CommitConfirmed != nil {
		t.Fatalf("precondition: %s must be registered WITHOUT commit_confirmed, got ok=%v cc=%+v", nonCCSlug, ok, s.CommitConfirmed)
	}
	return nonCCSlug
}
