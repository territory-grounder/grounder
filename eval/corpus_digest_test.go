package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TG-671: the corpus identity a scorecard carries is the SHA-256 of the file's bytes — a whitespace edit is a
// different corpus, and a missing file is an error, never an empty digest that would read as "unrecorded".
func TestCorpusDigestIsTheFileSHA256(t *testing.T) {
	p := filepath.Join(t.TempDir(), "corpus.json")
	if err := os.WriteFile(p, []byte(`[{"external_ref":"x"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(`[{"external_ref":"x"}]`))
	got, err := CorpusDigest(p)
	if err != nil || got != hex.EncodeToString(want[:]) || len(got) != 64 {
		t.Fatalf("digest = %q (%v), want %s", got, err, hex.EncodeToString(want[:]))
	}
	if err := os.WriteFile(p, []byte(`[{"external_ref":"x"} ]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if again, _ := CorpusDigest(p); again == got {
		t.Fatal("a byte-different file produced the same digest")
	}
	if _, err := CorpusDigest(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("a missing corpus must be an error, not an empty identity")
	}
	// The committed corpus has a stable, non-empty identity.
	if d, err := CorpusDigest("corpus.json"); err != nil || len(d) != 64 {
		t.Fatalf("committed corpus digest: %q %v", d, err)
	}
}
