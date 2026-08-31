package main

import (
	"os"
	"strings"
	"testing"
)

// Composition-root guard (TG-501): pin that wireAbandonedDecisionReap still wires the abandoned
// pending-decision sweep — the concrete-store type assertion, the VoteWait-derived deadline, and the
// bounded-context ticker loop — so the god-file carve that extracted it from main() cannot silently drop a
// piece. The sweep is a fire-and-forget background loop and not observable from outside the package, so —
// the same reasoning worker_wiring_inventory_test.go and worker_model_budget_test.go rely on — the guard
// reads the source as text and asserts the wiring, rather than exercising a live reap against Postgres.
func TestWireAbandonedDecisionReapWiresTheSweep(t *testing.T) {
	src, err := os.ReadFile("abandoned_decision_reap_wiring.go")
	if err != nil {
		t.Fatalf("read abandoned_decision_reap_wiring.go: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		`if ps, ok := pendingWriter.(*db.PendingStore); ok && ps != nil {`,
		`reapOlderThan := runner.VoteWait + time.Hour`,
		`t := time.NewTicker(reapEvery)`,
		`ps.ReapAbandoned(sctx, now.Add(-reapOlderThan), now)`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wireAbandonedDecisionReap no longer wires %q — an abandoned-decision sweep piece was dropped in the carve", want)
		}
	}
}

// main() must actually CALL the extracted constructor — a carve that leaves it uncalled is a dark seam
// (present in the tree, absent from the process; the same class TG-315's authlog collector shipped as).
func TestMainCallsWireAbandonedDecisionReap(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(mainSrc), "wireAbandonedDecisionReap(pendingWriter)") {
		t.Error("main.go no longer calls wireAbandonedDecisionReap(pendingWriter) — the extracted abandoned-decision sweep wiring is unreferenced")
	}
}
