package estate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TG-346. The relay is the mechanism that lets the actuation plane's mutation gate reason over the
// triage plane's persisted graph without holding a single read-triage credential. Its failure modes are
// therefore gate-safety failure modes: a stale relay hides a decommission from the gate, an empty relay
// blanks the estate mid-incident (TG-375's shape), and a provenance-erasing relay collapses every
// relayed edge under one label.

func relayFixtureSnapshot() Snapshot {
	return Snapshot{Edges: []SnapshotEdge{
		{FromType: "guest", FromName: "g1", ToType: "pve_node", ToName: "n1", Rel: "runs_on", Confidence: 0.9, Source: "pve"},
		{FromType: "service", FromName: "s1", ToType: "guest", ToName: "g1", Rel: "depends_on", Confidence: 0.85, Source: "netbox"},
	}}
}

func TestRelayPreservesPerEdgeProvenance(t *testing.T) {
	src := SnapshotRelaySource{
		Load:   func(context.Context) (Snapshot, time.Time, error) { return relayFixtureSnapshot(), time.Now(), nil },
		MaxAge: time.Hour,
	}
	edges, err := src.Edges(context.Background())
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("relayed %d edges, want 2", len(edges))
	}
	// The relay's OWN slug is for error reporting only; a relayed edge keeps the source it was built
	// with, or TG-391's provenance rendering collapses and the confidence ratchet fights under one label.
	for _, e := range edges {
		if e.Source == SourceSnapshotRelay {
			t.Fatalf("edge %s->%s was re-stamped %q — the original provenance is erased", e.From.Name, e.To.Name, e.Source)
		}
	}
	if edges[0].Source != SourcePVE || edges[1].Source != SourceNetbox {
		t.Fatalf("provenance not preserved: got %q, %q", edges[0].Source, edges[1].Source)
	}
	if edges[0].Confidence != 0.9 {
		t.Fatalf("confidence not preserved: %v", edges[0].Confidence)
	}
}

func TestRelayRefusesAStaleSnapshot(t *testing.T) {
	now := time.Now()
	src := SnapshotRelaySource{
		Load: func(context.Context) (Snapshot, time.Time, error) {
			return relayFixtureSnapshot(), now.Add(-2 * time.Hour), nil
		},
		MaxAge: 30 * time.Minute,
		Now:    func() time.Time { return now },
	}
	if _, err := src.Edges(context.Background()); err == nil {
		t.Fatal("a 2h-old snapshot passed a 30m bound. A stale relay serves the gate an estate that may " +
			"predate a decommission or a collapse — the refusal (with the prior graph kept by per-source " +
			"isolation) is the safe outcome.")
	} else if !strings.Contains(err.Error(), "stale") {
		t.Fatalf("the refusal does not say WHY: %v", err)
	}
}

func TestRelayRefusesAnEmptySnapshot(t *testing.T) {
	src := SnapshotRelaySource{
		Load:   func(context.Context) (Snapshot, time.Time, error) { return Snapshot{}, time.Now(), nil },
		MaxAge: time.Hour,
	}
	if _, err := src.Edges(context.Background()); err == nil {
		t.Fatal("a 0-edge snapshot was relayed as truth. An empty snapshot is an outage artifact; " +
			"installing it blanks the relayed tier during exactly the incident where the gate needs it.")
	}
}

func TestRelayPropagatesLoaderFailureLoudly(t *testing.T) {
	boom := errors.New("db down")
	src := SnapshotRelaySource{
		Load:   func(context.Context) (Snapshot, time.Time, error) { return Snapshot{}, time.Time{}, boom },
		MaxAge: time.Hour,
	}
	if _, err := src.Edges(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("loader failure not propagated: %v", err)
	}
}

func TestRestoreEdgesRoundTripsExport(t *testing.T) {
	g := NewGraph()
	g.Upsert(Edge{From: Entity{Type: "guest", Name: "g1"}, To: Entity{Type: "pve_node", Name: "n1"},
		Rel: "runs_on", Confidence: 0.9, Source: SourcePVE})
	restored := g.Export().RestoreEdges()
	if len(restored) != 1 {
		t.Fatalf("round trip lost edges: %d", len(restored))
	}
	e := restored[0]
	if e.From.Name != "g1" || e.To.Name != "n1" || e.Rel != "runs_on" || e.Source != SourcePVE || e.Confidence != 0.9 {
		t.Fatalf("round trip corrupted the edge: %+v", e)
	}
}
