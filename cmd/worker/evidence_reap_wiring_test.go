package main

import (
	"os"
	"strings"
	"testing"
)

// Composition-root guard (TG-501): pin that wireEvidenceReap still wires the agent-step evidence retention
// sweep — the clamped retention + interval knobs, the SECURITY DEFINER reap call, and the bounded-context
// ticker loop — so the god-file carve that extracted it from main() cannot silently drop a piece. The sweep
// is a fire-and-forget background loop gated on an interval and not observable from outside the package, so
// — the same reasoning worker_wiring_inventory_test.go and worker_model_budget_test.go rely on — the guard
// reads the source as text and asserts the wiring, rather than exercising a live reap against Postgres.
func TestWireEvidenceReapWiresTheRetentionSweep(t *testing.T) {
	src, err := os.ReadFile("evidence_reap_wiring.go")
	if err != nil {
		t.Fatalf("read evidence_reap_wiring.go: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		`evidenceRetention := db.ClampEvidenceRetention(envDuration("TG_EVIDENCE_RETENTION", db.DefaultEvidenceRetention))`,
		`evidenceReapEvery := envDuration("TG_EVIDENCE_REAP_INTERVAL", 6*time.Hour)`,
		`t := time.NewTicker(evidenceReapEvery)`,
		`evidenceStore.ReapEvidenceOlderThan(sctx, time.Now().UTC().Add(-evidenceRetention), db.DefaultEvidenceReapBatch)`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wireEvidenceReap no longer wires %q — an evidence-retention piece was dropped in the carve", want)
		}
	}
}

// main() must actually CALL the extracted constructor — a carve that leaves it uncalled is a dark seam
// (present in the tree, absent from the process; the same class TG-315's authlog collector shipped as).
func TestMainCallsWireEvidenceReap(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(mainSrc), "wireEvidenceReap(evidenceStore)") {
		t.Error("main.go no longer calls wireEvidenceReap(evidenceStore) — the extracted evidence-retention wiring is unreferenced")
	}
}
