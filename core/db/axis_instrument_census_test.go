package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TG-672 — THE INSTRUMENT-IDENTITY CENSUS LANES, over a real Postgres, on the TG-670 A4 fixture (its table is in
// axis_a4_census_test.go). The lanes are the harness check's: deterministic (skip1), infra-failed (fail1) and
// not-composed (nc1) are OUTSIDE the identity check; every other row is inside it, with "" its own value.
//
//	stamped X:  ok1 fb1 fbh1 fbold fbboth good1 bad1 bad2 esc1 fail2   → 10 under X
//	left "":    unrec1                                                 → 1 unrecorded (against uniformity)
//	outside:    skip1 fail1 nc1                                        → not in InstrumentIDs at all
//
// Expected values are read off that table, never off the implementation.
func TestWindowCensus_InstrumentIdentityLanes(t *testing.T) {
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
	f := seedA4Fixture(ctx, t, p)
	idX := strings.Repeat("ab", 32)
	stamped := []string{"ok1", "fb1", "fbh1", "fbold", "fbboth", "good1", "bad1", "bad2", "esc1", "fail2"}
	for _, short := range stamped {
		if _, err := p.Exec(ctx, `UPDATE session_triage SET instrument_id = $1 WHERE external_ref = $2`, idX, f.ref(short)); err != nil {
			t.Fatalf("stamp %s: %v", short, err)
		}
	}
	// Stamping the OUTSIDE lanes too proves they are excluded by lane, not by an empty id.
	for _, short := range []string{"skip1", "fail1", "nc1"} {
		if _, err := p.Exec(ctx, `UPDATE session_triage SET instrument_id = $1 WHERE external_ref = $2`, strings.Repeat("cd", 32), f.ref(short)); err != nil {
			t.Fatalf("stamp %s: %v", short, err)
		}
	}
	c, err := NewAxisReadStore(p).WindowCensus(ctx, f.base, f.until)
	if err != nil {
		t.Fatal(err)
	}
	if c.InstrumentIDs[idX] != 10 || c.InstrumentIDs[""] != 1 || len(c.InstrumentIDs) != 2 {
		t.Fatalf("InstrumentIDs = %v, want X×10 and \"\"×1 (unrec1); the deterministic, infra-failed and not-composed lanes are outside the check whatever they are stamped with", c.InstrumentIDs)
	}
	// MUTATION CONTROL: the identity census must move with the column — re-stamp one looped row and the map splits.
	if _, err := p.Exec(ctx, `UPDATE session_triage SET instrument_id = $1 WHERE external_ref = $2`, strings.Repeat("ef", 32), f.ref("ok1")); err != nil {
		t.Fatal(err)
	}
	c, err = NewAxisReadStore(p).WindowCensus(ctx, f.base, f.until)
	if err != nil {
		t.Fatal(err)
	}
	if c.InstrumentIDs[idX] != 9 || c.InstrumentIDs[strings.Repeat("ef", 32)] != 1 || len(c.InstrumentIDs) != 3 {
		t.Fatalf("after re-stamping one row: %v, want X×9, EF×1, \"\"×1", c.InstrumentIDs)
	}
	// DISCLOSURES, each seeded once so a broken query cannot pass. The fixture's 14 incidents all carry an dc1
	// ingest stamp and are organic and unmutated; one of them (good1) is made synthetic AND mutated on a host of its
	// own; one injected fault lands at an dc1 host; one estate snapshot lands in the window.
	host := "dc1" + f.run + "-h"
	if _, err := p.Exec(ctx, `UPDATE session_triage SET synthetic = true, mutated = true, host = $2 WHERE external_ref = $1`, f.ref("good1"), host); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE ingest_alert SET host = $2 WHERE external_ref = $1`, f.ref("good1"), host); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO injected_fault (host, fault_type, note, injected_at) VALUES ($1,'device-down',$2,$3)`, host, f.run, f.base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO estate_snapshot (node_count, edge_count, source_count, graph_json, schema_version, plane, created_at) VALUES (1, 0, 1, '{}'::jsonb, 1, 'triage', $1)`, f.base.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	c, err = NewAxisReadStore(p).WindowCensus(ctx, f.base, f.until)
	if err != nil {
		t.Fatal(err)
	}
	if c.SyntheticBySite["dc1"] != 1 || c.OrganicBySite["dc1"] != 13 || c.MutatedByHostBySite["dc1"][host] != 1 ||
		c.FaultClassesBySite["dc1"]["device-down"] != 1 || c.EstateSnapshots != 1 {
		t.Fatalf("disclosures: synthetic=%v organic=%v mutated-by-host=%v fault-classes=%v snapshots=%d — want exactly the one seeded row in each",
			c.SyntheticBySite, c.OrganicBySite, c.MutatedByHostBySite, c.FaultClassesBySite, c.EstateSnapshots)
	}
	// MUTATION CONTROL: the disclosures read the columns, not the row's existence — un-mutate the same incident and
	// the host share drops to nothing while the synthetic split is unchanged.
	if _, err := p.Exec(ctx, `UPDATE session_triage SET mutated = false WHERE external_ref = $1`, f.ref("good1")); err != nil {
		t.Fatal(err)
	}
	c, err = NewAxisReadStore(p).WindowCensus(ctx, f.base, f.until)
	if err != nil {
		t.Fatal(err)
	}
	if c.SyntheticBySite["dc1"] != 1 || c.OrganicBySite["dc1"] != 13 || c.MutatedByHostBySite["dc1"][host] != 0 {
		t.Fatalf("after un-mutating: synthetic=%v organic=%v mutated-by-host=%v", c.SyntheticBySite, c.OrganicBySite, c.MutatedByHostBySite)
	}
}
