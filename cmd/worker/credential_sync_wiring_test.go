package main

import (
	"os"
	"strings"
	"testing"
)

// Composition-root guard (TG-501): pin that wireCredentialSync still wires the credential engine's
// boot-time sync + optional scheduled re-sync (TG-109) — the initial SyncAll, the starved-source
// reporting, the per-source "Sync now" seam, and the TG_CREDENTIAL_SYNC_INTERVAL ticker loop — so the
// god-file carve that extracted it from main() cannot silently drop a piece. It returns a function value
// consumed only far downstream (the credentialsync activity), so — the same reasoning
// worker_wiring_inventory_test.go and worker_model_budget_test.go rely on — the guard reads the source as
// text and asserts the wiring, rather than exercising a live credential sync.
func TestWireCredentialSyncWiresTheEngine(t *testing.T) {
	src, err := os.ReadFile("credential_sync_wiring.go")
	if err != nil {
		t.Fatalf("read credential_sync_wiring.go: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		`runs, serr := credEngine.SyncAll(sctx)`,
		`run, err := credEngine.Sync(sctx, sourceID)`,
		`credCoverage[r.SourceID] += r.Added - r.Removed`,
		`publishCredentialState(runs, cov)`,
		`getenv("TG_CREDENTIAL_SYNC_INTERVAL", "")`,
		`return credentialSyncOne`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wireCredentialSync no longer wires %q — a credential-sync piece was dropped in the carve", want)
		}
	}
}

// main() must actually CALL the extracted constructor — a carve that leaves it uncalled is a dark seam
// (present in the tree, absent from the process; the same class TG-315's authlog collector shipped as).
func TestMainCallsWireCredentialSync(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(mainSrc), "credentialSyncOne := wireCredentialSync(credEngine, credSources, credCoverage, publishCredentialState)") {
		t.Error("main.go no longer calls wireCredentialSync(credEngine, credSources, credCoverage, publishCredentialState) — the extracted credential-sync wiring is unreferenced")
	}
}
