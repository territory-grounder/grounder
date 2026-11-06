package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
)

// TG-206a: the pgx EdgeDisproofs store persists per-edge disproofs durably (migration 0075) so a contradiction
// survives the restart the in-memory DecayReport did not. Real-pg round-trip: Record a pass's disproofs, List
// them back with attribution (deviation_key/action_id), decayed_to, aged_out and the pass time intact. Gated on
// TG_TEST_POSTGRES_DSN (it Migrates the empty db itself).
func TestEdgeDisproofRecordListRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the durable edge-disproof test")
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
	s := NewEdgeDisproofs(p)

	dev := "pve01|nl|web7-" + t.Name()
	at := time.Now().UTC().Truncate(time.Microsecond)
	rows := []estate.EdgeDisproof{
		{EdgeKey: "host:pve01|depends_on|host:web7", From: "pve01", Rel: "depends_on", To: "web7",
			Target: "pve01", DeviationKey: dev, ActionID: "act-1", DecayedTo: 0.2, AgedOut: false},
		{EdgeKey: "host:pve01|depends_on|host:cache9", From: "pve01", Rel: "depends_on", To: "cache9",
			Target: "pve01", DeviationKey: dev, ActionID: "act-1", DecayedTo: 0.0, AgedOut: true},
	}
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM edge_disproof WHERE deviation_key = $1", dev) }()

	n, err := s.Record(ctx, at, rows)
	if err != nil || n != 2 {
		t.Fatalf("record: n=%d err=%v", n, err)
	}
	// An empty pass is a no-op (never a spurious row / never an error).
	if n0, err := s.Record(ctx, at, nil); err != nil || n0 != 0 {
		t.Fatalf("empty record: n=%d err=%v, want 0/nil", n0, err)
	}

	all, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byEdge := map[string]estate.RecordedEdgeDisproof{}
	for _, r := range all {
		if r.DeviationKey == dev {
			byEdge[r.To] = r
		}
	}
	if len(byEdge) != 2 {
		t.Fatalf("round-trip returned %d rows for %s, want 2", len(byEdge), dev)
	}
	if r := byEdge["web7"]; r.ActionID != "act-1" || r.Target != "pve01" || r.DecayedTo != 0.2 || r.AgedOut || !r.ObservedAt.Equal(at) {
		t.Errorf("web7 disproof lost fidelity through the pgx round-trip: %+v (want action act-1, decayedTo 0.2, not aged, at %v)", r, at)
	}
	if r := byEdge["cache9"]; !r.AgedOut || r.DecayedTo != 0.0 || r.EdgeKey != "host:pve01|depends_on|host:cache9" {
		t.Errorf("cache9 disproof must be aged_out with decayed_to 0 and its edge key: %+v", r)
	}
}
