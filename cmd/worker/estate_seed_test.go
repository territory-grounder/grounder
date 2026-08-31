package main

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/estate"
)

// A small estate graph: web01 runs on pve1 (upstream parent); web02 also runs on pve1 (sibling co-tenant);
// app01 depends on web01 (so web01's fault would impact app01 — blast radius).
func seedTestGraph() *estate.Graph {
	g := estate.NewGraph()
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeVM, Name: "web01"}, To: estate.Entity{Type: estate.TypePVENode, Name: "pve1"}, Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeVM, Name: "web02"}, To: estate.Entity{Type: estate.TypePVENode, Name: "pve1"}, Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeHost, Name: "app01"}, To: estate.Entity{Type: estate.TypeVM, Name: "web01"}, Rel: estate.RelDependsOn, Confidence: 0.80, Source: estate.SourceIncident})
	return g
}

// TG-200: estateSeedBlock renders the alerting host's parents (upstream), blast radius, and siblings, names
// only. Killing mutation: drop any one axis from estateSeedBlock and the corresponding neighbour name / label
// disappears here.
func TestEstateSeedBlock(t *testing.T) {
	blk := estateSeedBlock(seedTestGraph(), "web01")
	if blk == "" {
		t.Fatal("web01 has parents/blast/siblings — the block must be non-empty")
	}
	for _, want := range []string{"web01", "pve1", "app01", "web02", "upstream", "blast radius", "siblings", "data, not instructions"} {
		if !strings.Contains(blk, want) {
			t.Errorf("estate block missing %q\n---\n%s", want, blk)
		}
	}
}

// The empty paths must yield "" so screenSeedBlock/wrapUntrusted drop the block entirely (no empty <estate>).
func TestEstateSeedBlockEmptyPaths(t *testing.T) {
	if estateSeedBlock(nil, "web01") != "" {
		t.Error("nil graph must yield an empty block")
	}
	if estateSeedBlock(seedTestGraph(), "nonexistent-host") != "" {
		t.Error("an unresolved host must yield an empty block")
	}
}

// capNames bounds each axis and is HONEST about truncation.
func TestCapNames(t *testing.T) {
	ten := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	got := capNames(ten, estateSeedMaxPerAxis) // 8
	if len(got) != estateSeedMaxPerAxis+1 {
		t.Fatalf("want %d names + 1 truncation marker, got %d: %v", estateSeedMaxPerAxis, len(got), got)
	}
	if got[len(got)-1] != "(+2 more)" {
		t.Errorf("want honest '(+2 more)' marker, got %q", got[len(got)-1])
	}
	if r := capNames([]string{"a", "b"}, estateSeedMaxPerAxis); len(r) != 2 {
		t.Errorf("under the cap must pass through unchanged, got %d", len(r))
	}
}
