package main

import (
	"os"
	"strings"
	"testing"
)

// Composition-root guard (TG-501): pin that wireTrackerCreate still wires the entry-ticket creator's
// reconciling pass (TG-490) — the TG_TRACKER_CREATE_PROJECT gate, the triage-plane + database +
// single-tracker guards, the EntryCreator capability assertion, and the filing/resolve-reserved/
// comment-recoveries ticker loop — so the god-file carve that extracted it from main() cannot silently
// drop a piece. It returns nothing observable from outside the package besides a fire-and-forget
// background loop gated on TG_TRACKER_CREATE_PROJECT, so — the same reasoning worker_wiring_inventory_
// test.go and worker_model_budget_test.go rely on — the guard reads the source as text and asserts the
// wiring, rather than exercising a live tracker-create pass.
func TestWireTrackerCreateWiresTheReconcilingPass(t *testing.T) {
	src, err := os.ReadFile("tracker_create_wiring.go")
	if err != nil {
		t.Fatalf("read tracker_create_wiring.go: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		`strings.TrimSpace(getenv("TG_TRACKER_CREATE_PROJECT", ""))`,
		`credentialPlane.HoldsTriage()`,
		`rawTracker.(tracker.EntryCreator)`,
		`entryfile.FileOnce(tctx, entryfile.Config{`,
		`entryfile.ResolveReservedOnce(tctx, entryfile.Config{`,
		`entryfile.CommentRecoveriesOnce(tctx, entryStore, commentTracker, 20)`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wireTrackerCreate no longer wires %q — a tracker-create piece was dropped in the carve", want)
		}
	}
}

// main() must actually CALL the extracted constructor — a carve that leaves it uncalled is a dark seam
// (present in the tree, absent from the process; the same class TG-315's authlog collector shipped as).
func TestMainCallsWireTrackerCreate(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(mainSrc), "wireTrackerCreate(dbPool, trackersByName, trackerSrcs, entryTracker)") {
		t.Error("main.go no longer calls wireTrackerCreate(dbPool, trackersByName, trackerSrcs, entryTracker) — the extracted tracker-create wiring is unreferenced")
	}
}
