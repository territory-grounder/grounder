package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/knowledge"
)

// The knowledge corpus is the seed ∪ maintained UNION (deploy-persistence fix): the worker writes ONLY
// the maintained file; the tracked seed is read-only bootstrap. These tests lock the load semantics that
// keep the novelty gate armed on a fresh box while letting runtime learning survive the deploy's
// tracked-file overwrite.

func writeCorpusFile(t *testing.T, rows ...knowledge.Incident) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "corpus.json")
	var b strings.Builder
	if err := knowledge.WriteCorpus(&b, rows); err != nil {
		t.Fatalf("serialize fixture corpus: %v", err)
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write fixture corpus: %v", err)
	}
	return p
}

func TestLoadKnowledgeCorpusUnion(t *testing.T) {
	seedRow := knowledge.Incident{ExternalRef: "pred-ik-1", Host: "web01", AlertRule: "NginxDown", Resolution: "seeded fix"}
	maintRow := knowledge.Incident{ExternalRef: "librenms-nl-9", Host: "db01", AlertRule: "DeviceDown", Resolution: "runtime fix"}
	dupSeed := knowledge.Incident{ExternalRef: "dup", Host: "a", Resolution: "seed version"}
	dupMaint := knowledge.Incident{ExternalRef: "dup", Host: "a", Resolution: "maintained version"}

	logs := &strings.Builder{}
	logf := func(f string, a ...any) { logs.WriteString(fmt.Sprintf(f+"\n", a...)) }

	t.Run("union of seed + maintained", func(t *testing.T) {
		seed := writeCorpusFile(t, seedRow, dupSeed)
		maint := writeCorpusFile(t, maintRow, dupMaint)
		r := loadKnowledgeCorpus(seed, maint, logf)
		if r == nil {
			t.Fatal("a present corpus must load")
		}
		// The lexical retriever ranks by query relevance, so assert each row is retrievable on its own terms.
		for ref, q := range map[string]knowledge.Query{
			"pred-ik-1":    {Host: "web01", AlertRule: "NginxDown"},
			"librenms-nl-9": {Host: "db01", AlertRule: "DeviceDown"},
		} {
			found := false
			for _, h := range r.Retrieve(q, 5) {
				if h.Incident.ExternalRef == ref {
					found = true
				}
			}
			if !found {
				t.Fatalf("the retriever must see the %s row (seed∪maintained union incomplete)", ref)
			}
		}
	})

	t.Run("fresh box: maintained absent ⇒ armed from the seed", func(t *testing.T) {
		seed := writeCorpusFile(t, seedRow)
		missing := filepath.Join(t.TempDir(), "not-yet-written.json")
		r := loadKnowledgeCorpus(seed, missing, logf)
		if r == nil {
			t.Fatal("a missing maintained corpus must NOT disarm the gate — the seed alone arms it")
		}
		hits := r.Retrieve(knowledge.Query{Host: "web01", AlertRule: "NginxDown"}, 5)
		if len(hits) == 0 {
			t.Fatal("the seed row must be retrievable before the first writeback")
		}
	})

	t.Run("no seed configured + maintained absent ⇒ nil (the wholly-absent corpus, gate-disabled warning fires upstream)", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "not-yet-written.json")
		if r := loadKnowledgeCorpus("", missing, logf); r != nil {
			t.Fatal("a wholly-absent corpus must keep the no-retriever semantics")
		}
	})

	t.Run("malformed maintained ⇒ nil (keep-prior — a torn write must not downgrade the retriever)", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		seed := writeCorpusFile(t, seedRow)
		if r := loadKnowledgeCorpus(seed, bad, logf); r != nil {
			t.Fatal("a malformed maintained corpus must keep the prior corpus (nil), not degrade to seed-only")
		}
	})

	t.Run("maintained wins over the seed on a shared external_ref", func(t *testing.T) {
		seed := writeCorpusFile(t, dupSeed)
		maint := writeCorpusFile(t, dupMaint)
		r := loadKnowledgeCorpus(seed, maint, logf)
		if r == nil {
			t.Fatal("corpus must load")
		}
		hits := r.Retrieve(knowledge.Query{Host: "a"}, 10)
		versions := 0
		for _, h := range hits {
			if h.Incident.ExternalRef == "dup" {
				versions++
				if h.Incident.Resolution != "maintained version" {
					t.Fatalf("the maintained row must win over the seed for a shared ref, got %q", h.Incident.Resolution)
				}
			}
		}
		if versions != 1 {
			t.Fatalf("a shared external_ref must appear EXACTLY once (no double-count), got %d", versions)
		}
	})

	// The review-caught regression: a write path that re-sets the live retriever from the maintained-only
	// set silently EVICTS the seed from the novelty gate until a restart. The write paths reload via
	// loadCorpus (the union) — this proves a reload after a runtime write keeps BOTH the seed-only shape
	// and the freshly written row visible.
	t.Run("write-after-load: a runtime write must not evict the seed", func(t *testing.T) {
		seed := writeCorpusFile(t, seedRow)
		maint := writeCorpusFile(t) // exists, zero rows (pre-first-writeback)
		before := loadKnowledgeCorpus(seed, maint, logf)
		if before == nil {
			t.Fatal("seed + empty maintained must load")
		}
		// Simulate a novelty writeback landing in the maintained file.
		var b strings.Builder
		if err := knowledge.WriteCorpus(&b, []knowledge.Incident{maintRow}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(maint, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		after := loadKnowledgeCorpus(seed, maint, logf)
		if after == nil {
			t.Fatal("reload after the write must load")
		}
		for ref, q := range map[string]knowledge.Query{
			"pred-ik-1":     {Host: "web01", AlertRule: "NginxDown"}, // the seed-only shape
			"librenms-nl-9": {Host: "db01", AlertRule: "DeviceDown"}, // the freshly written row
		} {
			found := false
			for _, h := range after.Retrieve(q, 5) {
				if h.Incident.ExternalRef == ref {
					found = true
				}
			}
			if !found {
				t.Fatalf("after a runtime write the %s row must STILL be visible (the seed must never be evicted)", ref)
			}
		}
	})
}
