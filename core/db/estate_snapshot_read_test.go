package db

import (
	"context"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/estate"
)

// TG-346. The plane filter IS the feature: both workers publish into estate_snapshot and their graphs
// differ by two orders of magnitude, so a read ordered by recency alone answers with whichever worker
// wrote LAST. On the actuation plane that is usually itself — the relay would hand the impoverished
// graph back to the plane it came from and the divergence alert would clear on a lie. This oracle
// writes BOTH planes' snapshots, impoverished one LAST, and asserts the read still returns the triage
// plane's graph. Only a real Postgres executes the SQL; MemTrialStore-style fakes cannot kill a
// mutation in this query.
func TestLatestSnapshotForPlaneIgnoresANewerForeignPlane(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the estate snapshot plane-filter oracle")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()
	if _, err := p.Pool.Exec(ctx, "DELETE FROM estate_snapshot"); err != nil {
		t.Fatalf("clear fixture table: %v", err)
	}
	s := NewEstateWriteStore(p)

	rich := estate.Snapshot{Edges: []estate.SnapshotEdge{
		{FromType: "guest", FromName: "g1", ToType: "pve_node", ToName: "n1", Rel: "runs_on", Confidence: 0.9, Source: "pve"},
		{FromType: "service", FromName: "s1", ToType: "guest", ToName: "g1", Rel: "depends_on", Confidence: 0.8, Source: "netbox"},
	}}
	poor := estate.Snapshot{Edges: []estate.SnapshotEdge{
		{FromType: "guest", FromName: "g1", ToType: "pve_node", ToName: "n1", Rel: "runs_on", Confidence: 0.9, Source: "pve"},
	}}
	if err := s.Publish(ctx, rich, 4, "triage"); err != nil {
		t.Fatalf("publish triage: %v", err)
	}
	// The actuation plane writes SECOND — the newest row overall is the impoverished one.
	if err := s.Publish(ctx, poor, 1, "actuation"); err != nil {
		t.Fatalf("publish actuation: %v", err)
	}

	snap, at, err := s.LatestSnapshotForPlane(ctx, "triage")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if at.IsZero() {
		t.Error("the snapshot's write time is zero — the relay's staleness bound would compare against " +
			"the epoch and refuse everything (or, worse, a Now-defaulted zero would accept everything)")
	}
	if len(snap.Edges) != 2 {
		t.Fatalf("asked for the TRIAGE plane and got %d edge(s) — a newer foreign-plane row won a read "+
			"that exists precisely to prevent that (the original TG-346 defect: 191 of 502 snapshots in "+
			"24h were the impoverished one, and recency-only reads served them as the estate)", len(snap.Edges))
	}

	// A plane that never published is an ERROR, not an empty snapshot — the relay's vacuity floor
	// depends on being able to distinguish "no snapshot" from "an empty estate".
	if _, _, err := s.LatestSnapshotForPlane(ctx, "never-published"); err == nil {
		t.Error("a plane with no snapshots returned success — the relay cannot tell 'never published' " +
			"from 'published an empty graph', and both would blank the relayed tier silently")
	}
}
