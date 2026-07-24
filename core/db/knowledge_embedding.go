package db

import (
	"context"
	"fmt"
	"strconv"

	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/schema"
)

// KnowledgeEmbeddingStore is the pgx-backed vector index over knowledge_embedding (migration 0013) — the
// semantic channel of the retrieval plane (spec/012 REQ-1110/REQ-1111). It stores ONLY derived data (the
// per-precedent embedding keyed by external_ref + content hash); the corpus file remains the source of
// truth for precedent content, so every write here is best-effort and reversible.
//
// All SQL is parameterized ($1…): the vector value is BOUND as its pgvector text form ("[x,y,…]") and cast
// server-side ($N::vector) — never interpolated into the statement (INV-03).
type KnowledgeEmbeddingStore struct{ p *Pool }

// NewKnowledgeEmbeddingStore returns the Postgres-backed semantic index.
func NewKnowledgeEmbeddingStore(p *Pool) *KnowledgeEmbeddingStore { return &KnowledgeEmbeddingStore{p: p} }

// compile-time proof it satisfies every seam the knowledge plane drives.
var (
	_ knowledge.SemanticSearcher = (*KnowledgeEmbeddingStore)(nil)
	_ knowledge.IndexSync        = (*KnowledgeEmbeddingStore)(nil)
	_ knowledge.EmbedSink        = (*KnowledgeEmbeddingStore)(nil)
)

// Dim reads the migrated embedding column's dimension from the catalog (vector(N) stores N in atttypmod).
// The worker refuses to boot the semantic plane when TG_EMBED_DIM disagrees with it — the migration's
// dimension is the law, config must match it, and a truncated/padded vector is never written.
func (s *KnowledgeEmbeddingStore) Dim(ctx context.Context) (int, error) {
	var dim int
	err := s.p.QueryRow(ctx, `
		SELECT atttypmod FROM pg_attribute
		WHERE attrelid = 'knowledge_embedding'::regclass AND attname = 'embedding'`).Scan(&dim)
	if err != nil {
		return 0, fmt.Errorf("db: knowledge embedding dim: %w", err)
	}
	if dim <= 0 {
		return 0, fmt.Errorf("db: knowledge embedding column has no declared dimension")
	}
	return dim, nil
}

// UpsertRef records a precedent's (external_ref, content_hash) identity. A NEW ref inserts unembedded; an
// existing ref with a CHANGED hash nulls its embedding (the text changed — the old vector is a lie about
// the new text) so backfill re-embeds it; an unchanged (ref, hash) is a no-op — the idempotency that makes
// re-syncing an unchanged corpus free.
func (s *KnowledgeEmbeddingStore) UpsertRef(ctx context.Context, externalRef, contentHash string) error {
	ver, err := schema.Stamp(schema.TableKnowledgeEmbedding)
	if err != nil {
		return err
	}
	_, err = s.p.Exec(ctx, `
		INSERT INTO knowledge_embedding (external_ref, content_hash, schema_version)
		VALUES ($1, $2, $3)
		ON CONFLICT (external_ref) DO UPDATE
		SET content_hash = EXCLUDED.content_hash, embedding = NULL, embed_model = '',
		    schema_version = EXCLUDED.schema_version, updated_at = now()
		WHERE knowledge_embedding.content_hash IS DISTINCT FROM EXCLUDED.content_hash`,
		externalRef, contentHash, int(ver))
	if err != nil {
		return fmt.Errorf("db: upsert knowledge embedding ref %s: %w", externalRef, err)
	}
	return nil
}

// PruneExcept deletes index rows whose external_ref is not in keep — precedent that left the corpus can
// never be surfaced from a stale vector. An empty keep prunes everything (an empty corpus has no index).
// keep binds as ONE array parameter, never a string-built IN list.
func (s *KnowledgeEmbeddingStore) PruneExcept(ctx context.Context, keep []string) (int, error) {
	if keep == nil {
		keep = []string{}
	}
	tag, err := s.p.Exec(ctx, `DELETE FROM knowledge_embedding WHERE NOT (external_ref = ANY($1))`, keep)
	if err != nil {
		return 0, fmt.Errorf("db: prune knowledge embeddings: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Unembedded returns up to limit rows still awaiting a vector (embedding IS NULL), deterministically
// ordered so repeated passes walk the same frontier.
func (s *KnowledgeEmbeddingStore) Unembedded(ctx context.Context, limit int) ([]knowledge.PendingEmbed, error) {
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, content_hash FROM knowledge_embedding
		WHERE embedding IS NULL
		ORDER BY external_ref ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("db: unembedded refs: %w", err)
	}
	defer rows.Close()
	var out []knowledge.PendingEmbed
	for rows.Next() {
		var p knowledge.PendingEmbed
		if err := rows.Scan(&p.ExternalRef, &p.ContentHash); err != nil {
			return nil, fmt.Errorf("db: unembedded scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: unembedded iterate: %w", err)
	}
	return out, nil
}

// WriteEmbedding stores a computed vector — ONLY while the row still carries the content hash the embedded
// text was rendered from (a concurrent corpus change makes this a no-op; the next pass embeds the new
// text). Returns whether a row was written.
func (s *KnowledgeEmbeddingStore) WriteEmbedding(ctx context.Context, externalRef, contentHash string, vec []float32, embedModel string) (bool, error) {
	if len(vec) == 0 {
		return false, fmt.Errorf("db: write embedding %s: empty vector", externalRef)
	}
	tag, err := s.p.Exec(ctx, `
		UPDATE knowledge_embedding
		SET embedding = $3::vector, embed_model = $4, updated_at = now()
		WHERE external_ref = $1 AND content_hash = $2`,
		externalRef, contentHash, vectorLiteral(vec), embedModel)
	if err != nil {
		return false, fmt.Errorf("db: write embedding %s: %w", externalRef, err)
	}
	return tag.RowsAffected() > 0, nil
}

// SearchSimilar returns the cosine top-k embedded precedents for a query vector, most similar first. The
// ORDER BY is the bare distance operator so the HNSW index serves the scan; ties are re-broken
// deterministically in Go (similarity desc, then external_ref) after the fetch.
func (s *KnowledgeEmbeddingStore) SearchSimilar(ctx context.Context, vec []float32, k int) ([]knowledge.SemanticMatch, error) {
	if len(vec) == 0 || k <= 0 {
		return nil, nil
	}
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, 1 - (embedding <=> $1::vector) AS similarity
		FROM knowledge_embedding
		WHERE embedding IS NOT NULL
		ORDER BY embedding <=> $1::vector
		LIMIT $2`, vectorLiteral(vec), k)
	if err != nil {
		return nil, fmt.Errorf("db: semantic search: %w", err)
	}
	defer rows.Close()
	var out []knowledge.SemanticMatch
	for rows.Next() {
		var m knowledge.SemanticMatch
		if err := rows.Scan(&m.ExternalRef, &m.Similarity); err != nil {
			return nil, fmt.Errorf("db: semantic search scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: semantic search iterate: %w", err)
	}
	// Deterministic order even at exact-tie similarities (the SQL tie order is unspecified).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && less(out[j], out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func less(a, b knowledge.SemanticMatch) bool {
	if a.Similarity != b.Similarity {
		return a.Similarity > b.Similarity
	}
	return a.ExternalRef < b.ExternalRef
}

// vectorLiteral renders a vector as pgvector's text form "[x,y,…]". It is a bound VALUE (always passed as
// a $N parameter and cast with ::vector), never part of the SQL text.
func vectorLiteral(vec []float32) string {
	buf := make([]byte, 0, len(vec)*10+2)
	buf = append(buf, '[')
	for i, v := range vec {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = strconv.AppendFloat(buf, float64(v), 'f', -1, 32)
	}
	buf = append(buf, ']')
	return string(buf)
}
