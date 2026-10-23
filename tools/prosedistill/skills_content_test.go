package main

// The two REAL-TREE gates of the distillation batch, wired into `go test ./...` so make all and the CI
// harness hold them without any Makefile or pipeline change:
//
//   - TestNoEstateTokensInProse — the denylist lint. skills/ is prose destined for the store plane and
//     the public mirror; this test scans EVERY committed file under skills/ against the mirror denylist
//     (github-sync/denylist.txt) plus the built-in minimal estate-token set, and fails naming file and
//     line on any hit. It is the same floor composeArtifacts enforces at build time, held independently
//     over the committed bytes — so a hand-edit that bypasses the tool still cannot land a token.
//
//   - TestDistilledTreeMatchesManifest — the idempotency proof. The committed tree must be byte-exactly
//     what the transform produces from the committed manifest and bodies: no drift, no strays, nothing
//     missing. This is `go run ./tools/prosedistill --verify` as a test.

import (
	"os"
	"path/filepath"
	"testing"
)

func realRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := findRepoRoot()
	if err != nil {
		t.Fatalf("cannot locate the repo root: %v", err)
	}
	return root
}

func TestNoEstateTokensInProse(t *testing.T) {
	root := realRepoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "skills")); err != nil {
		t.Fatalf("skills/ tree is absent (%v) — the manifest produces it; run `go run ./tools/prosedistill`", err)
	}
	pats, err := loadFloorPatterns(root)
	if err != nil {
		t.Fatalf("floor patterns: %v", err)
	}
	files, err := listTree(root)
	if err != nil {
		t.Fatalf("walking skills/: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("skills/ exists but holds zero files — an empty tree cannot satisfy the manifest")
	}
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		for _, hit := range scanBytes(rel, data, pats) {
			t.Errorf("estate token in committed prose: %s", hit)
		}
	}
	t.Logf("scanned %d files under skills/ against %d floor patterns", len(files), len(pats))
}

func TestDistilledTreeMatchesManifest(t *testing.T) {
	root := realRepoRoot(t)
	m, err := loadManifest(root)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	artifacts, err := composeArtifacts(root, m)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	problems, err := verifyTree(root, artifacts)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, p := range problems {
		t.Errorf("committed tree does not match the transform: %s", p)
	}
}
