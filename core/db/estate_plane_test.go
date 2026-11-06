package db

import (
	"context"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/estate"
)

// ★ THE ACTUATION PLANE'S FRAGMENT MUST NEVER BE SERVED AS THE ESTATE (TG-346).
//
// Both workers publish to estate_snapshot. Measured live 2026-08-06 they wrote two seconds apart:
//
//	03:12:32   410 nodes  1863 edges   <- triage
//	03:12:30    20 nodes    17 edges   <- actuation
//
// 191 of 502 snapshots in 24h were the impoverished one, and Latest() ordered by captured_at alone. It had
// not gone wrong yet only because the triage worker consistently writes a couple of seconds later — an
// accident of timing, not a guarantee. This drives the real pgx path, because an ORDER BY that quietly
// re-admits the actuation row is exactly what a fake cannot catch.
func TestLatestNeverReturnsTheActuationPlanesGraph(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database")
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
	if _, err := p.Exec(ctx, "DELETE FROM estate_snapshot"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	w := NewEstateWriteStore(p)
	// Triage first, actuation LAST — the ordering that currently saves the production query by accident.
	// If the plane filter is absent this test fails, which is the whole point.
	big := estate.Snapshot{Nodes: makeNodes(5), Edges: makeEdges(9)}
	small := estate.Snapshot{Nodes: makeNodes(1), Edges: makeEdges(1)}
	if err := w.Publish(ctx, big, 3, "triage"); err != nil {
		t.Fatalf("publish triage: %v", err)
	}
	if err := w.Publish(ctx, small, 1, "actuation"); err != nil {
		t.Fatalf("publish actuation: %v", err)
	}

	got, err := NewEstateReadStore(p).Latest(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if !got.Found {
		t.Fatal("Latest found nothing after two publishes")
	}
	if got.EdgeCount != 9 {
		t.Fatalf("Latest returned a %d-edge graph; want the 9-edge TRIAGE graph. The actuation plane cannot "+
			"hold estate read credentials by design, so its graph is a fragment — serving it as the estate "+
			"means the console and every consumer see an estate two orders of magnitude too small.", got.EdgeCount)
	}
}

// Rows written before the discriminator existed carry 'both' and must still be served, or an upgrade blanks
// the estate view until the next publish.
func TestPreSplitSnapshotsAreStillServed(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database")
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
	if _, err := p.Exec(ctx, "DELETE FROM estate_snapshot"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	// An empty plane records as "both" — the historic posture — never as triage.
	if err := NewEstateWriteStore(p).Publish(ctx, estate.Snapshot{Nodes: makeNodes(2), Edges: makeEdges(4)}, 2, ""); err != nil {
		t.Fatalf("publish: %v", err)
	}
	got, err := NewEstateReadStore(p).Latest(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if !got.Found || got.EdgeCount != 4 {
		t.Errorf("a pre-split ('both') snapshot was not served: found=%v edges=%d. An upgrade must not blank "+
			"the estate view.", got.Found, got.EdgeCount)
	}

	// AND IT MUST BE RECORDED AS 'both', NOT 'triage'. Being served is not enough — both values pass the
	// read filter, so a writer that defaulted an unknown plane to "triage" would satisfy the check above
	// while labelling an actuation-plane fragment as the estate. Read the column, not the outcome.
	var plane string
	if err := p.Pool.QueryRow(ctx,
		`SELECT plane FROM estate_snapshot ORDER BY captured_at DESC LIMIT 1`).Scan(&plane); err != nil {
		t.Fatalf("read plane column: %v", err)
	}
	if plane != "both" {
		t.Errorf("an empty plane recorded as %q, want \"both\". Defaulting to a plane the reader trusts "+
			"means any caller that forgets to stamp gets its graph served as the estate — which for the "+
			"actuation worker is a 17-edge fragment.", plane)
	}
}

func makeNodes(n int) []estate.Entity {
	out := make([]estate.Entity, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, estate.Entity{Type: "host", Name: string(rune('a' + i))})
	}
	return out
}

func makeEdges(n int) []estate.SnapshotEdge {
	out := make([]estate.SnapshotEdge, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, estate.SnapshotEdge{
			FromType: "host", FromName: "a", ToType: "host", ToName: string(rune('b' + i)),
			Rel: "depends_on", Confidence: 0.9, Source: "test",
		})
	}
	return out
}
