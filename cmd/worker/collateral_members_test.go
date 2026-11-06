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
