package estatedoc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// persistedCorpus is the on-disk shape: the chunks plus the tallies, so a reader (slice 2's retriever) loads
// one self-describing file. The estate-doc corpus is on-box data and is NEVER committed (it is derived from the
// operator's private docs and, though scrubbed, is not for the public mirror) — the same rule the incident
// corpus follows.
type persistedCorpus struct {
	Version    int        `json:"version"`
	Files      int        `json:"files"`
	Redactions int        `json:"redactions"`
	Chunks     []DocChunk `json:"chunks"`
}

const corpusVersion = 1

// CorpusHash is a stable digest of the corpus CONTENT (the chunk refs + their content hashes, in the corpus's
// already-deterministic order). It is the idempotency key: a re-ingest of an unchanged doc tree yields the same
// hash, so WriteCorpus can skip rewriting an identical file (and a caller can tell "nothing changed" from "the
// grounding drifted").
func CorpusHash(c Corpus) string {
	h := sha256.New()
	for _, ch := range c.Chunks {
		fmt.Fprintf(h, "%s\x00%s\n", ch.ExternalRef, ch.ContentHash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// WriteCorpus persists c to path as JSON, atomically (write-temp-then-rename, so a crash never leaves a
// half-written corpus a reader would parse as truth) and hash-guarded (if the file already holds a corpus with
// the same CorpusHash, the write is skipped). It returns whether it actually wrote. The parent directory is
// created if absent. The file is written 0600 — it is derived from private operator docs.
func WriteCorpus(path string, c Corpus) (wrote bool, err error) {
	if existing, rerr := ReadCorpus(path); rerr == nil && CorpusHash(existing) == CorpusHash(c) {
		return false, nil // unchanged — idempotent no-op
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("estatedoc: mkdir corpus dir: %w", err)
	}
	body, err := json.MarshalIndent(persistedCorpus{
		Version: corpusVersion, Files: c.Files, Redactions: c.Redactions, Chunks: c.Chunks,
	}, "", "  ")
	if err != nil {
		return false, fmt.Errorf("estatedoc: marshal corpus: %w", err)
	}
	// A UNIQUE temp file in the target dir (os.CreateTemp is 0600) so two concurrent ingests to the same -out
	// cannot interleave onto a shared "<path>.tmp" and rename each other's half-written content into place.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return false, fmt.Errorf("estatedoc: create temp corpus: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed; cleans up on every error path
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return false, fmt.Errorf("estatedoc: write temp corpus: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("estatedoc: close temp corpus: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, fmt.Errorf("estatedoc: rename corpus into place: %w", err)
	}
	return true, nil
}

// ReadCorpus loads a corpus previously written by WriteCorpus. A missing file is an error the caller decides how
// to treat (an ingest tool treats it as "no prior corpus"); a present-but-unparseable file is always an error.
func ReadCorpus(path string) (Corpus, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Corpus{}, err
	}
	var p persistedCorpus
	if err := json.Unmarshal(b, &p); err != nil {
		return Corpus{}, fmt.Errorf("estatedoc: parse corpus %q: %w", path, err)
	}
	return Corpus{Chunks: p.Chunks, Files: p.Files, Redactions: p.Redactions}, nil
}
