package db

// THE KILLING ORACLE FOR THE LEDGER-HEAD ANCHOR (TG-80 P1#1), against a REAL Postgres.
//
// The anchor exists to catch the two tampers VerifyChain cannot see — a truncated tail and a re-linked HEAD
// both leave a self-consistent chain behind. So the oracle EXECUTES those mutations on a live ledger AFTER an
// anchor is recorded, and asserts the split: VerifyChain (the pre-anchor control) stays GREEN over the
// tampered-but-consistent chain, while VerifyAgainstAnchors goes RED. A pgx fake could not stand in here —
// the whole point is that the mutation is a real DELETE/UPDATE the SQL privilege boundary would (for
// tg_runtime) forbid and a privileged role can still perform.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
)

// seedAnchorChain clears governance_ledger and seeds a fresh n-row hash chain, returning the rows. Like
// seedLedgerRows it OWNS the whole table: seq is the PK, gap-free from 1, and VerifyChain walks the lot, so a
// chain cannot be partially owned.
func seedAnchorChain(ctx context.Context, t *testing.T, p *Pool, n int) []audit.LedgerEntry {
	t.Helper()
	if _, err := p.Exec(ctx, `DELETE FROM governance_ledger`); err != nil {
		t.Fatalf("clear governance_ledger: %v", err)
	}
	l := audit.NewLedger()
	var rows []audit.LedgerEntry
	for i := 1; i <= n; i++ {
		e, err := l.Append(audit.GovDecision{
			Decision: "AUTO", Reason: fmt.Sprintf("seed-%d", i), ActionID: fmt.Sprintf("anchor-seed-%d", i)})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if _, err := p.Exec(ctx, `
			INSERT INTO governance_ledger (seq, decision, reason, action_id, withheld, prev_hash, hash)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			e.Seq, e.Decision, e.Reason, e.ActionID, e.Withheld, e.PrevHash, e.Hash); err != nil {
			t.Fatalf("seed governance_ledger seq %d: %v", i, err)
		}
		rows = append(rows, e)
	}
	return rows
}

func clearLedgerAndAnchors(ctx context.Context, p *Pool) func() {
	return func() {
		_, _ = p.Exec(ctx, `DELETE FROM governance_ledger`)
		_, _ = p.Exec(ctx, `DELETE FROM ledger_anchor`)
	}
}

func TestLedgerAnchor_DetectsTailTruncation(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()

	rows := seedAnchorChain(ctx, t, p, 6)
	cleanup := clearLedgerAndAnchors(ctx, p)
	defer cleanup()
	if _, err := p.Exec(ctx, `DELETE FROM ledger_anchor`); err != nil {
		t.Fatalf("clear ledger_anchor: %v", err)
	}

	ls := NewLedgerStore(p)
	anchors := NewAnchorStore(p)

	// Record an anchor over the current HEAD (seq 6), through the real store — a round-trip.
	hs, err := ls.Head(ctx, audit.DefaultAnchorWindow)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if hs.Seq != 6 || hs.Hash != rows[5].Hash || len(hs.Recent) != 6 {
		t.Fatalf("Head read the wrong HEAD/window: seq=%d hash=%s window=%d", hs.Seq, hs.Hash, len(hs.Recent))
	}
	anchor := audit.ComputeAnchor(hs)
	if err := anchors.Record(ctx, audit.DomainGovernanceLedger, anchor); err != nil {
		t.Fatalf("record anchor: %v", err)
	}

	before, err := ls.All(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	stored, err := anchors.Anchors(ctx, audit.DomainGovernanceLedger)
	if err != nil {
		t.Fatalf("read anchors: %v", err)
	}
	if len(stored) != 1 || stored[0].Digest != anchor.Digest || stored[0].Seq != 6 || stored[0].WindowSize != 6 {
		t.Fatalf("anchor did not round-trip through the store: got %v, want the seq-6 digest %s", stored, anchor.Digest)
	}
	if err := audit.VerifyChain(before); err != nil {
		t.Fatalf("the intact seeded chain failed VerifyChain: %v", err)
	}
	if err := audit.VerifyAgainstAnchors(audit.RowRefsOf(before), stored); err != nil {
		t.Fatalf("the intact chain was misread as tampered by its own anchor: %v", err)
	}

	// EXECUTE THE TAMPER: delete the HEAD row (seq 6). Superuser can; tg_runtime (migration 0015) could not —
	// this is the privileged prune / mis-scoped retention job the anchor exists to make loud.
	tag, err := p.Exec(ctx, `DELETE FROM governance_ledger WHERE seq = $1`, rows[len(rows)-1].Seq)
	if err != nil {
		t.Fatalf("tamper DELETE: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("tamper DELETE affected %d rows, want 1", tag.RowsAffected())
	}

	after, err := ls.All(ctx)
	if err != nil {
		t.Fatalf("all post-tamper: %v", err)
	}

	// THE CONTROL: VerifyChain is BLIND — the surviving 1..5 prefix is a perfect chain (green before the
	// anchor existed / with verify disabled).
	if err := audit.VerifyChain(after); err != nil {
		t.Fatalf("precondition failed: a truncated tail should still pass VerifyChain, got %v", err)
	}
	// THE CATCH: the anchor sees the HEAD regressed from 6 to 5 (red only because the anchor exists).
	if err := audit.VerifyAgainstAnchors(audit.RowRefsOf(after), stored); !errors.Is(err, audit.ErrAnchorMismatch) {
		t.Fatalf("the DELETEd tail row was NOT caught by the anchor — the tamper-evidence the anchor adds over "+
			"VerifyChain is absent: got %v", err)
	}
}

func TestLedgerAnchor_DetectsHeadRewrite(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()

	rows := seedAnchorChain(ctx, t, p, 6)
	cleanup := clearLedgerAndAnchors(ctx, p)
	defer cleanup()
	if _, err := p.Exec(ctx, `DELETE FROM ledger_anchor`); err != nil {
		t.Fatalf("clear ledger_anchor: %v", err)
	}

	ls := NewLedgerStore(p)
	anchors := NewAnchorStore(p)

	hs, err := ls.Head(ctx, audit.DefaultAnchorWindow)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if err := anchors.Record(ctx, audit.DomainGovernanceLedger, audit.ComputeAnchor(hs)); err != nil {
		t.Fatalf("record anchor: %v", err)
	}

	// EXECUTE A CONTENT TAMPER, correctly re-linked so VerifyChain stays green: rewrite the HEAD row (seq 6)
	// with new content and its recomputed hash (prev = row 5's hash, unchanged), then UPDATE both columns.
	relink := audit.NewLedgerFromTail(5, rows[4].Hash)
	forged, err := relink.Append(audit.GovDecision{Decision: "AUTO", Reason: "FORGED", ActionID: rows[5].ActionID})
	if err != nil {
		t.Fatalf("build re-linked row: %v", err)
	}
	tag, err := p.Exec(ctx, `UPDATE governance_ledger SET reason=$1, hash=$2 WHERE seq=$3`, forged.Reason, forged.Hash, int64(6))
	if err != nil {
		t.Fatalf("tamper UPDATE: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("tamper UPDATE affected %d rows, want 1", tag.RowsAffected())
	}

	after, err := ls.All(ctx)
	if err != nil {
		t.Fatalf("all post-tamper: %v", err)
	}
	stored, err := anchors.Anchors(ctx, audit.DomainGovernanceLedger)
	if err != nil {
		t.Fatalf("read anchors: %v", err)
	}

	// Control: the re-linked chain is internally consistent, so VerifyChain accepts the forgery.
	if err := audit.VerifyChain(after); err != nil {
		t.Fatalf("precondition failed: a re-linked HEAD should still pass VerifyChain, got %v", err)
	}
	// Catch: the anchored hash at seq 6 no longer matches.
	if err := audit.VerifyAgainstAnchors(audit.RowRefsOf(after), stored); !errors.Is(err, audit.ErrAnchorMismatch) {
		t.Fatalf("a rewritten HEAD row was NOT caught by the anchor: got %v", err)
	}
}

// TestLedgerAnchor_HeadEmptyAndForwardAppend exercises the Head reader's boundaries: an empty ledger reads
// Seq 0 (nothing to witness), and after a forward append an earlier anchor still verifies clean.
func TestLedgerAnchor_HeadEmptyAndForwardAppend(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()

	cleanup := clearLedgerAndAnchors(ctx, p)
	defer cleanup()

	ls := NewLedgerStore(p)
	anchors := NewAnchorStore(p)

	// Empty ledger: Head reads Seq 0.
	if _, err := p.Exec(ctx, `DELETE FROM governance_ledger`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if hs, err := ls.Head(ctx, audit.DefaultAnchorWindow); err != nil || hs.Seq != 0 {
		t.Fatalf("Head(empty) = (seq %d, err %v), want (0, nil)", hs.Seq, err)
	}

	// Seed 4, anchor the HEAD, then append 3 more continuing the same chain.
	rows := seedAnchorChain(ctx, t, p, 4)
	if _, err := p.Exec(ctx, `DELETE FROM ledger_anchor`); err != nil {
		t.Fatalf("clear ledger_anchor: %v", err)
	}
	hs, err := ls.Head(ctx, audit.DefaultAnchorWindow)
	if err != nil {
		t.Fatalf("head@4: %v", err)
	}
	if err := anchors.Record(ctx, audit.DomainGovernanceLedger, audit.ComputeAnchor(hs)); err != nil {
		t.Fatalf("record: %v", err)
	}

	cont := audit.NewLedgerFromTail(rows[3].Seq, rows[3].Hash)
	for i := 5; i <= 7; i++ {
		e, err := cont.Append(audit.GovDecision{Decision: "AUTO", Reason: fmt.Sprintf("seed-%d", i), ActionID: fmt.Sprintf("anchor-seed-%d", i)})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if _, err := p.Exec(ctx, `
			INSERT INTO governance_ledger (seq, decision, reason, action_id, withheld, prev_hash, hash)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			e.Seq, e.Decision, e.Reason, e.ActionID, e.Withheld, e.PrevHash, e.Hash); err != nil {
			t.Fatalf("append insert %d: %v", i, err)
		}
	}

	after, err := ls.All(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	stored, err := anchors.Anchors(ctx, audit.DomainGovernanceLedger)
	if err != nil {
		t.Fatalf("anchors: %v", err)
	}
	if head, err := ls.Head(ctx, audit.DefaultAnchorWindow); err != nil || head.Seq != 7 {
		t.Fatalf("Head after append = (seq %d, err %v), want (7, nil)", head.Seq, err)
	}
	if err := audit.VerifyAgainstAnchors(audit.RowRefsOf(after), stored); err != nil {
		t.Fatalf("a clean forward append (HEAD 4 -> 7) was misread as tamper: %v", err)
	}
}

// TestAnchorStore_DomainIsolation is the oracle for TG-515's whole purpose: witnesses recorded under one
// domain are NEVER returned for another. It records TWO domains into the SAME table and asserts each read sees
// only its own — with a single domain a dropped `WHERE domain = $1` is invisible (filtered == unfiltered), so
// this two-domain test is what actually PINS that clause: drop it and the seq assertions redden (each read
// would return both rows). It also proves the domain-separation the peer's TG-510 corpus consumer relies on.
func TestAnchorStore_DomainIsolation(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()
	if _, err := p.Exec(ctx, `DELETE FROM ledger_anchor`); err != nil {
		t.Fatalf("clear ledger_anchor: %v", err)
	}
	store := NewAnchorStore(p)
	gov := store.Scoped(audit.DomainGovernanceLedger)
	corpus := store.Scoped("knowledge-corpus")

	// Distinct witnesses in the SAME table under two domains (digests are opaque content commitments here).
	if err := gov.Record(ctx, audit.Anchor{Seq: 7, Hash: "gov-hash-7", WindowSize: 3, Digest: "gov-digest"}); err != nil {
		t.Fatalf("record gov: %v", err)
	}
	if err := corpus.Record(ctx, audit.Anchor{Seq: 42, Hash: "corpus-hash-42", WindowSize: 5, Digest: "corpus-digest"}); err != nil {
		t.Fatalf("record corpus: %v", err)
	}

	g, err := gov.Anchors(ctx)
	if err != nil {
		t.Fatalf("read gov: %v", err)
	}
	if len(g) != 1 || g[0].Seq != 7 || g[0].Digest != "gov-digest" {
		t.Fatalf("governance-ledger domain read the wrong set: %+v (want ONLY the seq-7 gov anchor — a leak means the WHERE domain filter is broken)", g)
	}
	c, err := corpus.Anchors(ctx)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	if len(c) != 1 || c[0].Seq != 42 || c[0].Digest != "corpus-digest" {
		t.Fatalf("knowledge-corpus domain read the wrong set: %+v (want ONLY the seq-42 corpus anchor)", c)
	}
	// A domain that recorded nothing reads empty — never a leak from a populated sibling.
	empty, err := store.Anchors(ctx, "never-recorded")
	if err != nil {
		t.Fatalf("read empty domain: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("an unrecorded domain leaked %d rows from its siblings — the WHERE domain filter is broken", len(empty))
	}
}

// TestAnchorStore_AnchorsErrorsOnDBFailure pins the invariant the TG-516 no-witnesses short-circuit trusts:
// Anchors MUST surface a DB failure as an ERROR, never as a silent empty slice. VerifyJob.Run short-circuits
// to (nil, true) "clean" when Anchors returns no witnesses; if a future refactor swallowed the query error and
// returned (nil, nil), a ledger/DB outage would masquerade as "no witnesses recorded" and the verifier would
// report the AUDIT SPINE CLEAN during the outage — the "sealed-store, wrong-half-broken" defect class (TG-518).
// A closed pool models the DB becoming unreachable.
func TestAnchorStore_AnchorsErrorsOnDBFailure(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store := NewAnchorStore(p)
	p.Close() // the DB is now unreachable to this store

	got, aerr := store.Anchors(ctx, audit.DomainGovernanceLedger)
	if aerr == nil {
		t.Fatalf("Anchors over a closed pool returned (%v, nil) — a DB failure MUST surface as an error, NEVER a "+
			"silent empty slice, or a ledger outage reads as 'no witnesses' and the verifier calls a dead spine clean", got)
	}
	if got != nil {
		t.Errorf("on error Anchors must return a nil slice, got %v", got)
	}
}
