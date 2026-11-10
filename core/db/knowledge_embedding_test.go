package db

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/knowledge"
)

// vectorLiteral renders pgvector's text form exactly: bracketed, comma-separated, float32 round-trip safe.
// It is always BOUND as a parameter ($N::vector), so its shape is a correctness (not injection) concern —
// but it must never contain anything but digits, signs, dots, commas, and brackets.
func TestVectorLiteral(t *testing.T) {
	for _, tc := range []struct {
		in   []float32
		want string
	}{
		{[]float32{}, "[]"},
		{[]float32{1}, "[1]"},
		{[]float32{0.5, -0.25, 3}, "[0.5,-0.25,3]"},
	} {
		if got := vectorLiteral(tc.in); got != tc.want {
			t.Errorf("vectorLiteral(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, r := range vectorLiteral([]float32{1.5e-8, -2.25, 42}) {
		switch {
		case r >= '0' && r <= '9', r == '.', r == ',', r == '-', r == '+', r == '[', r == ']', r == 'e':
		default:
			t.Fatalf("vector literal contains unexpected rune %q", r)
		}
	}
}

// The pgx vector index round-trips against a real Postgres (pgvector image): sync upserts refs, backfill
// writes hash-guarded vectors, cosine search returns nearest-first through the HNSW index, a changed hash
// nulls the vector, and prune removes vanished refs. Skipped in CI (no DB); runs under compose when
// TG_TEST_POSTGRES_DSN points at a migrated pgvector database.
func TestKnowledgeEmbeddingIntegration(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated pgvector database to run the semantic-index integration test")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()
	s := NewKnowledgeEmbeddingStore(p)

	// The migrated column's dimension is discoverable (the boot mismatch check reads this).
	dim, err := s.Dim(ctx)
	if err != nil || dim != knowledge.DefaultEmbedDim {
		t.Fatalf("dim: got %d err=%v, want %d", dim, err, knowledge.DefaultEmbedDim)
	}

	// Unique refs per run: a shared database never collides with a prior run. (The prune assertion at the
	// end leaves only refB behind — this suite is the table's only writer.)
	uniq := fmt.Sprintf("it-%d", os.Getpid())
	refA, refB, refC := uniq+"-A", uniq+"-B", uniq+"-C"

	for ref, hash := range map[string]string{refA: "hA", refB: "hB", refC: "hC"} {
		if err := s.UpsertRef(ctx, ref, hash); err != nil {
			t.Fatalf("upsert %s: %v", ref, err)
		}
	}
	pending, err := s.Unembedded(ctx, 10)
	if err != nil || len(pending) < 3 {
		t.Fatalf("unembedded: %d err=%v", len(pending), err)
	}

	// Basis-vector embeddings make cosine ranking exact: qv is closest to A, then B, then orthogonal C.
	vec := func(x, y, z float32) []float32 {
		v := make([]float32, dim)
		v[0], v[1], v[2] = x, y, z
		return v
	}
	for ref, v := range map[string][]float32{refA: vec(1, 0, 0), refB: vec(1, 1, 0), refC: vec(0, 0, 1)} {
		wrote, werr := s.WriteEmbedding(ctx, ref, "h"+ref[len(ref)-1:], v, "test-model")
		if werr != nil || !wrote {
			t.Fatalf("write %s: wrote=%v err=%v", ref, wrote, werr)
		}
	}
	// A hash-mismatched write must be a no-op (the guard the backfill loop leans on).
	if wrote, werr := s.WriteEmbedding(ctx, refA, "stale-hash", vec(0, 1, 0), "test-model"); werr != nil || wrote {
		t.Fatalf("stale-hash write must not land: wrote=%v err=%v", wrote, werr)
	}

	matches, err := s.SearchSimilar(ctx, vec(1, 0.1, 0), 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// Filter to this run's refs (a shared DB may hold other rows), preserving order.
	var got []knowledge.SemanticMatch
	for _, m := range matches {
		if m.ExternalRef == refA || m.ExternalRef == refB || m.ExternalRef == refC {
			got = append(got, m)
		}
	}
	if len(got) != 3 || got[0].ExternalRef != refA || got[1].ExternalRef != refB || got[2].ExternalRef != refC {
		t.Fatalf("cosine order wrong: %+v", got)
	}
	if got[0].Similarity < 0.98 || got[2].Similarity > 0.1 {
		t.Fatalf("similarity range wrong: %+v", got)
	}

	// A changed content hash nulls the embedding (re-embed contract).
	if err := s.UpsertRef(ctx, refA, "hA-v2"); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	pending, err = s.Unembedded(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pe := range pending {
		if pe.ExternalRef == refA && pe.ContentHash == "hA-v2" {
			found = true
		}
	}
	if !found {
		t.Fatal("a changed hash must null the vector and re-queue the row")
	}

	// Prune removes everything not kept.
	if _, err := s.PruneExcept(ctx, []string{refB}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	matches, err = s.SearchSimilar(ctx, vec(1, 0, 0), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range matches {
		if m.ExternalRef == refA || m.ExternalRef == refC {
			t.Fatalf("pruned ref %s still searchable", m.ExternalRef)
		}
	}
}
