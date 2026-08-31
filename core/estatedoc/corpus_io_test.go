package estatedoc

import (
	"os"
	"path/filepath"
	"testing"
)

func sampleCorpus() Corpus {
	return Corpus{
		Files:      1,
		Redactions: 2,
		Chunks: []DocChunk{
			{ExternalRef: "a.md#0", Path: "a.md", Heading: "H", Content: "body", ContentHash: "hash0", Redactions: 2},
		},
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corpus.json")
	if _, err := WriteCorpus(path, sampleCorpus()); err != nil {
		t.Fatalf("WriteCorpus: %v", err)
	}
	got, err := ReadCorpus(path)
	if err != nil {
		t.Fatalf("ReadCorpus: %v", err)
	}
	if got.Files != 1 || got.Redactions != 2 || len(got.Chunks) != 1 || got.Chunks[0].ExternalRef != "a.md#0" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	// written 0600 — derived from private operator docs.
	if fi, _ := os.Stat(path); fi != nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("corpus mode = %v, want 0600", fi.Mode().Perm())
	}
}

// The hash-guard: an unchanged re-ingest must NOT rewrite the file. KILLING MUTATION: drop the CorpusHash
// equality skip in WriteCorpus and the second write reports wrote=true.
func TestWriteCorpusIdempotentSkip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corpus.json")
	wrote1, err := WriteCorpus(path, sampleCorpus())
	if err != nil || !wrote1 {
		t.Fatalf("first write: wrote=%v err=%v, want wrote=true", wrote1, err)
	}
	wrote2, err := WriteCorpus(path, sampleCorpus())
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if wrote2 {
		t.Error("unchanged re-ingest rewrote the corpus — the hash-guard did not fire")
	}
}

func TestWriteCorpusRewritesOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corpus.json")
	if _, err := WriteCorpus(path, sampleCorpus()); err != nil {
		t.Fatal(err)
	}
	changed := sampleCorpus()
	changed.Chunks[0].ContentHash = "hashDIFFERENT" // the content changed → the corpus hash changes
	wrote, err := WriteCorpus(path, changed)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Error("a changed corpus was not rewritten — the grounding would silently go stale")
	}
}

func TestCorpusHash_KeysOnRefAndContent(t *testing.T) {
	base := sampleCorpus()
	if CorpusHash(base) != CorpusHash(sampleCorpus()) {
		t.Error("identical corpora hashed differently")
	}
	moved := sampleCorpus()
	moved.Chunks[0].ContentHash = "other"
	if CorpusHash(base) == CorpusHash(moved) {
		t.Error("a changed content hash did not change the corpus hash")
	}
	// Files/Redactions tallies are metadata, not the content identity — a re-count with the same chunks is
	// the same corpus.
	recount := sampleCorpus()
	recount.Files = 99
	if CorpusHash(base) != CorpusHash(recount) {
		t.Error("corpus hash changed on a metadata-only difference — it must key on chunk content only")
	}
}

func TestReadCorpus_MissingFileErrors(t *testing.T) {
	if _, err := ReadCorpus(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("ReadCorpus of a missing file must error (the caller decides how to treat it)")
	}
}
