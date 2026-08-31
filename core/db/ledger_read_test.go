package db

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

// GOLDEN-FIXTURE TEST FOR THE LEDGER READ PROJECTION (the console's TIME column).
//
// Runs against a REAL PostgreSQL, gated on TG_TEST_DSN, for the same reason axis_read_test.go does: this is
// a column-projection defect, and a pgx fake has already hidden a field-drop in this repository once. A fake
// would have happily "returned" a created_at that the SELECT never asked for.
//
// ★ THE FIXTURE SEEDS A DISTINCTIVE PAST TIMESTAMP ON PURPOSE. governance_ledger.created_at defaults to
// now(), so if the fixture also used now() then a mutation replacing the column with now() would be
// observationally IDENTICAL to the correct code and the control could not fire. That is cause #4 in this
// project's catalogue of controls that cannot fail ("the fixture couldn't discriminate"), and it is why the
// rows below are stamped 72 hours in the past with a minute between them.

func seedLedgerRows(ctx context.Context, t *testing.T, p *Pool) (func(), []time.Time) {
	t.Helper()
	// Truncate to the microsecond: timestamptz keeps microseconds, so a nanosecond-precision Go time would
	// never compare equal on the way back and the test would fail for a reason that is not the defect.
	base := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Microsecond)
	const pfx = "gold-ledger-"

	// A HASH CHAIN CANNOT BE PARTIALLY OWNED, so this fixture clears the WHOLE table rather than its own
	// action_id prefix.
	//
	// governance_ledger is ONE chain: seq is the primary key, monotonic and gap-free from 1, and each row's
	// prev_hash links to the row before it. TestLedgerAll_ProjectionDoesNotBreakTheChain calls VerifyChain
	// over everything in the table. So any row this fixture does not control — left by an earlier crashed
	// run, or by ledger_context_test.go / plane_roles_test.go, which also insert here — either collides on
	// seq 1..3 or sits in the middle of the chain being verified.
	//
	// Deleting by prefix looked correct and was not: it freed the action_ids and left the seqs. Against a
	// long-lived local Postgres that made every SECOND run fail with "duplicate key value violates unique
	// constraint governance_ledger_pkey" — a message pointing at the ledger rather than at the fixture. It
	// cost several clear-the-table-and-rerun cycles on 2026-08-06 before the cause was read rather than
	// worked around.
	//
	// Offsetting the seq from MAX(seq) was tried first and is WRONG for a different reason: the rows insert
	// cleanly and then VerifyChain fails, because a chain starting mid-table with prev_hash "" is exactly
	// what a broken linkage looks like. The fixture has to own the table or it cannot verify a chain at all.
	cleanup := func() { _, _ = p.Exec(ctx, `DELETE FROM governance_ledger`) }
	cleanup()

	// A real chain: each row's prev_hash is the previous row's hash, so VerifyChain has something to walk.
	l := audit.NewLedger()
	want := make([]time.Time, 0, 3)
	for i, d := range []audit.GovDecision{
		{Decision: "AUTO", Reason: "graduated op-class", ActionID: pfx + "1"},
		{Decision: "POLL_PAUSE", Reason: "ood-novel-incident", ActionID: pfx + "2", Withheld: true},
		{Decision: "AUTO_NOTICE", Reason: "canary-policy-pinned", ActionID: pfx + "3"},
	} {
		e, err := l.Append(d)
		if err != nil {
			t.Fatalf("building the fixture chain: %v", err)
		}
		ts := base.Add(time.Duration(i) * time.Minute)
		want = append(want, ts)
		if _, err := p.Exec(ctx, `
			INSERT INTO governance_ledger (seq, decision, reason, action_id, withheld, prev_hash, hash, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			e.Seq, e.Decision, e.Reason, e.ActionID, e.Withheld, e.PrevHash, e.Hash, ts); err != nil {
			t.Fatalf("seeding governance_ledger: %v", err)
		}
	}
	return cleanup, want
}

// TestLedgerAll_ProjectsTheStoredCreatedAt is the defect as an oracle. Before this change the console's
// ledger TIME column was blank on every row: created_at existed in the table, was absent from the SELECT,
// absent from the DTO, and hardcoded to "" in the console.
func TestLedgerAll_ProjectsTheStoredCreatedAt(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()

	cleanup, want := seedLedgerRows(ctx, t, p)
	defer cleanup()

	all, err := NewLedgerStore(p).All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	got := map[string]time.Time{}
	for _, e := range all {
		got[e.ActionID] = e.CreatedAt
	}
	for i, ref := range []string{"gold-ledger-1", "gold-ledger-2", "gold-ledger-3"} {
		ts, ok := got[ref]
		if !ok {
			t.Fatalf("%s missing from the projection entirely", ref)
		}
		if ts.IsZero() {
			t.Errorf("%s came back with a ZERO created_at — the column is in the table and in the DTO but "+
				"the read is not projecting it, which is exactly why the console's TIME column rendered "+
				"blank for every row", ref)
			continue
		}
		if !ts.UTC().Equal(want[i].UTC()) {
			t.Errorf("%s created_at = %v, want the STORED value %v — a non-zero but wrong time means the "+
				"read is reporting some other clock (now(), the query time) rather than when the decision "+
				"was actually appended", ref, ts.UTC(), want[i].UTC())
		}
	}
}

// TestLedgerAll_ProjectionDoesNotBreakTheChain — reading a new column must not disturb INV-19. This is the
// half that would matter if the SELECT were reordered: scanning created_at into the wrong destination would
// corrupt a hashed field and VerifyChain would catch it.
func TestLedgerAll_ProjectionDoesNotBreakTheChain(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()

	cleanup, _ := seedLedgerRows(ctx, t, p)
	defer cleanup()

	all, err := NewLedgerStore(p).All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) < 3 {
		t.Fatalf("expected at least the 3 seeded rows, got %d", len(all))
	}
	if err := audit.VerifyChain(all[:3]); err != nil {
		t.Fatalf("the seeded chain no longer verifies after adding created_at to the SELECT: %v — a "+
			"projection change has corrupted a HASHED field, which would report untampered rows as "+
			"tampered", err)
	}
}
