package estate

import (
	"context"
	"testing"
	"time"
)

// TG-673 (deep-review F13): chaos edges are evidence about a fault WE caused, not observed topology. They used
// to count as the judge's ground truth at 0.90, feed prediction after ONE injection, and never decay when later
// injections of the same root did NOT cascade. Three fixes, each pinned here.

// (1) A chaos-only edge is not ground truth: the judge must stay SILENT about an entity whose only live edge is
// a chaos cascade, exactly as it does for a cluster pseudo-node (TG-390).
func TestChaosEdgeIsNotGroundTruth(t *testing.T) {
	g := NewGraph()
	now := time.Now()
	g.Upsert(Edge{From: Entity{Type: TypeHost, Name: "app1"}, To: Entity{Type: TypeHost, Name: "pve3"}, Rel: RelDependsOn,
		Source: SourceChaos, Confidence: SourceConfidence[SourceChaos], ValidUntil: now.Add(time.Hour)})
	if g.HasGroundTruth(Entity{Type: TypeHost, Name: "app1"}) || g.HasGroundTruth(Entity{Type: TypeHost, Name: "pve3"}) {
		t.Fatal("an entity whose only live edge is a chaos cascade has NO observed topology — HasGroundTruth must be false")
	}
	// Control: an observed PVE edge on the same pair IS ground truth.
	g.Upsert(Edge{From: Entity{Type: TypeLXC, Name: "app1"}, To: Entity{Type: TypePVENode, Name: "pve3"}, Rel: RelRunsOn,
		Source: SourcePVE, Confidence: SourceConfidence[SourcePVE], ValidUntil: now.Add(time.Hour)})
	if !g.HasGroundTruth(Entity{Type: TypeHost, Name: "app1"}) {
		t.Fatal("control: an observed hypervisor edge is ground truth")
	}
}

// (2) Two distinct injections before a pair becomes a chaos edge (DefaultChaosMinInjections, aligned with the
// TG-655 recorded decision), and (3) disproof decay: the edge's confidence is scaled by how often the root's
// injections actually cascaded, so a pair that followed 2 of 10 injections sinks below the ground-truth cutoff
// instead of standing at 0.90 forever.
func TestChaosSourceRequiresTwoInjectionsAndDecaysOnDisproof(t *testing.T) {
	if DefaultChaosMinInjections != 2 {
		t.Fatalf("DefaultChaosMinInjections = %d, want 2 (TG-655 recorded decision, TG-673)", DefaultChaosMinInjections)
	}
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	once := ChaosCascade{Root: "pve3", Downstream: "app1", Injections: 1, LatestInjectedAt: at}
	twice := ChaosCascade{Root: "pve3", Downstream: "app2", Injections: 2, LatestInjectedAt: at}
	src := NewChaosSource(chaosLoaderFrom(once, twice))
	edges, err := src.Edges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].From.Name != "app2" {
		t.Fatalf("one injection is a coincidence, two are a pattern: want only app2, got %+v", edges)
	}
	if edges[0].Confidence != 0 {
		t.Fatalf("without a root-injection count the edge leaves confidence 0 for Build's 0.90 default, got %v", edges[0].Confidence)
	}

	// With the denominator: app2 followed 2 of 2 injections (full confidence, Build default); app3 followed 2
	// of 10 (0.90 * 0.2 = 0.18 — below the ground-truth cutoff and the learned cap, so it stops feeding
	// blast-radius prediction as if it were topology).
	roots := map[string]int{"pve3": 2, "pve4": 10}
	src = NewChaosSource(chaosLoaderFrom(twice, ChaosCascade{Root: "pve4", Downstream: "app3", Injections: 2, LatestInjectedAt: at}),
		WithChaosRootInjections(func(context.Context) (map[string]int, error) { return roots, nil }))
	edges, err = src.Edges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 {
		t.Fatalf("want 2 edges, got %d", len(edges))
	}
	byHost := map[string]Edge{}
	for _, e := range edges {
		byHost[e.From.Name] = e
	}
	if byHost["app2"].Confidence != 0 {
		t.Fatalf("a pair that cascaded on every injection keeps the full default confidence, got %v", byHost["app2"].Confidence)
	}
	got := byHost["app3"].Confidence
	if got < 0.17 || got > 0.19 {
		t.Fatalf("2 of 10 injections cascaded ⇒ 0.90 × 0.2 = 0.18, got %v", got)
	}
	if got >= GroundTruthCutoff {
		t.Fatalf("a disproved chaos edge must fall below the ground-truth cutoff, got %v", got)
	}
}

// A denominator that cannot be read is NOT a reason to drop the tier: the edges keep the default confidence
// and the error is surfaced as a load error only when the cascades themselves failed.
func TestChaosRootInjectionsUnreadableKeepsDefaultConfidence(t *testing.T) {
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	src := NewChaosSource(chaosLoaderFrom(ChaosCascade{Root: "pve3", Downstream: "app2", Injections: 3, LatestInjectedAt: at}),
		WithChaosRootInjections(func(context.Context) (map[string]int, error) { return nil, context.DeadlineExceeded }))
	edges, err := src.Edges(context.Background())
	if err != nil || len(edges) != 1 || edges[0].Confidence != 0 {
		t.Fatalf("an unreadable denominator leaves the default confidence: edges=%+v err=%v", edges, err)
	}
}

// A DECAYED chaos edge never relabels or overwrites a stronger incumbent on the same key (fresh-eyes review of
// this change): before the decay every chaos edge sat at 0.90 and the chaos-wins-at-a-tie rule in Upsert was
// unreachable below the seed ranks; a 0.18 edge must leave a declared 0.85 edge declared (so HasGroundTruth
// still sees it) and keep its measured delay.
func TestDecayedChaosEdgeNeverOutranksADeclaredEdge(t *testing.T) {
	g := NewGraph()
	now := time.Now()
	declared := Edge{From: Entity{Type: TypeHost, Name: "app1"}, To: Entity{Type: TypeHost, Name: "pve3"}, Rel: RelDependsOn,
		Source: SourceDeclared, Confidence: SourceConfidence[SourceDeclared], DelaySeconds: 30, ValidUntil: now.Add(time.Hour)}
	g.Upsert(declared)
	g.Upsert(Edge{From: declared.From, To: declared.To, Rel: RelDependsOn, Source: SourceChaos, Confidence: 0.18, DelaySeconds: 99, ValidUntil: now.Add(time.Hour)})
	var got SnapshotEdge
	for _, e := range g.Export().Edges {
		if e.FromName == "app1" && e.ToName == "pve3" {
			got = e
		}
	}
	if got.Source != string(SourceDeclared) || got.Confidence != SourceConfidence[SourceDeclared] || got.DelaySeconds != 30 {
		t.Fatalf("a decayed chaos edge must not relabel or overwrite the declared incumbent, got %+v", got)
	}
	if !g.HasGroundTruth(Entity{Type: TypeHost, Name: "app1"}) {
		t.Fatal("the declared edge must still count as ground truth")
	}
	// Control: a FULL-confidence chaos edge (the default 0.90) still outranks and relabels, exactly as before.
	g.Upsert(Edge{From: declared.From, To: declared.To, Rel: RelDependsOn, Source: SourceChaos, Confidence: SourceConfidence[SourceChaos], DelaySeconds: 99, ValidUntil: now.Add(time.Hour)})
	for _, e := range g.Export().Edges {
		if e.FromName == "app1" && e.ToName == "pve3" {
			got = e
		}
	}
	if got.Source != string(SourceChaos) || got.DelaySeconds != 99 {
		t.Fatalf("control: a 0.90 chaos edge still outranks the declared one, got %+v", got)
	}
}
