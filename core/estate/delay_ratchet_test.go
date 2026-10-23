package estate

import "testing"

// TG-188 s2d — on a shared edge key, the propagation delay must follow the WINNING provenance, not the last
// writer: a lower-confidence LEARNED delay must never overwrite a higher-confidence ground-truth CHAOS delay.
func TestUpsertDelayFollowsWinningProvenance(t *testing.T) {
	from, to, rel := Entity{Type: TypeHost, Name: "dep"}, Entity{Type: TypeHost, Name: "root"}, RelDependsOn

	// chaos first (0.90, delay 100), then a learned re-seed (0.75, delay 200) — the chaos delay must SURVIVE
	// (this is the exact order-dependent overwrite s2d fixes; last-writer-wins would corrupt it to 200).
	g1 := NewGraph()
	g1.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceChaos, Confidence: 0.90, DelaySeconds: 100})
	g1.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceIncident, Confidence: 0.75, DelaySeconds: 200})
	if got := g1.edges[edgeKey(from, to, rel)].DelaySeconds; got != 100 {
		t.Errorf("after a learned re-seed over a chaos edge, DelaySeconds = %v, want 100 (the ground-truth chaos delay, not the learned 200)", got)
	}

	// learned first (0.75, delay 200), then chaos (0.90, delay 100) — chaos wins the confidence AND the delay.
	g2 := NewGraph()
	g2.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceIncident, Confidence: 0.75, DelaySeconds: 200})
	g2.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceChaos, Confidence: 0.90, DelaySeconds: 100})
	if got := g2.edges[edgeKey(from, to, rel)].DelaySeconds; got != 100 {
		t.Errorf("after a chaos edge over a learned edge, DelaySeconds = %v, want 100 (chaos wins the delay)", got)
	}

	// a same-source re-seed still refreshes the measured delay (the winner-or-tie path).
	g3 := NewGraph()
	g3.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceIncident, Confidence: 0.75, DelaySeconds: 120})
	g3.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceIncident, Confidence: 0.75, DelaySeconds: 150})
	if got := g3.edges[edgeKey(from, to, rel)].DelaySeconds; got != 150 {
		t.Errorf("a same-source re-seed did not refresh the delay: got %v, want 150", got)
	}
}
