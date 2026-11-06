package db

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/worldmodel"
	"github.com/territory-grounder/grounder/temporal/worlddiscovery"
)

// END-TO-END FOR THE LANE THAT SHIPPED DEAD.
//
// worlddiscovery was built, documented and unit-tested while being wired by nothing: production ran with
// manifest_entry at 0 rows and #manifest permanently blank. Its own cron_test.go was green throughout,
// against an in-memory fake store.
//
// So this test deliberately does NOT use a fake. It drives the real pass through the real pgx store into a
// real Postgres and then reads the rows back through the SAME accessor the HTTP surface uses — because
// every link in that chain was individually proven and the CHAIN was never exercised. A fake store cannot
// catch a column default, a status enum mismatch, or an upsert conflict clause, and those are exactly what
// stands between "the pass ran" and "the operator sees a row".
//
// The pass is observe-only by construction: it drafts (inert until an operator approves) and marks stale
// (which never retires a grant). Nothing here can widen what TG is permitted to do.
type e2eSource struct {
	src   estate.Source
	edges []estate.Edge
	err   error
}

func (f e2eSource) Source() estate.Source                          { return f.src }
func (f e2eSource) Edges(_ context.Context) ([]estate.Edge, error) { return f.edges, f.err }

func svcOn(unit, host string) estate.Edge {
	return estate.Edge{
		From:       estate.Entity{Type: estate.TypeService, Name: unit},
		To:         estate.Entity{Type: estate.TypeHost, Name: host},
		Rel:        estate.RelRunsOn,
		Confidence: 0.9,
		Source:     estate.SourceDeclared,
	}
}

func TestWorldDiscoveryDraftsThroughTheRealStoreAndIsReadableBack(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	const host = "wd-e2e-host01"
	const unit = "wd-e2e-nginx.service"
	cleanup := func() {
		_, _ = p.Exec(ctx, `DELETE FROM manifest_entry WHERE name IN ($1,$2)`, host, unit)
	}
	cleanup()
	defer cleanup()

	store := NewWorldManifestStore(p)
	job := worlddiscovery.Job{
		Sources: []estate.EdgeSource{e2eSource{src: estate.SourceDeclared, edges: []estate.Edge{svcOn(unit, host)}}},
		Store:   store,
		Ledger:  audit.NewLedger(),
		Now:     func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	}

	res, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("discovery pass: %v", err)
	}
	if res.Drafted < 2 {
		t.Fatalf("both sides of the edge are entities and both must be drafted (service + host), got %+v", res)
	}

	// Read back through the accessor the HTTP surface uses — not through a query written for this test.
	// The console renders what AllEntries returns; if a draft lands in a shape that accessor cannot see,
	// the operator's page stays blank while the table is full, which is the failure this whole thread is about.
	entries, _, _, err := store.AllEntries(ctx, 500)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := map[string]worldmodel.Entry{}
	for _, e := range entries {
		if e.Name == host || e.Name == unit {
			got[e.Name] = e
		}
	}
	for _, want := range []string{host, unit} {
		e, ok := got[want]
		if !ok {
			t.Errorf("%q was drafted but is NOT visible through AllEntries — the surface an operator reads. "+
				"A row the console cannot see is the same as no row.", want)
			continue
		}
		if e.Status != worldmodel.StatusDraft {
			t.Errorf("%q must land as a DRAFT (inert until an operator approves), got status %q", want, e.Status)
		}
	}

	// A draft must NOT materialize into the allowlist. This is the safety property that makes arming the
	// lane a read-only act: ApprovedEntries is what becomes permission, and a draft is not in it.
	approved, err := store.ApprovedEntries(ctx)
	if err != nil {
		t.Fatalf("approved: %v", err)
	}
	for _, e := range approved {
		if e.Name == host || e.Name == unit {
			t.Errorf("%q is a DRAFT and must not appear in ApprovedEntries — drafting would then widen "+
				"what TG may act on with no operator in the loop", e.Name)
		}
	}

	// IDEMPOTENCE. DraftEntry is an upsert; a second pass over an unchanged estate must refresh, not
	// accumulate. Without this a 30m cadence would grow the operator's review queue by ~48 rows a day
	// per entity, forever — which is what makes a short interval safe to choose.
	before := len(entries)
	if _, err := job.Run(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	after, _, _, err := store.AllEntries(ctx, 500)
	if err != nil {
		t.Fatalf("read back 2: %v", err)
	}
	if len(after) != before {
		t.Errorf("a second pass over an unchanged estate must not add rows: %d -> %d. DraftEntry is an "+
			"upsert precisely so a periodic pass converges instead of growing the review queue.",
			before, len(after))
	}
}

// TestWorldDiscoveryWithNoSourcesTouchesNothing — the destructive-looking failure the composition root's
// guard exists to prevent, asserted against the real store.
//
// The pass computes disappearance by diffing observations against the manifest. With no sources it observes
// nothing; a naive implementation would conclude every approved entry had vanished and mark the entire
// world model stale. Here it must refuse and change NOTHING.
func TestWorldDiscoveryWithNoSourcesTouchesNothing(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	store := NewWorldManifestStore(p)
	before, _, _, err := store.AllEntries(ctx, 500)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}

	job := worlddiscovery.Job{Sources: nil, Store: store, Ledger: audit.NewLedger()}
	if _, err := job.Run(ctx); err == nil {
		t.Error("a pass with NO sources must refuse (ErrNoSources) rather than diff an empty observation " +
			"against the manifest — that diff marks every approved entry stale, turning one missing config " +
			"into estate-wide drift noise")
	}

	after, _, _, err := store.AllEntries(ctx, 500)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("a refused pass must change nothing, %d -> %d rows", len(before), len(after))
	}
}
