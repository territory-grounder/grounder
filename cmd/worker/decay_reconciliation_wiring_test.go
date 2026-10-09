package main

import (
	"os"
	"strings"
	"testing"
)

// Composition-root guard (TG-501): pin that wireDecayReconciliation still ages all THREE learned stores —
// lessons provenance-prune, core/learn co-occurrence half-life, and estate decay-on-disproof — plus the
// immediate boot pass, so the god-file carve that extracted it from main() cannot silently drop one. It
// returns nothing observable from outside the package (a fire-and-forget background loop gated on an
// interval), so — the same reasoning worker_wiring_inventory_test.go and worker_model_budget_test.go rely
// on — the guard reads the source as text and asserts the wiring, rather than exercising live stores.

func TestWireDecayReconciliationAgesAllThreeStores(t *testing.T) {
	src, err := os.ReadFile("decay_reconciliation_wiring.go")
	if err != nil {
		t.Fatalf("read decay_reconciliation_wiring.go: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		`getenv("TG_DECAY_INTERVAL"`,
		`reconcileLessons()`,
		`learner.Decay(now, learnHalfLife)`,
		`learner.DecayOnDisproof(paths, edgeDecayFactor)`,
		`estateHolder.Graph().DecayOnDisproof(estate.Disproof{Paths: paths, At: now}, estate.DecayOptions{Factor: edgeDecayFactor})`,
		`runDecay() // one reconciliation pass at boot`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wireDecayReconciliation no longer wires %q — a decay step was dropped in the carve", want)
		}
	}
}

// main() must actually CALL the extracted constructor — a carve that leaves it uncalled is a dark seam
// (present in the tree, absent from the process; the same class TG-315's authlog collector shipped as).
func TestMainCallsWireDecayReconciliation(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(mainSrc), "wireDecayReconciliation(dbPool, reconcileLessons, learner, discoveryCorpus, estateHolder, publishEstate)") {
		t.Error("main.go no longer calls wireDecayReconciliation(...) — the extracted decay-reconciliation wiring is unreferenced")
	}
}
