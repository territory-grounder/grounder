package main

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
)

// TG-624 G3: the collateral re-check scans topology dependents only. A host linked to the anchor solely by a
// LEARNED co-occurrence edge (two hosts that alerted together in earlier drills) must not make a separate
// fault on it read as collateral of this heal.
func TestCollateralBlastMembers_SkipsLearnedCoOccurrencePaths(t *testing.T) {
	g := estate.NewGraph()
	until := time.Now().Add(time.Hour)
	// app01 depends on db01 (topology): a heal on db01 can perturb app01.
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeHost, Name: "site01app01"}, To: estate.Entity{Type: estate.TypeHost, Name: "site01db01"},
		Rel: estate.RelDependsOn, Confidence: 0.9, Source: estate.SourceDeclared, ValidUntil: until})
	// far01 merely co-alarmed with db01 (learned): not a dependency.
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeHost, Name: "site02far01"}, To: estate.Entity{Type: estate.TypeHost, Name: "site01db01"},
		Rel: estate.RelDependsOn, Confidence: 0.6, Source: estate.SourceIncident, ValidUntil: until})

	got := map[string]bool{}
	for _, m := range collateralBlastMembers(g, "site01db01") {
		got[m] = true
	}
	if !got["site01db01"] || !got["site01app01"] {
		t.Fatalf("the anchor and its topology dependent must be scanned, got %v", got)
	}
	if got["site02far01"] {
		t.Fatalf("a host reached only through a learned co-occurrence edge must not be scanned as collateral, got %v", got)
	}
	if collateralBlastMembers(nil, "site01db01") != nil || collateralBlastMembers(g, "nowhere") != nil {
		t.Fatal("a nil graph or an unresolvable host must return nil (reported UNKNOWN), never an all-clear set")
	}
}

// TG-655, the 2026-11-07 shape: a hypervisor "depends_on" a guest on another node through a single CHAOS edge,
// learned because it alerted inside an injection's cascade window while six soak faults ran. Its alert must
// not read as collateral of a container heal on that guest; the guest's real topology dependents still do.
func TestCollateralBlastMembers_SkipsChaosLearnedPaths(t *testing.T) {
	g := estate.NewGraph()
	until := time.Now().Add(time.Hour)
	guest := estate.Entity{Type: estate.TypeHost, Name: "site01app02"}
	// Topology: a proxy really depends on the guest (declared).
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeHost, Name: "site01proxy01"}, To: guest,
		Rel: estate.RelDependsOn, Confidence: 0.9, Source: estate.SourceDeclared, ValidUntil: until})
	// Chaos-learned: a hypervisor and an edge host "depend on" it.
	for _, n := range []string{"site01hv03", "site01dmz01"} {
		g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeHost, Name: n}, To: guest,
			Rel: estate.RelDependsOn, Confidence: 0.9, Source: estate.SourceChaos, ValidUntil: until})
	}
	// And a topology dependent reached THROUGH a chaos hop is excluded too (the path is a guess).
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeHost, Name: "site01behind01"}, To: estate.Entity{Type: estate.TypeHost, Name: "site01hv03"},
		Rel: estate.RelDependsOn, Confidence: 0.9, Source: estate.SourceDeclared, ValidUntil: until})

	got := map[string]bool{}
	for _, m := range collateralBlastMembers(g, "site01app02") {
		got[m] = true
	}
	if !got["site01app02"] || !got["site01proxy01"] {
		t.Fatalf("the anchor and its declared dependent must be scanned, got %v", got)
	}
	for _, n := range []string{"site01hv03", "site01dmz01", "site01behind01"} {
		if got[n] {
			t.Fatalf("%s is reached only through a chaos-learned edge and must not be scanned as collateral, got %v", n, got)
		}
	}
}
