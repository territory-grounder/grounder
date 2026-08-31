package ledgeranchor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

type fakeLedger struct {
	entries []audit.LedgerEntry
	err     error
}

func (f fakeLedger) All(context.Context) ([]audit.LedgerEntry, error) { return f.entries, f.err }

type fakeAnchors struct {
	anchors []audit.Anchor
	err     error
}

func (f fakeAnchors) Anchors(context.Context) ([]audit.Anchor, error) { return f.anchors, f.err }

func chainOf(seqs ...int64) []audit.LedgerEntry {
	out := make([]audit.LedgerEntry, len(seqs))
	for i, s := range seqs {
		out[i] = audit.LedgerEntry{Seq: s, Hash: fmt.Sprintf("h%d", s)}
	}
	return out
}

// cleanAnchorOver builds the witness VerifyAgainstAnchors would accept for a full 3-row chain — the same
// HeadState the verifier rebuilds from the chain (HEAD at seq 3, the trailing rows in ascending seq).
func cleanAnchorOver3() audit.Anchor {
	return audit.ComputeAnchor(audit.HeadState{
		Seq: 3, Hash: "h3",
		Recent: []audit.RowRef{{Seq: 1, Hash: "h1"}, {Seq: 2, Hash: "h2"}, {Seq: 3, Hash: "h3"}},
	})
}

// raceStore models the boot read-snapshot race (TG-516) as a coupled LedgerReader + AnchorReader: All()
// returns `short` (a chain snapshot one row BEHIND the anchor) UNTIL Anchors() has been read, then `full`.
// That reproduces "the verify snapshotted the chain before the in-flight row was visible, while the anchor for
// that row was already committed." With the OLD chain-first order All() runs first -> short (false tamper);
// with the anchors-first fix All() runs AFTER Anchors() -> full (the race self-heals). For the real-truncation
// case, full == short so the anchored row NEVER appears, however late it is read.
type raceStore struct {
	short, full []audit.LedgerEntry
	anchor      audit.Anchor
	anchorsRead bool
}

func (r *raceStore) Anchors(context.Context) ([]audit.Anchor, error) {
	r.anchorsRead = true
	return []audit.Anchor{r.anchor}, nil
}

func (r *raceStore) All(context.Context) ([]audit.LedgerEntry, error) {
	if r.anchorsRead {
		return r.full, nil
	}
	return r.short, nil
}

// TestVerifyJob_Run_BootReadRace is the two-sided killing-oracle for TG-516: (b) anchors-first self-heals the
// boot read-race (no false tamper), while (c) a REAL truncation (the anchored row absent AND staying absent)
// still reddens — so the reorder fixes the cry-wolf without ever swallowing a genuine truncation.
func TestVerifyJob_Run_BootReadRace(t *testing.T) {
	ctx := context.Background()
	anchor := audit.ComputeAnchor(audit.HeadState{
		Seq: 4, Hash: "h4",
		Recent: []audit.RowRef{{Seq: 1, Hash: "h1"}, {Seq: 2, Hash: "h2"}, {Seq: 3, Hash: "h3"}, {Seq: 4, Hash: "h4"}},
	})
	short := chainOf(1, 2, 3)    // the racy stale snapshot — one row behind the anchored HEAD (4)
	full := chainOf(1, 2, 3, 4)  // the chain the instant it is read AFTER the anchor

	// (a) PRECONDITION — the bug the reorder fixes: the stale snapshot vs the ahead anchor genuinely looks like
	// a truncation to the pure checker. This is exactly what the OLD chain-first order fed it at boot.
	if err := audit.VerifyAgainstAnchors(audit.RowRefsOf(short), []audit.Anchor{anchor}); !errors.Is(err, audit.ErrAnchorMismatch) {
		t.Fatalf("precondition: a chain snapshot behind an ahead anchor must look like a tamper to VerifyAgainstAnchors: got %v", err)
	}

	// (b) THE FIX — anchors-first: Run reads the anchor first, so the chain read comes AFTER and returns `full`.
	// No false tamper on a perfectly intact spine.
	race := &raceStore{short: short, full: full, anchor: anchor}
	if err, ok := (VerifyJob{Ledger: race, Anchors: race}).Run(ctx); err != nil || !ok {
		t.Fatalf("anchors-first must self-heal the boot read-race: got err=%v ok=%v (want nil,true)", err, ok)
	}

	// (c) THE GUARD — a REAL truncation must STILL alarm: row 4 is absent and STAYS absent (full == short), so
	// no read order can make it appear. The fix must not silently swallow a genuine truncation.
	trunc := &raceStore{short: short, full: short, anchor: anchor}
	if err, ok := (VerifyJob{Ledger: trunc, Anchors: trunc}).Run(ctx); !ok || !errors.Is(err, audit.ErrAnchorMismatch) {
		t.Fatalf("a REAL truncation (anchored seq 4 absent on every read) must still be caught: got err=%v ok=%v (want ErrAnchorMismatch,true)", err, ok)
	}
}

// TestVerifyJob_Run is the contract of the CONSUMING half (TG-509): read both stores, run the anchor check, and
// distinguish (a) a clean/intact chain, (b) a detected TAMPER, and (c) a read that could not run — the last
// surfaced as ok=false, never a silent pass.
func TestVerifyJob_Run(t *testing.T) {
	ctx := context.Background()
	chain := chainOf(1, 2, 3)

	// (a) clean: a matching witness over the intact chain.
	if err, ok := (VerifyJob{Ledger: fakeLedger{entries: chain}, Anchors: fakeAnchors{anchors: []audit.Anchor{cleanAnchorOver3()}}}).Run(ctx); err != nil || !ok {
		t.Fatalf("intact chain misread: err=%v ok=%v (want nil,true)", err, ok)
	}

	// (b) TAMPER: a witness at seq 5 while the chain stops at seq 3 — the rows it covered were truncated/rolled
	// back (the exact tamper VerifyChain cannot see). ok=true (the verification RAN), err=the contradiction.
	tamper := audit.Anchor{Seq: 5, Hash: "h5", WindowSize: 1, Digest: "irrelevant"}
	if err, ok := (VerifyJob{Ledger: fakeLedger{entries: chain}, Anchors: fakeAnchors{anchors: []audit.Anchor{tamper}}}).Run(ctx); !ok || !errors.Is(err, audit.ErrAnchorMismatch) {
		t.Fatalf("truncation tamper not detected: err=%v ok=%v (want ErrAnchorMismatch,true)", err, ok)
	}

	// (c) a read that FAILED must be (err, ok=false) — an unverifiable spine is not a clean one. Since TG-516
	// reads ANCHORS first, a chain-read failure is surfaced only when there ARE witnesses to check against, so
	// this case supplies a witness so the chain read is actually reached.
	if err, ok := (VerifyJob{Ledger: fakeLedger{err: errors.New("db down")}, Anchors: fakeAnchors{anchors: []audit.Anchor{cleanAnchorOver3()}}}).Run(ctx); err == nil || ok {
		t.Fatalf("ledger read failure (with a witness present) must be (err, ok=false): err=%v ok=%v", err, ok)
	}
	// An anchor-read failure is ALWAYS surfaced — anchors are read first.
	if err, ok := (VerifyJob{Ledger: fakeLedger{entries: chain}, Anchors: fakeAnchors{err: errors.New("db down")}}).Run(ctx); err == nil || ok {
		t.Fatalf("anchor read failure must be (err, ok=false): err=%v ok=%v", err, ok)
	}

	// no witnesses yet (fresh spine) — nothing to contradict, honestly not a tamper.
	if err, ok := (VerifyJob{Ledger: fakeLedger{entries: chain}, Anchors: fakeAnchors{anchors: nil}}).Run(ctx); err != nil || !ok {
		t.Fatalf("a spine with no witnesses must be clean: err=%v ok=%v", err, ok)
	}
	// TG-516 consequence of anchors-first: with NO witnesses, Run returns (nil, true) WITHOUT reading the
	// chain — nothing to verify, so a would-be chain read (even a failing one) is correctly never attempted.
	if err, ok := (VerifyJob{Ledger: fakeLedger{err: errors.New("db down")}, Anchors: fakeAnchors{}}).Run(ctx); err != nil || !ok {
		t.Fatalf("no witnesses must short-circuit to (nil, true) before any chain read: err=%v ok=%v", err, ok)
	}
}

// TestRunVerifyPeriodically_RoutesTamperVsReadGap: the immediate pass routes a DETECTED tamper to onTamper (the
// critical operator signal) and a read gap to onErr (a control gap, not a tamper) — never the wrong callback,
// and never both. Uses an already-cancelled ctx so the loop runs exactly the immediate pass and returns.
func TestRunVerifyPeriodically_RoutesTamperVsReadGap(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	var tampers, gaps int
	// tamper: witness at seq 5, chain stops at 3.
	tj := VerifyJob{Ledger: fakeLedger{entries: chainOf(1, 2, 3)}, Anchors: fakeAnchors{anchors: []audit.Anchor{{Seq: 5, Hash: "h5", WindowSize: 1}}}}
	RunVerifyPeriodically(cancelled, tj, time.Hour, func(error) { tampers++ }, func(error) { gaps++ })
	if tampers != 1 || gaps != 0 {
		t.Fatalf("a detected tamper must route to onTamper only: tampers=%d gaps=%d", tampers, gaps)
	}

	tampers, gaps = 0, 0
	// A witness IS present (TG-516 anchors-first: an empty set short-circuits to clean before the chain read),
	// so the failing chain read is reached and routes to onErr.
	gj := VerifyJob{Ledger: fakeLedger{err: errors.New("db down")}, Anchors: fakeAnchors{anchors: []audit.Anchor{{Seq: 1, Hash: "h1"}}}}
	RunVerifyPeriodically(cancelled, gj, time.Hour, func(error) { tampers++ }, func(error) { gaps++ })
	if gaps != 1 || tampers != 0 {
		t.Fatalf("a read gap must route to onErr only: tampers=%d gaps=%d", tampers, gaps)
	}
}
