package main

import (
	"os"
	"strings"
	"testing"
)

// Composition-root guard (TG-501): pin that wireNoveltyWriteback still wires the full live close-out
// feeder (TG-124) — the confirmed-clean distill through lessons.Merge, the atomic persist, and the
// reload-both-stores refresh — so the god-file carve that extracted it from main() cannot silently drop a
// step. It returns nothing observable from outside the package (deps.LearnResolved is a fire-and-forget
// callback field, exercised only from a live Temporal activity), so — the same reasoning
// worker_wiring_inventory_test.go and worker_model_budget_test.go rely on — the guard reads the source as
// text and asserts the wiring, rather than exercising a live corpus file.
func TestWireNoveltyWritebackWiresTheCloseOutFeeder(t *testing.T) {
	src, err := os.ReadFile("novelty_writeback_wiring.go")
	if err != nil {
		t.Fatalf("read novelty_writeback_wiring.go: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		`if knowledgeHolder != nil && corpusPath != "" {`,
		`deps.LearnResolved = func(_ context.Context, ri lessons.ResolvedIncident) error {`,
		`merged, added := lessons.Merge(existing, []lessons.ResolvedIncident{ri})`,
		`if flags := lessons.ScreenedTags(inc); len(flags) > 0 {`,
		`if werr := persistCorpus(existing, merged); werr != nil {`,
		`knowledgeHolder.Set(loadCorpus())`,
		`syncEmbed()`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wireNoveltyWriteback no longer wires %q — a novelty-writeback step was dropped in the carve", want)
		}
	}
}

// main() must actually CALL the extracted constructor — a carve that leaves it uncalled is a dark seam
// (present in the tree, absent from the process; the same class TG-315's authlog collector shipped as).
func TestMainCallsWireNoveltyWriteback(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(mainSrc), "wireNoveltyWriteback(&deps, knowledgeHolder, corpusPath, &lessonsMu, persistCorpus, loadCorpus, syncEmbed)") {
		t.Error("main.go no longer calls wireNoveltyWriteback(&deps, knowledgeHolder, corpusPath, &lessonsMu, persistCorpus, loadCorpus, syncEmbed) — the extracted novelty-writeback wiring is unreferenced")
	}
}
