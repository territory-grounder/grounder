package knowledge

import (
	"context"
	"errors"
	"sort"
	"testing"
)

// fakeIndexStore is the in-memory oracle twin of db.KnowledgeEmbeddingStore: hash-guarded writes, NULL
// embeddings on hash change, prune-by-keep-set.
type fakeIndexStore struct {
	rows     map[string]*fakeIndexRow
	upserts  int
	failNext error
}

type fakeIndexRow struct {
	hash  string
	vec   []float32
	model string
}

func newFakeIndexStore() *fakeIndexStore { return &fakeIndexStore{rows: map[string]*fakeIndexRow{}} }

func (f *fakeIndexStore) UpsertRef(_ context.Context, ref, hash string) error {
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return err
	}
	f.upserts++
	if r, ok := f.rows[ref]; ok {
		if r.hash != hash {
			r.hash, r.vec, r.model = hash, nil, "" // content changed → the vector is stale, re-embed
		}
		return nil
	}
	f.rows[ref] = &fakeIndexRow{hash: hash}
	return nil
}

func (f *fakeIndexStore) PruneExcept(_ context.Context, keep []string) (int, error) {
	keepSet := map[string]struct{}{}
	for _, k := range keep {
		keepSet[k] = struct{}{}
	}
	pruned := 0
	for ref := range f.rows {
		if _, ok := keepSet[ref]; !ok {
			delete(f.rows, ref)
			pruned++
		}
	}
	return pruned, nil
}

func (f *fakeIndexStore) Unembedded(_ context.Context, limit int) ([]PendingEmbed, error) {
	var out []PendingEmbed
	for ref, r := range f.rows {
		if r.vec == nil {
			out = append(out, PendingEmbed{ExternalRef: ref, ContentHash: r.hash})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExternalRef < out[j].ExternalRef })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeIndexStore) WriteEmbedding(_ context.Context, ref, hash string, vec []float32, model string) (bool, error) {
	r, ok := f.rows[ref]
	if !ok || r.hash != hash {
		return false, nil // the hash moved (or the row was pruned) — never bind a vector to other text
	}
	r.vec, r.model = vec, model
	return true, nil
}

// SyncIndex upserts every live ref, prunes vanished ones, and is idempotent over an unchanged corpus; a
// changed precedent's row loses its vector (it re-embeds).
func TestSyncIndex(t *testing.T) {
	ctx := context.Background()
	store := newFakeIndexStore()
	corpus := semCorpus()
	up, pruned, err := SyncIndex(ctx, store, corpus)
	if err != nil || up != 3 || pruned != 0 {
		t.Fatalf("initial sync: up=%d pruned=%d err=%v", up, pruned, err)
	}
	// Re-sync of the identical corpus changes nothing.
	if _, _, err := SyncIndex(ctx, store, corpus); err != nil {
		t.Fatalf("idempotent resync: %v", err)
	}
	// Embed one row, then change its content: the vector must null out.
	if ok, _ := store.WriteEmbedding(ctx, "TG-200", ContentHash(corpus[0]), []float32{1, 2}, "m"); !ok {
		t.Fatal("precondition: embed TG-200")
	}
	changed := append([]Incident{}, corpus...)
	changed[0].Resolution = "reload nginx config"
	if _, _, err := SyncIndex(ctx, store, changed); err != nil {
		t.Fatalf("changed sync: %v", err)
	}
	if store.rows["TG-200"].vec != nil {
		t.Fatal("a changed precedent must drop its stale vector (re-embed)")
	}
	// A shrunk corpus prunes the vanished ref.
	_, pruned, err = SyncIndex(ctx, store, changed[:2])
	if err != nil || pruned != 1 {
		t.Fatalf("shrink sync: pruned=%d err=%v", pruned, err)
	}
	if _, ok := store.rows["TG-202"]; ok {
		t.Fatal("a vanished precedent must be pruned from the index")
	}
	// An empty corpus empties the index; a nil index errs loudly.
	if _, pruned, err = SyncIndex(ctx, store, nil); err != nil || pruned != 2 {
		t.Fatalf("empty sync: pruned=%d err=%v", pruned, err)
	}
	if _, _, err := SyncIndex(ctx, nil, corpus); err == nil {
		t.Fatal("a nil index must be refused")
	}
}

// The backfill pass embeds exactly the unembedded rows (bounded by Batch), is idempotent (a second pass
// embeds nothing), skips rows whose content moved, and refuses a wrong-dimension vector.
func TestBackfillerRunOnce(t *testing.T) {
	ctx := context.Background()
	store := newFakeIndexStore()
	corpus := semCorpus()
	if _, _, err := SyncIndex(ctx, store, corpus); err != nil {
		t.Fatal(err)
	}
	holder := NewHolder(NewLexicalRetriever(corpus))
	embed := &fakeEmbedder{vec: []float32{0.1, 0.2}}
	b := &Backfiller{Store: store, Lookup: holder, Embed: embed, Model: "nomic-embed-text", Dim: 2, Batch: 2}

	// Pass 1: batch bound 2 of 3.
	res, err := b.RunOnce(ctx)
	if err != nil || res.Embedded != 2 {
		t.Fatalf("pass 1: %+v err=%v", res, err)
	}
	// Pass 2: the remaining 1.
	res, err = b.RunOnce(ctx)
	if err != nil || res.Embedded != 1 {
		t.Fatalf("pass 2: %+v err=%v", res, err)
	}
	// Pass 3: idempotent — nothing left, and the embedder is not called for an empty batch.
	calls := embed.calls
	res, err = b.RunOnce(ctx)
	if err != nil || res.Embedded != 0 || embed.calls != calls {
		t.Fatalf("pass 3 must be a no-op: %+v err=%v calls=%d→%d", res, err, calls, embed.calls)
	}
	for ref, r := range store.rows {
		if r.vec == nil || r.model != "nomic-embed-text" {
			t.Fatalf("row %s not embedded/attributed: %+v", ref, r)
		}
	}
}

// Backfill failure modes: an embed outage errors (rows stay honestly NULL for the next pass), a stale row
// (corpus moved past the index) is skipped, and a wrong-dimension vector is refused, never stored.
func TestBackfillerFailureModes(t *testing.T) {
	ctx := context.Background()
	corpus := semCorpus()
	holder := NewHolder(NewLexicalRetriever(corpus))

	// Embed outage: error surfaces, nothing written.
	store := newFakeIndexStore()
	if _, _, err := SyncIndex(ctx, store, corpus); err != nil {
		t.Fatal(err)
	}
	b := &Backfiller{Store: store, Lookup: holder, Embed: &fakeEmbedder{err: errors.New("embed down")}, Dim: 2}
	if _, err := b.RunOnce(ctx); err == nil {
		t.Fatal("an embed outage must surface as an error")
	}
	if got, _ := store.Unembedded(ctx, 10); len(got) != 3 {
		t.Fatalf("rows must stay unembedded after an outage, got %d embedded-side", 3-len(got))
	}

	// Stale rows: the index knows a hash the live corpus no longer has → skipped, not embedded.
	stale := newFakeIndexStore()
	if err := stale.UpsertRef(ctx, "TG-200", "hash-of-old-text"); err != nil {
		t.Fatal(err)
	}
	if err := stale.UpsertRef(ctx, "TG-GONE", "hash-x"); err != nil { // not in the corpus at all
		t.Fatal(err)
	}
	b = &Backfiller{Store: stale, Lookup: holder, Embed: &fakeEmbedder{vec: []float32{1, 2}}, Dim: 2}
	res, err := b.RunOnce(ctx)
	if err != nil || res.Embedded != 0 || res.Skipped != 2 {
		t.Fatalf("stale rows must be skipped: %+v err=%v", res, err)
	}

	// Dimension mismatch: refused loudly, nothing stored.
	store2 := newFakeIndexStore()
	if _, _, err := SyncIndex(ctx, store2, corpus); err != nil {
		t.Fatal(err)
	}
	b = &Backfiller{Store: store2, Lookup: holder, Embed: &fakeEmbedder{vec: []float32{1, 2, 3}}, Dim: 2}
	if _, err := b.RunOnce(ctx); err == nil {
		t.Fatal("a wrong-dimension vector must be refused")
	}
	if got, _ := store2.Unembedded(ctx, 10); len(got) != 3 {
		t.Fatal("no truncated/padded vector may ever be stored")
	}

	// An unwired backfiller refuses to run.
	if _, err := (&Backfiller{}).RunOnce(ctx); err == nil {
		t.Fatal("an unwired backfiller must be refused")
	}
}
