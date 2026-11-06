package main

import (
	"strings"
	"testing"
)

// TG-80 (audit tamper-isolation): the governance ledger's write-domain airgap must stay WIRED at boot. The
// ledger + risk-audit sinks select a SEPARATE credential (TG_LEDGER_WRITE_DSN) when it is set — so the runtime
// pool, which can read/write everything else, cannot UPDATE/DELETE the spine — and a set-but-unusable DSN
// fails the boot CLOSED rather than silently falling back to the tamperable pool. This source guard pins all
// three, in the guest_liveness_wire_test.go house pattern (workerMainSource strips comments + a vacuity
// floor), so the airgap can never silently regress into the always-runtime-pool path it replaced.
//
// KILLING MUTATION: change `db.NewLedgerStore(ledgerPool)` back to `db.NewLedgerStore(pool)` — this test
// fails naming the unwired sink. Restore → green.
func TestLedgerAirgapIsWiredFailClosed(t *testing.T) {
	src := workerMainSource(t)
	// the sinks use the airgap-SELECTED pool (ledgerPool), not the raw runtime pool, so an armed airgap
	// actually routes the writes through the separate credential.
	if !strings.Contains(src, "db.NewLedgerStore(ledgerPool)") || !strings.Contains(src, "db.NewRiskAuditStore(ledgerPool)") {
		t.Error("the ledger/risk sinks are not wired to the airgap-selected pool (ledgerPool) — an armed " +
			"TG_LEDGER_WRITE_DSN would not actually route writes through the separate credential (TG-80)")
	}
	if !strings.Contains(src, "TG_LEDGER_WRITE_DSN") {
		t.Error("the ledger write-domain airgap flag (TG_LEDGER_WRITE_DSN) is gone from main() (TG-80)")
	}
	// a set-but-unusable airgap DSN must FAIL THE BOOT CLOSED, never fall back to the tamperable runtime pool.
	if !strings.Contains(src, "refusing to boot with an unusable airgapped ledger sink") {
		t.Error("the fail-closed boot refusal for a bad TG_LEDGER_WRITE_DSN is missing — a broken airgap DSN " +
			"must not silently downgrade to the tamperable pool (TG-80)")
	}
}
