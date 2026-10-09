package main

import (
	"os"
	"strings"
	"testing"
)

// TestSlurpitEstateSourceIsDarkUnlessConfigured asserts the composition-root contract for the Slurp'it estate
// source (TG-91): it is CONSTRUCTED AND REGISTERED ONLY when TG_SLURPIT_URL is set, exactly like NetBox and
// PVE. A behavioural unit test proves the source works; only this proves the worker keeps it dark until an
// operator configures it — the "unconfigured ⇒ unregistered" half a unit test cannot reach.
//
// KILLING MUTATION: move slurpit.New out of the TG_SLURPIT_URL guard (construct it unconditionally). RED —
// slurpit.New would appear before the guard, so an unconfigured worker would build a source pointed at "".
func TestSlurpitEstateSourceIsDarkUnlessConfigured(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	s := string(src)

	if !strings.Contains(s, `"github.com/territory-grounder/grounder/modules/cmdb/slurpit"`) {
		t.Fatal("main.go does not import modules/cmdb/slurpit — the source is unreachable and no Slurp'it " +
			"device could ever reach the estate graph")
	}

	const guard = `if slURL := getenv("TG_SLURPIT_URL", ""); slURL != ""`
	gi := strings.Index(s, guard)
	if gi < 0 {
		t.Fatal("the Slurp'it source is not gated on TG_SLURPIT_URL — it must be dark unless configured, " +
			"exactly like the netbox/pve sources above it")
	}

	// Every construction/registration site must live AFTER the guard, so nothing is built without a URL.
	for _, marker := range []string{
		"slurpit.New(",
		`probeReg.offer("cmdb", slurpit.SourceType`,
		"estateSources = append(estateSources, sl)",
	} {
		mi := strings.Index(s, marker)
		if mi < 0 {
			t.Errorf("main.go is missing the Slurp'it wiring %q", marker)
			continue
		}
		if mi < gi {
			t.Errorf("Slurp'it wiring %q appears BEFORE the TG_SLURPIT_URL guard — an unconfigured worker "+
				"would register the source anyway", marker)
		}
	}
}
