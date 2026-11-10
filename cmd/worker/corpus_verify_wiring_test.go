package main

import (
	"os"
	"strings"
	"testing"
)

// Composition-root guard (TG-501): pin that wireCorpusVerify still wires the consuming half of TG-510 Slice
// A (the periodic re-derive + WARN against the latest recorded witness, plus the immediate boot pass), so
// the god-file carve that extracted it from main() cannot silently drop a step. It returns nothing
// observable from outside the package (a fire-and-forget background loop gated on a witness sink and a
// corpus path), so — the same reasoning worker_wiring_inventory_test.go and worker_model_budget_test.go
// rely on — the guard reads the source as text and asserts the wiring, rather than exercising a live
// database.

func TestWireCorpusVerifyWiresTheConsumingHalf(t *testing.T) {
	src, err := os.ReadFile("corpus_verify_wiring.go")
	if err != nil {
		t.Fatalf("read corpus_verify_wiring.go: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		`getenv("TG_CORPUS_VERIFY_INTERVAL"`,
		`corpusEvidence.anchors(ctx)`,
		`knowledge.VerifyCorpusAgainstAnchor(current, anchors[len(anchors)-1])`,
		`verifyCorpusOnce() // immediate boot pass`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wireCorpusVerify no longer wires %q — a corpus-verify step was dropped in the carve", want)
		}
	}
}

// main() must actually CALL the extracted constructor — a carve that leaves it uncalled is a dark seam
// (present in the tree, absent from the process; the same class TG-315's authlog collector shipped as).
func TestMainCallsWireCorpusVerify(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(mainSrc), "wireCorpusVerify(corpusEvidence, corpusPath)") {
		t.Error("main.go no longer calls wireCorpusVerify(corpusEvidence, corpusPath) — the extracted corpus-verify wiring is unreferenced")
	}
}
