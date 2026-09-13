// index.go is the WRITE path of the semantic retrieval plane (spec/012 REQ-1110/REQ-1111): keeping the
// vector index (the knowledge_embedding sidecar table) in step with the corpus, and backfilling embeddings
// in bounded, idempotent batches. The corpus file stays the single source of truth for precedent CONTENT —
// the index stores only (external_ref, content_hash, embedding), so index writes are always best-effort: a
// sync or embed failure leaves rows unembedded (lexical still serves them) and NEVER blocks a corpus write.
package knowledge

import (
	"context"
	"fmt"
)

// IndexSync is the ref-sync seam over the vector index: upsert the (ref, content_hash) identity of every
// live precedent (a changed hash nulls the row's embedding so it re-embeds) and prune rows whose precedent
// left the corpus (a pruned ref can never be surfaced from a stale vector).
type IndexSync interface {
	UpsertRef(ctx context.Context, externalRef, contentHash string) error
	PruneExcept(ctx context.Context, keep []string) (int, error)
}

// PendingEmbed is one index row awaiting an embedding (embedding IS NULL).
type PendingEmbed struct {
	ExternalRef string
	ContentHash string
}

// EmbedSink is the embedding write seam over the vector index. WriteEmbedding is guarded by the content
// hash (it writes only while the row still carries the hash the text was rendered from), so a concurrent
// corpus change can never bind a vector to text it was not computed from — the idempotency contract the
// backfill loop leans on.
type EmbedSink interface {
	Unembedded(ctx context.Context, limit int) ([]PendingEmbed, error)
	WriteEmbedding(ctx context.Context, externalRef, contentHash string, vec []float32, embedModel string) (bool, error)
}

// RefLookup resolves a precedent by external_ref from the live corpus (Holder implements it).
type RefLookup interface {
	ByRef(externalRef string) (Incident, bool)
}

// SyncIndex folds the current corpus into the vector index: every precedent's (ref, content_hash) is
// upserted (new/changed rows end up unembedded, awaiting backfill) and rows for vanished refs are pruned.
// Idempotent: re-syncing an unchanged corpus is a no-op. Returns (upserted, pruned).
func SyncIndex(ctx context.Context, idx IndexSync, corpus []Incident) (int, int, error) {
	if idx == nil {
		return 0, 0, fmt.Errorf("knowledge: sync: nil index")
	}
	keep := make([]string, 0, len(corpus))
	upserted := 0
	for _, inc := range corpus {
		if inc.ExternalRef == "" {
			continue // ParseCorpus rejects these; defensive
		}
		if err := idx.UpsertRef(ctx, inc.ExternalRef, ContentHash(inc)); err != nil {
			return upserted, 0, fmt.Errorf("knowledge: sync upsert %s: %w", inc.ExternalRef, err)
		}
		keep = append(keep, inc.ExternalRef)
		upserted++
	}
	pruned, err := idx.PruneExcept(ctx, keep)
	if err != nil {
		return upserted, 0, fmt.Errorf("knowledge: sync prune: %w", err)
	}
	return upserted, pruned, nil
}

// BackfillResult is one bounded embed pass's outcome.
type BackfillResult struct {
	Embedded int // vectors computed and written
	Skipped  int // rows skipped: ref no longer in the corpus, hash moved, or a dimension mismatch
}

// Backfiller embeds unembedded index rows in bounded batches — the worker runs it after every corpus sync
// (the best-effort write-path embed) and on TG_EMBED_BACKFILL_INTERVAL (the sweep). One Embed call per
// pass (a batch of texts), hash-guarded writes, and a full-stop error return on an embedding failure so the
// caller logs and retries next tick with the rows still honestly NULL.
type Backfiller struct {
	Store  EmbedSink
	Lookup RefLookup
	Embed  Embedder
	Model  string // recorded on each embedded row (provenance: which model produced the vector)
	Dim    int    // expected vector dimension; a mismatched model output is refused, never stored
	Batch  int    // max rows per pass; <=0 ⇒ DefaultBackfillBatch
}

// DefaultBackfillBatch bounds one embed pass (TG_EMBED_BACKFILL_BATCH).
const DefaultBackfillBatch = 64

// RunOnce embeds up to Batch unembedded rows. Idempotent: an already-embedded row is never selected, a row
// whose corpus text changed since selection is skipped (the next sync/pass picks up the new hash), and a
// re-run after a partial failure re-embeds only what is still NULL.
func (b *Backfiller) RunOnce(ctx context.Context) (BackfillResult, error) {
	var res BackfillResult
	if b.Store == nil || b.Lookup == nil || b.Embed == nil {
		return res, fmt.Errorf("knowledge: backfill: unwired (store/lookup/embedder)")
	}
	batch := b.Batch
	if batch <= 0 {
		batch = DefaultBackfillBatch
	}
	pending, err := b.Store.Unembedded(ctx, batch)
	if err != nil {
		return res, fmt.Errorf("knowledge: backfill: read unembedded: %w", err)
	}
	if len(pending) == 0 {
		return res, nil
	}
	// Resolve each pending ref against the live corpus and keep only rows whose stored hash still matches
	// the current text — anything else is stale and the next sync corrects it.
	texts := make([]string, 0, len(pending))
	rows := make([]PendingEmbed, 0, len(pending))
	for _, p := range pending {
		inc, ok := b.Lookup.ByRef(p.ExternalRef)
		if !ok || ContentHash(inc) != p.ContentHash {
			res.Skipped++
			continue
		}
		texts = append(texts, EmbedText(inc))
		rows = append(rows, p)
	}
	if len(rows) == 0 {
		return res, nil
	}
	vecs, err := b.Embed.Embed(ctx, texts)
	if err != nil {
		return res, fmt.Errorf("knowledge: backfill: embed batch of %d: %w", len(texts), err)
	}
	if len(vecs) != len(rows) {
		return res, fmt.Errorf("knowledge: backfill: embedder returned %d vectors for %d texts", len(vecs), len(rows))
	}
	for i, p := range rows {
		if b.Dim > 0 && len(vecs[i]) != b.Dim {
			// A model whose output dimension does not match the migrated column is a config error — refuse
			// the vector (never truncate/pad) and surface it; the row stays honestly unembedded.
			return res, fmt.Errorf("knowledge: backfill: %s: embedder returned dim %d, want %d (TG_EMBED_MODEL vs TG_EMBED_DIM mismatch)",
				p.ExternalRef, len(vecs[i]), b.Dim)
		}
		wrote, werr := b.Store.WriteEmbedding(ctx, p.ExternalRef, p.ContentHash, vecs[i], b.Model)
		if werr != nil {
			return res, fmt.Errorf("knowledge: backfill: write %s: %w", p.ExternalRef, werr)
		}
		if wrote {
			res.Embedded++
		} else {
			res.Skipped++ // the hash moved between select and write — the next pass re-embeds the new text
		}
	}
	return res, nil
}
