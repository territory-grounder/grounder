package main

import (
	"os"
	"strings"
	"testing"
)

// TG-346. The relay type (core/estate.SnapshotRelaySource) has its own oracles; what they cannot see is
// whether main.go actually WIRES it — this repo's recurring defect is a guarded resolver whose call site
// goes unguarded, and this session alone produced seven mutation survivals of that exact shape. These
// are comment-stripped, window-scoped source assertions on the composition root.

// estateRelayWindow returns the comment-stripped span of main.go from the relay arming to the initial
// Build — the window every wiring property below must hold inside.
func estateRelayWindow(t *testing.T) (full, armWindow string) {
	t.Helper()
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	full = stripGoComments(string(raw))
	if len(full) < 10_000 {
		t.Fatalf("VACUITY FLOOR: main.go stripped to %d bytes", len(full))
	}
	i := strings.Index(full, "estateRelayArmed := credentialPlane == credential.ProcessPlaneActuation")
	j := strings.Index(full, "initialGraph, estateErrs := estate.Build")
	if i < 0 {
		t.Fatal("the relay arming line is gone from main.go. Without it the actuation plane's gate is back " +
			"to reasoning over the 17 edges its own credentials can see (TG-346), and the " +
			"EstateGraphDivergesBetweenPlanes alert re-fires with nothing in CI to say why.")
	}
	if j < 0 || j < i {
		t.Fatal("estate.Build no longer follows the relay arming — the source is appended after the build " +
			"consumed the list, which wires nothing")
	}
	return full, full[i:j]
}

func TestActuationPlaneArmsTheSnapshotRelay(t *testing.T) {
	_, w := estateRelayWindow(t)
	if !strings.Contains(w, "estate.SnapshotRelaySource{") {
		t.Fatal("the arming window no longer appends estate.SnapshotRelaySource — the relay type exists " +
			"and nothing composes it (the resolver-vs-wiring defect, again)")
	}
	if !strings.Contains(w, "TG_ESTATE_SNAPSHOT_RELAY_MAX_AGE") {
		t.Error("the staleness bound is no longer operator-tunable (TG_ESTATE_SNAPSHOT_RELAY_MAX_AGE) — a " +
			"hardcoded bound cannot be widened during a long triage-plane outage without a rebuild")
	}
}

func TestRelayLoadsTheTriagePlaneByConstant(t *testing.T) {
	full, _ := estateRelayWindow(t)
	// The loader must ask for the TRIAGE plane's snapshot BY THE TYPED CONSTANT. A recency-only read
	// answers with whichever worker wrote last (the original TG-346 defect); a literal "triage" string
	// would drift silently if the plane names ever change.
	k := strings.Index(full, "LatestSnapshotForPlane(ctx, string(credential.ProcessPlaneTriage))")
	if k < 0 {
		t.Fatal("the relay loader no longer requests the TRIAGE plane's snapshot via " +
			"LatestSnapshotForPlane(ctx, string(credential.ProcessPlaneTriage)). Either the plane filter " +
			"was dropped (the reader then gets whichever plane wrote last — on this plane, usually ITSELF, " +
			"relaying the impoverished graph back and calling it convergence) or the constant became a " +
			"string literal that can drift from the plane enum.")
	}
	// And the binding must be guarded by the SAME arming flag the source append uses — a loader bound
	// unconditionally would run triage-plane reads on the triage worker too.
	window := full[max(0, k-600):k]
	if !strings.Contains(window, "estateRelayArmed") {
		t.Error("the loader binding is no longer gated on estateRelayArmed — the triage plane would " +
			"relay its own snapshot to itself")
	}
}

func TestRelayIsPrimedAfterThePoolConnects(t *testing.T) {
	full, _ := estateRelayWindow(t)
	// Anchored on the SUMMARY line specifically: "estate: relay prime" alone also matches the
	// per-source error log, and a mutation that deleted only the summary (and with it the evidence the
	// prime ran to completion) survived the looser anchor — the contains-matches-a-superstring lesson.
	k := strings.Index(full, "relay prime — graph")
	if k < 0 {
		t.Fatal("the post-connect prime is gone. The initial Build runs before the pool exists, so " +
			"without the prime the actuation plane serves the 17-edge graph until the first " +
			"TG_ESTATE_REFRESH_INTERVAL tick — a window in which the gate under-refuses on exactly the " +
			"plane that mutates the estate.")
	}
	window := full[max(0, k-1200):k]
	if !strings.Contains(window, "estateHolder.Refresh(") {
		t.Errorf("the prime log line exists but no Refresh call precedes it in its window — the line " +
			"announces a prime that never runs")
	}
	if !strings.Contains(window, "LearnedSource()") {
		t.Errorf("the prime refreshes without the learner's co-occurrence source — the primed graph would " +
			"silently drop the learned tier the periodic refresh includes, so the graph SHRINKS at the " +
			"next tick for no observable reason")
	}
}
