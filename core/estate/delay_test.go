package estate

import (
	"context"
	"testing"
)

// TG-188 slice 2 — the co-occurrence learner MEASURES propagation delay per pair (MeanDelaySeconds) but had no
// Edge field to carry it, so the measurement was computed and DISCARDED. These pin that the learned delay now
// rides the edge, survives the Export→RestoreEdges relay round-trip (the silent-field-drop guard for the ripple
// across every Edge copy-site), and that Upsert refreshes it without a 0 clobbering a measured value.

func TestLearnedSourceCarriesPropagationDelay(t *testing.T) {
	src := NewLearnedSource([]CoOccurrence{
		{Primary: "root", Dependent: "dep", Count: 5, MeanDelaySeconds: 42.5},
	}, WithMinObservations(1))
	edges, err := src.Edges(context.Background())
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1", len(edges))
	}
	// dep depends_on root (From=dependent, To=primary).
	if edges[0].DelaySeconds != 42.5 {
		t.Errorf("learned edge DelaySeconds = %v, want 42.5 (the learner's MeanDelaySeconds carried onto the edge)", edges[0].DelaySeconds)
	}
}

func TestExportRoundTripPreservesDelay(t *testing.T) {
	// The silent-drop guard: a delay Upserted onto the graph must survive Export→RestoreEdges (the relay path
	// that reconstructs edges on the actuation plane), i.e. SnapshotEdge + Export + RestoreEdges all carry it. A
	// missed copy-site anywhere on that path reddens this.
	g := NewGraph()
	g.Upsert(Edge{From: Entity{Type: TypeHost, Name: "dep"}, To: Entity{Type: TypeHost, Name: "root"}, Rel: RelDependsOn, Source: SourceIncident, Confidence: 0.7, DelaySeconds: 90})
	snap := g.Export()
	if len(snap.Edges) != 1 {
		t.Fatalf("exported %d edges, want 1", len(snap.Edges))
	}
	if snap.Edges[0].DelaySeconds != 90 {
		t.Errorf("SnapshotEdge.DelaySeconds = %v, want 90 (Export must carry the delay)", snap.Edges[0].DelaySeconds)
	}
	restored := snap.RestoreEdges()
	if len(restored) != 1 {
		t.Fatalf("restored %d edges, want 1", len(restored))
	}
	if restored[0].DelaySeconds != 90 {
		t.Errorf("restored Edge.DelaySeconds = %v, want 90 (RestoreEdges must carry it — the relay round-trip must not drop the delay)", restored[0].DelaySeconds)
	}
}

func TestUpsertRefreshesDelayButZeroDoesNotClobber(t *testing.T) {
	g := NewGraph()
	e := Edge{From: Entity{Type: TypeHost, Name: "dep"}, To: Entity{Type: TypeHost, Name: "root"}, Rel: RelDependsOn, Source: SourceIncident, Confidence: 0.7}
	e.DelaySeconds = 30
	g.Upsert(e)
	// A re-seed with 0 delay (an unlearned source, e.g. ground-truth PVE) must NOT clobber the measured 30.
	e.DelaySeconds = 0
	g.Upsert(e)
	if got := g.edges[edgeKey(e.From, e.To, e.Rel)]; got.DelaySeconds != 30 {
		t.Errorf("after re-Upsert with 0 delay, DelaySeconds = %v, want 30 (0 = unlearned, must not clobber a measured value)", got.DelaySeconds)
	}
	// A re-seed with a NEW measured delay refreshes it.
	e.DelaySeconds = 55
	g.Upsert(e)
	if got := g.edges[edgeKey(e.From, e.To, e.Rel)]; got.DelaySeconds != 55 {
		t.Errorf("after re-Upsert with 55, DelaySeconds = %v, want 55 (a measured delay refreshes)", got.DelaySeconds)
	}
}
