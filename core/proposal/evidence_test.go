package proposal

import (
	"testing"

	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/safety"
)

// TestAuthoredActionEvidenceFloorsAtPollPauseAndNeverLowers — spec/026 REQ-2611 (oracle O-2607's
// floor half). The EXHAUSTIVE truth table over the constitutional band vocabulary: with the
// authored-action floor, every computed band composes to POLL_PAUSE; with no floor, every computed
// band passes through unchanged. Exhaustive because the safe-direction property must hold by
// construction, not by the fixtures we happened to pick.
//
// RED mutation control (executed 2026-07-31): with FloorFor's authored-action mapping removed
// (returning no-floor), the AUTO+authored ⇒ POLL_PAUSE case fails ("authored-action must floor");
// restored green.
func TestAuthoredActionEvidenceFloorsAtPollPauseAndNeverLowers(t *testing.T) {
	authored := ClassifyEvidence([]attribution.Evidence{
		{Domain: "pve", Actor: "root@pam", ActionKind: "vzstop", Target: "dc1excalidraw01"},
	})
	if !authored[EvidenceAuthoredAction] {
		t.Fatalf("a named principal + closed-table verb must classify as authored-action: %v", authored)
	}
	floor, ok := FloorFor(authored)
	if !ok || floor != safety.BandPollPause {
		t.Fatalf("v1 policy: authored-action must floor at POLL_PAUSE, got %v ok=%v", floor, ok)
	}

	bands := []safety.Band{safety.BandPollPause, safety.BandAutoNotice, safety.BandAuto}
	for _, computed := range bands {
		// With the authored-action floor: every computed band lands at POLL_PAUSE.
		if got := ComposeFloor(computed, floor); got != safety.BandPollPause {
			t.Errorf("computed %v + authored-action floor = %v, want POLL_PAUSE", computed, got)
		}
		// With no floor: the computed band passes through untouched.
		noFloor, applies := FloorFor(map[EvidenceClass]bool{})
		if applies {
			t.Fatalf("no evidence classes must mean no floor")
		}
		if got := ComposeFloor(computed, noFloor); got != computed {
			t.Errorf("computed %v + no floor = %v, want unchanged", computed, got)
		}
	}

	// The never-lowers property by construction: composing any pair never yields a LOOSER band than
	// the computed one (Band order is strictness-descending; looser = greater).
	for _, computed := range bands {
		for _, f := range bands {
			if got := ComposeFloor(computed, f); got > computed {
				t.Errorf("floor %v LOWERED computed %v to %v — floors may only raise the bar", f, computed, got)
			}
		}
	}
}

// TestEvidenceClassificationIsConservative — an unknown verb, or a record with no principal, derives NO
// class and therefore no floor: absence of declared policy must fail toward the classifier's own band,
// never toward a manufactured floor (and never toward suppression — nothing in this API can suppress a
// proposal, which is REQ-2609's half of the policy: every function returns a Band, not a veto).
func TestEvidenceClassificationIsConservative(t *testing.T) {
	cases := []attribution.Evidence{
		{Domain: "pve", Actor: "", ActionKind: "vzstop"},          // no principal
		{Domain: "journal", Actor: "root", ActionKind: "login"},   // verb not in the closed table
		{Domain: "netbox", Actor: "svc", ActionKind: "changelog"}, // read-shaped record
	}
	for _, ev := range cases {
		classes := ClassifyEvidence([]attribution.Evidence{ev})
		if len(classes) != 0 {
			t.Errorf("record %+v must derive no evidence class, got %v", ev, classes)
		}
		if _, ok := FloorFor(classes); ok {
			t.Errorf("record %+v must produce no floor", ev)
		}
	}
}
