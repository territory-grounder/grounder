package main

import (
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
)

// TG-112, worker half. runtime_posture is keyed on `component` with ON CONFLICT DO UPDATE, and both
// worker processes published the literal "worker" — so the two planes shared ONE row and whichever
// heartbeated last won. Measured on the live database 2026-08-06: exactly one row.
//
// The grounder-side oracles for this live in cmd/grounder/posture_plane_test.go. They are not enough on
// their own: with only those, reverting main.go to publish the bare "worker" literal stays GREEN, because
// the resolver is still correct about a table nothing writes correctly. Verified by mutation.

func TestEachPlanePublishesUnderItsOwnKey(t *testing.T) {
	triage := PostureComponent(credential.ProcessPlaneTriage)
	actuation := PostureComponent(credential.ProcessPlaneActuation)

	if triage == actuation {
		t.Fatalf("both planes publish under %q — they share one runtime_posture row and whichever "+
			"heartbeats last wins, which is the defect this exists to fix", triage)
	}
	for name, got := range map[string]string{"triage": triage, "actuation": actuation} {
		if !strings.Contains(got, name) {
			t.Errorf("the %s plane publishes as %q, which does not name the plane — an operator reading "+
				"runtime_posture cannot tell which process wrote the row", name, got)
		}
	}
}

// TestASingleProcessKeepsTheLegacyKey. plane=both is the pre-split posture, and that deployment must be
// byte-identical to before this change — the grounder's legacy lookup depends on it.
func TestASingleProcessKeepsTheLegacyKey(t *testing.T) {
	if got := PostureComponent(credential.ProcessPlaneBoth); got != "worker" {
		t.Errorf("plane=both publishes as %q, want the bare \"worker\". A single-process deployment must "+
			"keep the legacy key, or the grounder's fallback finds nothing and the console reads unknown.",
			got)
	}
}

// TestThePublishCallIsPlaneQualifiedAtTheCompositionRoot. Guarding PostureComponent is not guarding the
// wiring: main.go can call Publish with a literal and every test above stays green. That mutation
// survived a full round before this test existed.
func TestThePublishCallIsPlaneQualifiedAtTheCompositionRoot(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := stripGoComments(string(raw))

	i := strings.Index(src, "postureStore.Publish(")
	if i < 0 {
		t.Fatal("VACUITY FLOOR: main.go never calls postureStore.Publish — this guard is anchored on a " +
			"call that no longer exists and would otherwise pass while checking nothing")
	}
	// Scope to the call itself: a file-wide search for PostureComponent would be satisfied by any other
	// mention, which is how a guard of mine survived gutting its own call site before.
	end := i + 200
	if end > len(src) {
		end = len(src)
	}
	call := src[i:end]

	if !strings.Contains(call, "PostureComponent(") {
		t.Errorf("postureStore.Publish is called without PostureComponent — the component key is not "+
			"plane-qualified, so both worker planes write the same runtime_posture row again and the "+
			"actuation plane becomes unrepresentable.\ncall site:\n%s", call)
	}
	if strings.Contains(call, `Publish(pctx, "worker"`) {
		t.Error("the publish call passes the literal \"worker\" — that is the defect verbatim")
	}
}
