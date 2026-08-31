package main

import (
	"os"
	"strings"
	"testing"
)

// Composition-root guard (TG-501): pin that wireCoOccurrencePersist still wires BOTH halves — the boot-time
// snapshot restore and the periodic persistence (including the "0"/"off" explicit-disable switch) — so the
// god-file carve that extracted it from main() cannot silently drop one. Persistence is a fire-and-forget
// background loop gated on an interval and not observable from outside the package, so — the same reasoning
// worker_wiring_inventory_test.go and worker_model_budget_test.go rely on — the guard reads the source as
// text and asserts the wiring, rather than exercising a live co-occurrence store.
func TestWireCoOccurrencePersistWiresRestoreAndPersist(t *testing.T) {
	src, err := os.ReadFile("cooccurrence_persist_wiring.go")
	if err != nil {
		t.Fatalf("read cooccurrence_persist_wiring.go: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		`coStore := db.NewCoOccurrenceStore(pool)`,
		`learner.Restore(snap)`,
		`raw := strings.TrimSpace(getenv("TG_LEARN_PERSIST_INTERVAL", "")); raw == "0" || raw == "off"`,
		`persistEvery := envDuration("TG_LEARN_PERSIST_INTERVAL", 15*time.Minute)`,
		`coStore.Save(context.Background(), learner.Snapshot())`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wireCoOccurrencePersist no longer wires %q — a co-occurrence persistence piece was dropped in the carve", want)
		}
	}
}

// main() must actually CALL the extracted constructor — a carve that leaves it uncalled is a dark seam
// (present in the tree, absent from the process; the same class TG-315's authlog collector shipped as).
func TestMainCallsWireCoOccurrencePersist(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(mainSrc), "wireCoOccurrencePersist(pool, learner)") {
		t.Error("main.go no longer calls wireCoOccurrencePersist(pool, learner) — the extracted co-occurrence persistence wiring is unreferenced")
	}
}
