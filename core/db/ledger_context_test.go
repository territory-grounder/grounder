package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

// THE DEADLINE ORACLE FOR THE DURABLE LEDGER WRITE (TG-277).
//
// Runs against a REAL PostgreSQL, gated on TG_TEST_DSN, because the defect is entirely about which
// context pgx receives. A fake sink cannot fail this: it would honour whatever context the test hands it
// regardless of what the production INSERT does, which is precisely the blindness that let
// context.Background() sit on this path.
//
// THE DEFECT. Persist ran its INSERT on context.Background(). The Ledger holds its chain gate across this
// call, so on 2026-08-04 a stalled substrate could block a sealed-secret write for the whole 15s
// StartToCloseTimeout, and every other governance decision in the worker behind it, with no way for any
// caller to give up. Measured live: the activity is ~12ms with the ledger at seq ~8800, so second-scale
// time in here is always the substrate — and the caller has to be able to say so.

func TestLedgerPersistContext_HonoursTheCallersDeadline(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()
	s := NewLedgerStore(p)

	const pfx = "tg277-deadline-"
	cleanup := func() { _, _ = p.Exec(ctx, `DELETE FROM governance_ledger WHERE action_id LIKE $1`, pfx+"%") }
	cleanup()
	// defer, not t.Cleanup: t.Cleanup runs AFTER this function's defers, by which point p is closed and the
	// delete silently no-ops — leaving a seq behind that collides with the next test's fixture.
	defer cleanup()

	seq := reserveLedgerSeq(ctx, t, p)
	entry := audit.LedgerEntry{
		Seq: seq, Decision: "secret:put", Reason: "TG-277 deadline oracle",
		ActionID: pfx + "1", PrevHash: "", Hash: "deadbeef",
	}

	// An already-expired deadline must reach pgx and stop the INSERT. On context.Background() it never
	// would: the row would land and the caller's budget would mean nothing.
	expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()
	err = s.PersistContext(expired, entry)
	if err == nil {
		t.Fatal("PersistContext committed a governance row under an EXPIRED deadline: the caller's budget " +
			"does not reach the durable write, so a stalled Postgres holds the chain gate — and the calling " +
			"activity — until the database answers, which is the 15s unattributable secret-write timeout " +
			"TG-277 was filed for")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("PersistContext failed for the wrong reason: %v (want context.DeadlineExceeded)", err)
	}

	var n int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM governance_ledger WHERE action_id = $1`, pfx+"1").Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d row(s) landed despite the expired deadline — the write was not actually bounded", n)
	}

	// The same store with a live deadline still writes: the bound must not have broken the happy path.
	live, lcancel := context.WithTimeout(ctx, 10*time.Second)
	defer lcancel()
	if err := s.PersistContext(live, entry); err != nil {
		t.Fatalf("PersistContext with a live deadline: %v", err)
	}
	if err := p.QueryRow(ctx, `SELECT count(*) FROM governance_ledger WHERE action_id = $1`, pfx+"1").Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 1 {
		t.Fatalf("the bounded write stored %d rows, want 1 — governance decisions would go unrecorded", n)
	}
}

// reserveLedgerSeq picks a sequence past the chain's tail so the fixture never collides with real rows
// (governance_ledger.seq is the PK, and a duplicate is a hard error by design).
func reserveLedgerSeq(ctx context.Context, t *testing.T, p *Pool) int64 {
	t.Helper()
	var maxSeq *int64
	if err := p.QueryRow(ctx, `SELECT max(seq) FROM governance_ledger`).Scan(&maxSeq); err != nil {
		t.Fatalf("reading the ledger tail: %v", err)
	}
	if maxSeq == nil {
		return 1
	}
	return *maxSeq + 1
}
