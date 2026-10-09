package main

import (
	"os"
	"strings"
	"testing"
)

// Composition-root guard (TG-501): pin that wireLedgerAnchor still wires BOTH halves of the ledger-anchor
// tamper-evidence pair — the recorder (TG-80 P1#1) and the consumer (TG-509) — so the god-file carve that
// extracted it from main() cannot silently drop one. Neither half returns anything observable from outside
// the package (both are fire-and-forget background loops gated on a DSN), so — the same reasoning
// worker_wiring_inventory_test.go and worker_model_budget_test.go rely on — the guard reads the source as
// text and asserts the wiring, rather than exercising a live database.

func TestWireLedgerAnchorWiresBothHalves(t *testing.T) {
	src, err := os.ReadFile("ledger_anchor_wiring.go")
	if err != nil {
		t.Fatalf("read ledger_anchor_wiring.go: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		`getenv("TG_LEDGER_ANCHOR_INTERVAL"`,
		`go ledgeranchor.RunPeriodically(context.Background(), anchorJob, d,`,
		`getenv("TG_LEDGER_VERIFY_INTERVAL"`,
		`go ledgeranchor.RunVerifyPeriodically(context.Background(), verifyJob, d,`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wireLedgerAnchor no longer wires %q — a ledger-anchor half was dropped in the carve", want)
		}
	}
}

// main() must actually CALL the extracted constructor — a carve that leaves it uncalled is a dark seam
// (present in the tree, absent from the process; the same class TG-315's authlog collector shipped as).
func TestMainCallsWireLedgerAnchor(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(mainSrc), "wireLedgerAnchor(dbPool)") {
		t.Error("main.go no longer calls wireLedgerAnchor(dbPool) — the extracted ledger-anchor wiring is unreferenced")
	}
}
