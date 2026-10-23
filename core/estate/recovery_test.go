package estate

import (
	"context"
	"testing"
	"time"
)

// TG-188 slice 2c — recovery time / MTTR rides the estate graph as Edge.RecoverySeconds, fed by the chaos
// tier's ground-truth cascades. These mirror the slice-2 delay tests: the chaos carrier, the winning-provenance
// ratchet, and the Export round-trip a consumer reads.

// THE DoD's NAMED TEST: a chaos ledger row carrying a recovery time produces a non-zero Edge.RecoverySeconds.
// RED before chaos.go carries MeanRecoverySeconds onto the edge (RecoverySeconds would be 0); GREEN after.
// Killing mutation: remove `RecoverySeconds: c.MeanRecoverySeconds` from ChaosSource.Edges and this reddens.
func TestChaosSourceCarriesGroundTruthRecovery(t *testing.T) {
	injectedAt := time.Date(2026, 8, 12, 3, 0, 0, 0, time.UTC)
	src := NewChaosSource(chaosLoaderFrom(ChaosCascade{
		Root: "root", Downstream: "down", Injections: 2, LatestInjectedAt: injectedAt, MeanRecoverySeconds: 900,
	}))
	edges, err := src.Edges(context.Background())
	if err != nil || len(edges) != 1 {
		t.Fatalf("Edges: %v, %d edges (want 1)", err, len(edges))
	}
	if edges[0].RecoverySeconds != 900 {
		t.Errorf("chaos edge RecoverySeconds = %v, want 900 (the ground-truth MTTR carried onto the edge)", edges[0].RecoverySeconds)
	}
	// A cascade with NO measured recovery leaves RecoverySeconds 0 (unmeasured), never a fabricated instant.
	src0 := NewChaosSource(chaosLoaderFrom(ChaosCascade{Root: "r", Downstream: "d", Injections: 1, LatestInjectedAt: injectedAt}))
	e0, err := src0.Edges(context.Background())
	if err != nil || len(e0) != 1 {
		t.Fatalf("Edges: %v, %d edges (want 1)", err, len(e0))
	}
	if e0[0].RecoverySeconds != 0 {
		t.Errorf("unmeasured cascade RecoverySeconds = %v, want 0 (no observed recovery is 'no estimate', not 0s MTTR)", e0[0].RecoverySeconds)
	}
}

// The recovery time follows the WINNING provenance on the same rule as the delay: a lower-confidence LEARNED
// recovery must never overwrite a higher-confidence ground-truth CHAOS recovery on a shared edge key. Killing
// mutation: drop the RecoverySeconds ratchet block in Upsert and the learned re-seed corrupts the chaos MTTR.
func TestUpsertRecoveryFollowsWinningProvenance(t *testing.T) {
	from, to, rel := Entity{Type: TypeHost, Name: "dep"}, Entity{Type: TypeHost, Name: "root"}, RelDependsOn

	// chaos first (0.90, recovery 300), then a learned re-seed (0.75, recovery 60) — the chaos MTTR must SURVIVE
	// (last-writer-wins would corrupt it to 60).
	g1 := NewGraph()
	g1.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceChaos, Confidence: 0.90, RecoverySeconds: 300})
	g1.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceIncident, Confidence: 0.75, RecoverySeconds: 60})
	if got := g1.edges[edgeKey(from, to, rel)].RecoverySeconds; got != 300 {
		t.Errorf("after a learned re-seed over a chaos edge, RecoverySeconds = %v, want 300 (the ground-truth chaos MTTR, not the learned 60)", got)
	}

	// learned first, then chaos — chaos wins the confidence AND the recovery.
	g2 := NewGraph()
	g2.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceIncident, Confidence: 0.75, RecoverySeconds: 60})
	g2.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceChaos, Confidence: 0.90, RecoverySeconds: 300})
	if got := g2.edges[edgeKey(from, to, rel)].RecoverySeconds; got != 300 {
		t.Errorf("after a chaos edge over a learned edge, RecoverySeconds = %v, want 300 (chaos wins the MTTR)", got)
	}

	// a same-source re-seed still refreshes the measured recovery (the winner-or-tie path).
	g3 := NewGraph()
	g3.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceIncident, Confidence: 0.75, RecoverySeconds: 120})
	g3.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceIncident, Confidence: 0.75, RecoverySeconds: 150})
	if got := g3.edges[edgeKey(from, to, rel)].RecoverySeconds; got != 150 {
		t.Errorf("a same-source re-seed did not refresh the recovery: got %v, want 150", got)
	}

	// a 0 (unmeasured) recovery never clobbers a measured one — same discipline as the delay ratchet.
	g4 := NewGraph()
	g4.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceChaos, Confidence: 0.90, RecoverySeconds: 300})
	g4.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceChaos, Confidence: 0.90, RecoverySeconds: 0})
	if got := g4.edges[edgeKey(from, to, rel)].RecoverySeconds; got != 300 {
		t.Errorf("a 0 (unmeasured) recovery clobbered a measured one: got %v, want 300", got)
	}
}

// A consumer READS the MTTR: it must survive Build → Export onto the published snapshot edge. Killing mutation:
// drop RecoverySeconds from Export()'s SnapshotEdge and this reddens.
func TestRecoverySecondsRoundTripsThroughExport(t *testing.T) {
	injectedAt := time.Date(2026, 8, 12, 3, 0, 0, 0, time.UTC)
	src := NewChaosSource(chaosLoaderFrom(ChaosCascade{
		Root: "root1", Downstream: "down1", Injections: 1, LatestInjectedAt: injectedAt, MeanRecoverySeconds: 720,
	}))
	g, errs := Build(context.Background(), []EdgeSource{src})
	if len(errs) != 0 {
		t.Fatalf("build errors: %v", errs)
	}
	var found bool
	for _, se := range g.Export().Edges {
		if se.FromName == "down1" && se.ToName == "root1" {
			found = true
			if se.RecoverySeconds != 720 {
				t.Errorf("exported edge RecoverySeconds = %v, want 720 (the MTTR must survive Export to a consumer)", se.RecoverySeconds)
			}
		}
	}
	if !found {
		t.Fatal("down1 depends_on root1 edge missing from Export")
	}
}

// TG-188 organic recovery: the LEARNED tier now carries the dependent's observed MTTR too — a co-occurrence
// with MeanRecoverySeconds produces a learned edge with RecoverySeconds. Killing mutation: drop the
// RecoverySeconds mapping from LearnedSource.Edges and this reddens. The chaos-outranks-learned property on a
// shared key is already pinned by TestUpsertRecoveryFollowsWinningProvenance above.
func TestLearnedSourceCarriesOrganicRecovery(t *testing.T) {
	src := NewLearnedSource([]CoOccurrence{{Primary: "root", Dependent: "dep", Count: 3, MeanRecoverySeconds: 480}})
	edges, err := src.Edges(context.Background())
	if err != nil || len(edges) != 1 {
		t.Fatalf("Edges: %v, %d edges (want 1)", err, len(edges))
	}
	if edges[0].RecoverySeconds != 480 {
		t.Errorf("learned edge RecoverySeconds = %v, want 480 (the dependent's organic MTTR)", edges[0].RecoverySeconds)
	}
	// An unlearned recovery stays 0 (absent-is-not-zero).
	src0 := NewLearnedSource([]CoOccurrence{{Primary: "r", Dependent: "d", Count: 3}})
	e0, _ := src0.Edges(context.Background())
	if len(e0) != 1 || e0[0].RecoverySeconds != 0 {
		t.Errorf("count-only co-occurrence produced RecoverySeconds = %v, want 0", e0[0].RecoverySeconds)
	}
}
