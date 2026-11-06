package main

import (
	"os"
	"path/filepath"
	"testing"
)

// sameCorpusFile is the gate that decides whether the AWX playbooks lane writes the maintained precedent
// corpus (TG_KNOWLEDGE_FILE) and so must route through the TG-510 witness. It must see through distinct
// spellings of one file and never conflate two different files.
func TestSameCorpusFile(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "corpus.json")
	if err := os.WriteFile(a, []byte("[]\n"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	b := filepath.Join(dir, "other.json")
	if err := os.WriteFile(b, []byte("[]\n"), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}

	// Same file, two spellings (a redundant ./ segment) — must be SAME (os.SameFile sees one inode).
	spelled := filepath.Join(dir, ".", "corpus.json")
	if !sameCorpusFile(a, spelled) {
		t.Errorf("sameCorpusFile must treat %q and %q as the same file", a, spelled)
	}

	// Two distinct files — must be DIFFERENT.
	if sameCorpusFile(a, b) {
		t.Errorf("sameCorpusFile must not conflate distinct files %q and %q", a, b)
	}

	// Empty operands (env var unset) — must be DIFFERENT (never route on a blank path).
	if sameCorpusFile("", a) || sameCorpusFile(a, "") || sameCorpusFile("", "") {
		t.Errorf("sameCorpusFile must be false when either path is empty")
	}

	// Two not-yet-existing paths that clean-resolve to the same location — SAME (falls back to path compare).
	nx := filepath.Join(dir, "notyet.json")
	nxSpelled := filepath.Join(dir, "sub", "..", "notyet.json")
	if !sameCorpusFile(nx, nxSpelled) {
		t.Errorf("sameCorpusFile must treat clean-equal non-existent paths as the same: %q vs %q", nx, nxSpelled)
	}

	// Two distinct not-yet-existing paths — DIFFERENT.
	if sameCorpusFile(nx, filepath.Join(dir, "elsewhere.json")) {
		t.Errorf("sameCorpusFile must not conflate distinct non-existent paths")
	}
}
