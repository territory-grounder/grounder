package retrievaleval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	knowledge "github.com/territory-grounder/grounder/core/knowledge"
)

// loadSeedCorpus reads the same shipped corpus (deploy/knowledge/corpus.seed.json) the worker serves, so
// this metric measures the retriever over reality. eval/retrieval is depth-2 like core/knowledge, so the
// ../../ hop to the repo root is identical to saturation_test.go's shared helper.
func loadSeedCorpus(t *testing.T) []knowledge.Incident {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "knowledge", "corpus.seed.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shipped seed corpus: %v", err)
	}
	var corpus []knowledge.Incident
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parse shipped seed corpus: %v", err)
	}
	return corpus
}

// TestResolutionRecall_HonestRetrievalQuality is TG-491's honest retrieval-quality measurement: leave-one-out
// resolution-recall over the shipped corpus. It uses each incident's OWN recorded resolution as ground
// truth (a label no one invented, that the retriever never scores on), so it needs no hand-labels and is
// non-circular. It logs the number and its ceiling/gap/circularity-baseline, and asserts only structural
// honesty invariants (recall can't exceed ceiling; of-findable in [0,1]) plus the raise-only ratchet.
func TestResolutionRecall_HonestRetrievalQuality(t *testing.T) {
	corpus := loadSeedCorpus(t)
	if len(corpus) < 10 {
		t.Fatalf("seed corpus too small to measure: %d rows", len(corpus))
	}

	// Production retrieves top-k=3; the resolution-match cutoff is 0.5 shared tokens over the shorter fix.
	r := ResolutionRecall(corpus, 3, 0.5)
	t.Logf("RESOLUTION-RECALL@3 (thr 0.5) over %d-row shipped corpus:", len(corpus))
	t.Logf("  denom(resolution-bearing)=%d  findable(peer-exists)=%d  retrieved=%d", r.Denom, r.Findable, r.Retrieved)
	t.Logf("  recall=%.3f  ceiling=%.3f  OF-FINDABLE=%.3f  gap=%.3f  trivial(host+rule-only)=%.3f",
		r.Recall, r.Ceiling, r.OfFindable, r.Gap, r.TrivialHostRule)

	// Honesty + sanity invariants (not a quality bar — the number is what it is; these only prove the
	// measurement is coherent and un-fudged).
	if r.Denom == 0 {
		t.Fatal("no resolution-bearing incidents in the corpus — nothing to measure")
	}
	if r.Recall > r.Ceiling+1e-9 {
		t.Fatalf("recall %.3f exceeds ceiling %.3f — impossible, the metric is broken", r.Recall, r.Ceiling)
	}
	if r.Findable > 0 && (r.OfFindable < -1e-9 || r.OfFindable > 1+1e-9) {
		t.Fatalf("of-findable %.3f out of [0,1]", r.OfFindable)
	}

	// RATCHET (mirrors saturation_test.go's discipline). Of-findable resolution-recall@3 — of the
	// incidents whose fix IS recoverable, the fraction the retriever surfaced — is the honest quality
	// number, and it may only RISE. RAISE this floor when a change genuinely improves recall; NEVER LOWER
	// it to make a build pass — lowering is the ratchet failing, and the reason to lower it is always the
	// reason not to. Measured 2026-08-16: 0.933 (112/120 findable) over the 140-row shipped seed.
	const ofFindableFloor = 0.90
	if r.OfFindable < ofFindableFloor {
		t.Errorf("of-findable resolution-recall@3 = %.3f regressed below the %.2f ratchet floor (measured 0.933)", r.OfFindable, ofFindableFloor)
	}
	// The extra channels (summary/tags/recency) must never make retrieval WORSE than a host+rule-only
	// ranker — if they did, they would be injecting noise, not signal. Today they earn ~+2.5 points
	// (0.933 vs 0.908 trivial), which is also the honest measure of how much this retriever adds beyond
	// deterministic exact-match — the very "is it circular?" question TG-491 was right to worry about.
	if r.OfFindable < r.TrivialHostRule-1e-9 {
		t.Errorf("full retriever of-findable %.3f is WORSE than host+rule-only %.3f — the extra channels are hurting", r.OfFindable, r.TrivialHostRule)
	}

	// The k / threshold curve, for transparency — so a reader sees the sensitivity, not one hand-picked cell.
	t.Log("  --- sensitivity curve (of-findable recall) ---")
	for _, k := range []int{1, 3, 5} {
		for _, thr := range []float64{0.34, 0.5, 0.67} {
			rr := ResolutionRecall(corpus, k, thr)
			t.Logf("    k=%d thr=%.2f: of-findable=%.3f  (findable=%d/%d)  gap=%.3f  trivial=%.3f",
				k, thr, rr.OfFindable, rr.Findable, rr.Denom, rr.Gap, rr.TrivialHostRule)
		}
	}
}
