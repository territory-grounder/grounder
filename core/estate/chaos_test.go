package estate

import (
	"context"
	"errors"
	"testing"
	"time"
)

// chaosLoaderFrom wraps a fixed cascade set (the DB reader's output shape) so the source's mapping is exercised
// without a database.
func chaosLoaderFrom(cascades ...ChaosCascade) func(context.Context) ([]ChaosCascade, error) {
	return func(context.Context) ([]ChaosCascade, error) { return cascades, nil }
}

func TestChaosSourceMapsCascadeToDependsOnEdge(t *testing.T) {
	injectedAt := time.Date(2026, 8, 12, 3, 0, 0, 0, time.UTC)
	src := NewChaosSource(chaosLoaderFrom(ChaosCascade{
		Root: "dc1pve03", Downstream: "dc1app1", Injections: 1, LatestInjectedAt: injectedAt,
	}))
	if got := src.Source(); got != SourceChaos {
		t.Fatalf("Source() = %q, want %q", got, SourceChaos)
	}
	edges, err := src.Edges(context.Background())
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1", len(edges))
	}
	e := edges[0]
	// DIRECTION: the downstream host depends_on the injected root — we broke the root and the downstream
	// followed, so the causal arrow points downstream→root (not the reverse).
	if e.From.Name != "dc1app1" || e.To.Name != "dc1pve03" {
		t.Errorf("direction = %s depends_on %s, want dc1app1 depends_on dc1pve03", e.From.Name, e.To.Name)
	}
	if e.From.Type != TypeHost || e.To.Type != TypeHost {
		t.Errorf("endpoints typed %s/%s, want host/host", e.From.Type, e.To.Type)
	}
	if e.Rel != RelDependsOn {
		t.Errorf("rel = %q, want depends_on", e.Rel)
	}
	if e.Source != SourceChaos {
		t.Errorf("source = %q, want chaos", e.Source)
	}
	// Confidence left 0 so Build stamps the 0.90 policy default.
	if e.Confidence != 0 {
		t.Errorf("confidence = %v, want 0 (Build stamps the SourceChaos default)", e.Confidence)
	}
	// SELF-EXPIRY: ValidUntil = latest injection + TTL. The decay pass is SourceIncident-scoped and never reaps
	// chaos, so without this a chaos edge would be immortal.
	if want := injectedAt.Add(ChaosEdgeTTL); !e.ValidUntil.Equal(want) {
		t.Errorf("ValidUntil = %s, want %s (latest injection + ChaosEdgeTTL)", e.ValidUntil, want)
	}
}

func TestChaosSourceStampsBuildDefaultConfidence(t *testing.T) {
	// Through Build the chaos edge lands at the 0.90 SourceChaos policy confidence — above the learned cap
	// (0.75) because the root is observed, not guessed.
	injectedAt := time.Date(2026, 8, 12, 3, 0, 0, 0, time.UTC)
	src := NewChaosSource(chaosLoaderFrom(ChaosCascade{Root: "root1", Downstream: "down1", Injections: 1, LatestInjectedAt: injectedAt}))
	g, errs := Build(context.Background(), []EdgeSource{src})
	if len(errs) != 0 {
		t.Fatalf("build errors: %v", errs)
	}
	e, ok := g.edges[edgeKey(Entity{Type: TypeHost, Name: "down1"}, Entity{Type: TypeHost, Name: "root1"}, RelDependsOn)]
	if !ok {
		t.Fatalf("chaos edge down1 depends_on root1 not present after Build")
	}
	if want := SourceConfidence[SourceChaos]; e.Confidence != want {
		t.Errorf("confidence = %v, want %v (the SourceChaos default)", e.Confidence, want)
	}
	if e.Confidence != 0.90 {
		t.Errorf("confidence = %v, want 0.90", e.Confidence)
	}
}

func TestChaosSourceEmptyLedgerEmitsNoEdges(t *testing.T) {
	// The EMPTY case: no injections → no edges. Killing mutation for a producer that must not fabricate edges
	// from an empty ledger.
	src := NewChaosSource(chaosLoaderFrom())
	edges, err := src.Edges(context.Background())
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("got %d edges from an empty ledger, want 0", len(edges))
	}
}

func TestChaosSourceSkipsSubThresholdAndDegenerate(t *testing.T) {
	injectedAt := time.Date(2026, 8, 12, 3, 0, 0, 0, time.UTC)
	src := NewChaosSource(chaosLoaderFrom(
		ChaosCascade{Root: "r", Downstream: "d", Injections: 1, LatestInjectedAt: injectedAt},       // below min=2
		ChaosCascade{Root: "self", Downstream: "self", Injections: 5, LatestInjectedAt: injectedAt}, // self-loop
		ChaosCascade{Root: "", Downstream: "d2", Injections: 5, LatestInjectedAt: injectedAt},       // empty root
		ChaosCascade{Root: "r2", Downstream: "  ", Injections: 5, LatestInjectedAt: injectedAt},     // blank downstream
		ChaosCascade{Root: "keep", Downstream: "kept", Injections: 5, LatestInjectedAt: injectedAt}, // the only survivor
	), WithChaosMinInjections(2))
	edges, err := src.Edges(context.Background())
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1 (only keep→kept clears every guard)", len(edges))
	}
	if edges[0].From.Name != "kept" || edges[0].To.Name != "keep" {
		t.Errorf("survivor = %s depends_on %s, want kept depends_on keep", edges[0].From.Name, edges[0].To.Name)
	}
}

func TestChaosSourceZeroTimestampNeverImmortal(t *testing.T) {
	// A cascade with no injection timestamp is SKIPPED, never emitted with a zero ValidUntil — a chaos edge must
	// never be immortal (the decay pass is SourceIncident-scoped and would never reap it).
	src := NewChaosSource(chaosLoaderFrom(ChaosCascade{Root: "r", Downstream: "d", Injections: 9, LatestInjectedAt: time.Time{}}))
	edges, err := src.Edges(context.Background())
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("a zero-timestamp cascade emitted %d edges, want 0 (never an immortal chaos edge)", len(edges))
	}
}

func TestChaosSourcePropagatesLoadError(t *testing.T) {
	// A load failure is RETURNED so Build isolates it per-source (the prior graph survives), never swallowed —
	// this is exactly the late-bound "pool not connected yet" case at boot.
	sentinel := errors.New("pool not connected")
	src := NewChaosSource(func(context.Context) ([]ChaosCascade, error) { return nil, sentinel })
	if _, err := src.Edges(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("Edges error = %v, want the load sentinel", err)
	}
}

func TestChaosSourceTTLOverride(t *testing.T) {
	injectedAt := time.Date(2026, 8, 12, 3, 0, 0, 0, time.UTC)
	src := NewChaosSource(
		chaosLoaderFrom(ChaosCascade{Root: "r", Downstream: "d", Injections: 1, LatestInjectedAt: injectedAt}),
		WithChaosEdgeTTL(48*time.Hour),
	)
	edges, err := src.Edges(context.Background())
	if err != nil || len(edges) != 1 {
		t.Fatalf("Edges: %v, %d edges (want 1)", err, len(edges))
	}
	if want := injectedAt.Add(48 * time.Hour); !edges[0].ValidUntil.Equal(want) {
		t.Errorf("ValidUntil = %s, want %s (overridden TTL)", edges[0].ValidUntil, want)
	}
}

func TestChaosSourceNilLoaderIsEmpty(t *testing.T) {
	// A source constructed with a nil loader degrades to empty rather than panicking.
	src := NewChaosSource(nil)
	edges, err := src.Edges(context.Background())
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("got %d edges from a nil loader, want 0", len(edges))
	}
}

func TestChaosSourceCarriesGroundTruthDelay(t *testing.T) {
	// TG-188 slice 2b: the cascade's measured mean propagation delay rides onto the chaos edge's DelaySeconds
	// (the slice-2 field). Unlike the co-occurrence learner's inferred delay, the root time is ground truth.
	injectedAt := time.Date(2026, 8, 12, 3, 0, 0, 0, time.UTC)
	src := NewChaosSource(chaosLoaderFrom(ChaosCascade{
		Root: "root", Downstream: "down", Injections: 2, LatestInjectedAt: injectedAt, MeanDelaySeconds: 210,
	}))
	edges, err := src.Edges(context.Background())
	if err != nil || len(edges) != 1 {
		t.Fatalf("Edges: %v, %d edges (want 1)", err, len(edges))
	}
	if edges[0].DelaySeconds != 210 {
		t.Errorf("chaos edge DelaySeconds = %v, want 210 (the ground-truth cascade delay carried onto the edge)", edges[0].DelaySeconds)
	}
}
