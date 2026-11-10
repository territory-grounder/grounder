package main

// TG-681: the estate snapshot's age is published beside its size, so a stale shared world model is a number
// an alert rule can read rather than a property nobody can observe.

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
)

func TestEstateGraphAgeIsPublishedWithTheInstalledFlag(t *testing.T) {
	now := time.Date(2026, 11, 7, 21, 0, 0, 0, time.UTC)
	ss := estateGraphAgeSamples(now.Add(-2*time.Hour), true, now)
	inst, ok := estSample(ss, "tg_estate_graph_installed")
	if !ok || inst.Value != 1 {
		t.Fatalf("an installed snapshot must publish installed=1, got %+v ok=%v", inst, ok)
	}
	age, ok := estSample(ss, "tg_estate_graph_age_seconds")
	if !ok || age.Value != 7200 {
		t.Fatalf("a snapshot installed 2h ago must read 7200s, got %+v ok=%v", age, ok)
	}
}

// Never installed is NOT "zero seconds old": the flag must say so, and the pair must still be emitted (an
// absent series would be indistinguishable from the exporter being gone).
func TestANeverInstalledSnapshotIsFlaggedNotFresh(t *testing.T) {
	now := time.Date(2026, 11, 7, 21, 0, 0, 0, time.UTC)
	ss := estateGraphAgeSamples(time.Time{}, false, now)
	inst, ok := estSample(ss, "tg_estate_graph_installed")
	if !ok || inst.Value != 0 {
		t.Fatalf("a never-installed holder must publish installed=0, got %+v ok=%v", inst, ok)
	}
	if _, ok := estSample(ss, "tg_estate_graph_age_seconds"); !ok {
		t.Fatal("the age series must be emitted even when meaningless — absent and zero must not look alike")
	}
}

// The scrape job appends the age pair to the size samples, reading the holder's own stamp: a rejected refresh
// leaves the stamp alone, so the published age grows exactly as the holder's does.
func TestEstateSizeJobCarriesTheSnapshotAge(t *testing.T) {
	g := estate.NewGraph()
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeLXC, Name: "web01"}, To: estate.Entity{Type: estate.TypePVENode, Name: "pve01"}, Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	installed := time.Now().UTC().Add(-45 * time.Minute)
	h := estate.NewHolderWithClock(g, func() time.Time { return installed })
	read := startEstateSizeJob(h, nil)
	ss := read()
	if _, ok := estSample(ss, "tg_estate_edges"); !ok {
		t.Fatal("the size samples must still be published")
	}
	age, ok := estSample(ss, "tg_estate_graph_age_seconds")
	if !ok {
		t.Fatal("the age sample must ride the same scrape job as the size")
	}
	if age.Value < 44*60 || age.Value > 50*60 {
		t.Fatalf("the published age must reflect the holder's stamp (~45m), got %vs", age.Value)
	}
}
