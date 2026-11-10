package db

import (
	"context"
	"os"
	"testing"
)

func TestExcludingAControlRemovesItFromEverySessionCell(t *testing.T) {
	dsn := os.Getenv("TG_TEST_DSN")
	if dsn == "" {
		t.Skip("TG_TEST_DSN unset: needs the migrated pgvector fixture")
	}
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	f := seedScopeFixture(ctx, t, p)
	store := NewAxisReadStore(p)
	base, err := store.AggregateScoped(ctx, AxisScope{Since: f.base, Until: f.until})
	if err != nil {
		t.Fatal(err)
	}
	ref := f.run + "-gr1"
	got, err := store.AggregateScoped(ctx, AxisScope{Since: f.base, Until: f.until, ExcludeRefs: []string{ref}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != base.Total-1 {
		t.Fatalf("total %d, want %d", got.Total, base.Total-1)
	}
	if got.MutatedCount != base.MutatedCount-1 {
		t.Fatalf("mutated %d, want %d", got.MutatedCount, base.MutatedCount-1)
	}
	if base.SuspiciousActuations != 1 || got.SuspiciousActuations != 0 {
		t.Fatalf("suspicious actuations %d -> %d, want 1 -> 0", base.SuspiciousActuations, got.SuspiciousActuations)
	}
	bc, err := store.WindowCensus(ctx, f.base, f.until)
	if err != nil {
		t.Fatal(err)
	}
	gc, err := store.WindowCensus(ctx, f.base, f.until, ref)
	if err != nil {
		t.Fatal(err)
	}
	if bc.TriagesBySite["dc2"] != gc.TriagesBySite["dc2"]+1 {
		t.Fatalf("census dc2 %d -> %d", bc.TriagesBySite["dc2"], gc.TriagesBySite["dc2"])
	}
	present, err := store.RefsPresent(ctx, []string{ref, f.run + "-nothing"}, f.base, f.until)
	if err != nil {
		t.Fatal(err)
	}
	if len(present) != 1 || present[0] != ref {
		t.Fatalf("present %v", present)
	}
	if none, err := store.AggregateScoped(ctx, AxisScope{Since: f.base, Until: f.until, ExcludeRefs: nil}); err != nil || none.Total != base.Total {
		t.Fatalf("no exclusions must change nothing: %v %d", err, none.Total)
	}
}
