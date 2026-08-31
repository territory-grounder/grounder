package knowledge

// Drift-guard (flywheel integration-audit S1): the SHIPPED corpus.seed.json must carry NO wildcard host="*"
// row. A "*" row is matched fleet-wide by Count (LexicalRetriever.Count), so it silently de-novels its rule on
// EVERY host — defeating the first-sight-human novelty poll (the one control meant to force a human onto a
// never-seen (host,rule)). Fleet-wide de-novel must be a DELIBERATE operator choice, never a shipped default.
// This test fails at branch time if a "*" row is ever (re)added to the shipped seed.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSeedHasNoWildcardHost(t *testing.T) {
	// Locate the shipped seed relative to this source file (robust to the test working directory).
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve the test source path")
	}
	seedPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "knowledge", "corpus.seed.json")
	f, err := os.Open(seedPath)
	if err != nil {
		t.Skipf("shipped seed not found at %s (%v) — guard inapplicable in this layout", seedPath, err)
	}
	defer f.Close()

	corpus, err := ParseCorpus(f)
	if err != nil {
		t.Fatalf("shipped corpus.seed.json must parse: %v", err)
	}
	if len(corpus) == 0 {
		t.Fatal("shipped corpus.seed.json parsed empty — unexpected")
	}
	for _, inc := range corpus {
		if strings.TrimSpace(inc.Host) == "*" { // mirror LexicalRetriever.Count exactly (a padded " * " also de-novels)
			t.Errorf("shipped corpus.seed.json carries a wildcard host=\"*\" row (%s, rule %q) — it de-novels that "+
				"rule fleet-wide, defeating the first-sight-human novelty poll. Fleet-wide de-novel must be a "+
				"deliberate operator opt-in, never shipped. Remove it or make it a concrete host.", inc.ExternalRef, inc.AlertRule)
		}
	}
}
