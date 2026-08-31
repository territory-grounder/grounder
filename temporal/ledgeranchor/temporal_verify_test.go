package ledgeranchor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

// fakeTemporalReader is the in-memory TemporalAnchorReader oracle: a map keyed by (domain, seq), so a test
// can witness exactly the seqs it wants and leave the rest genuinely absent (never witnessed / erased /
// aged out of retention) — the three cases TemporalVerifyJob cannot and must not distinguish (see
// ErrTemporalWitnessUnavailable's doc).
type fakeTemporalReader struct {
	witnessed map[string]audit.Anchor
	err       error
}

func newFakeTemporalReader() *fakeTemporalReader {
	return &fakeTemporalReader{witnessed: map[string]audit.Anchor{}}
}

func (f *fakeTemporalReader) witness(domain string, a audit.Anchor) {
	f.witnessed[WitnessWorkflowID(domain, a.Seq)] = a
}

func (f *fakeTemporalReader) ReadWitness(_ context.Context, domain string, seq int64) (audit.Anchor, bool, error) {
	if f.err != nil {
		return audit.Anchor{}, false, f.err
	}
	a, ok := f.witnessed[WitnessWorkflowID(domain, seq)]
	return a, ok, nil
}

// dbAnchorFor builds the DB-recorded audit.Anchor for a chain prefix exactly as core/db.LedgerStore.Head +
// core/audit.ComputeAnchor would (HEAD at `seq`, the whole chain as the trailing window) — a real anchor over
// a real hash-chain, not a hand-typed hash string, so the tests exercise the actual digest math.
func dbAnchorFor(chain []audit.LedgerEntry, seq int64) audit.Anchor {
	var recent []audit.RowRef
	for _, e := range chain {
		if e.Seq <= seq {
			recent = append(recent, audit.RowRef{Seq: e.Seq, Hash: e.Hash})
		}
	}
	head := recent[len(recent)-1]
	return audit.ComputeAnchor(audit.HeadState{Seq: head.Seq, Hash: head.Hash, Recent: recent})
}

// TestTemporalVerifyJob_Run_CleanWhenAllThreeAgree is the base case: the DB-recorded anchor, the
// Temporal-witnessed anchor, and the live chain all agree — a clean pass.
func TestTemporalVerifyJob_Run_CleanWhenAllThreeAgree(t *testing.T) {
	chain := chainOf(1, 2, 3, 4)
	dbA := dbAnchorFor(chain, 4)
	temp := newFakeTemporalReader()
	temp.witness("governance-ledger", dbA)

	j := TemporalVerifyJob{
		Anchors:  fakeAnchors{anchors: []audit.Anchor{dbA}},
		Ledger:   fakeLedger{entries: chain},
		Temporal: temp,
		Domain:   "governance-ledger",
	}
	if err, ok := j.Run(context.Background()); err != nil || !ok {
		t.Fatalf("three agreeing witnesses must be clean: err=%v ok=%v (want nil,true)", err, ok)
	}
}

// TestTemporalVerifyJob_Run_DetectsDBLedgerDivergence is the killing oracle this whole file exists for: an
// actor rewrites BOTH governance_ledger and ledger_anchor consistently within TG's ONE Postgres instance (the
// exact residual VerifyJob's DB-vs-DB comparison cannot see — a self-consistent forged pair). The
// Temporal-side witness, recorded under a DIFFERENT credential in a DIFFERENT database before the tamper,
// still shows the OLD, true HEAD hash — so it now disagrees with the (also-tampered) DB anchor.
func TestTemporalVerifyJob_Run_DetectsDBLedgerDivergence(t *testing.T) {
	chain := chainOf(1, 2, 3, 4)
	genuineAnchor := dbAnchorFor(chain, 4)

	// The Temporal witness recorded the GENUINE HEAD before any tamper.
	temp := newFakeTemporalReader()
	temp.witness("governance-ledger", genuineAnchor)

	// The DB-recorded anchor (and, in a real attack, governance_ledger itself) has since been REWRITTEN to a
	// forged, internally-consistent hash the attacker computed to cover their tracks — VerifyChain and
	// VerifyJob (DB-vs-DB) would both accept this, because both halves of that comparison were rewritten
	// together. Only the Temporal-side witness — never touched — disagrees.
	forged := genuineAnchor
	forged.Hash = "deadbeef00000000"
	forged.Digest = "forgeddigest0000"

	j := TemporalVerifyJob{
		Anchors:  fakeAnchors{anchors: []audit.Anchor{forged}},
		Ledger:   fakeLedger{entries: chain}, // the live chain the attacker also rewrote to match `forged`
		Temporal: temp,
		Domain:   "governance-ledger",
	}
	err, ok := j.Run(context.Background())
	if !ok || !errors.Is(err, ErrTemporalWitnessMismatch) {
		t.Fatalf("a DB-side forged anchor disagreeing with the Temporal witness must be caught: err=%v ok=%v (want ErrTemporalWitnessMismatch,true)", err, ok)
	}
}

// TestTemporalVerifyJob_Run_DetectsChainRegressedBelowTemporalWitness: the DB anchor and the Temporal witness
// still AGREE with each other (an attacker who did not know about, or could not reach, the Temporal side), but
// the LIVE chain has since regressed below what both witnesses recorded — a tail truncation. Proves the
// cross-check also catches tamper of the chain alone, not just DB-anchor forgery.
func TestTemporalVerifyJob_Run_DetectsChainRegressedBelowTemporalWitness(t *testing.T) {
	full := chainOf(1, 2, 3, 4)
	dbA := dbAnchorFor(full, 4)
	temp := newFakeTemporalReader()
	temp.witness("governance-ledger", dbA)

	truncated := full[:2] // rows 3,4 deleted — the live chain regressed below the jointly-witnessed HEAD

	j := TemporalVerifyJob{
		Anchors:  fakeAnchors{anchors: []audit.Anchor{dbA}},
		Ledger:   fakeLedger{entries: truncated},
		Temporal: temp,
		Domain:   "governance-ledger",
	}
	err, ok := j.Run(context.Background())
	if !ok || !errors.Is(err, ErrTemporalWitnessMismatch) {
		t.Fatalf("a live chain truncated below a jointly-witnessed HEAD must be caught: err=%v ok=%v (want ErrTemporalWitnessMismatch,true)", err, ok)
	}
}

// TestTemporalVerifyJob_Run_FailSafeWhenNoTemporalWitnessFound is the OTHER half of the fail-safe contract
// (task requirement #3): the DB has recorded anchors, but NOT ONE of the recently-checked ones has a matching
// Temporal witness (Temporal witnessing was never wired, its workflow was erased, or every anchor has aged out
// of namespace retention — indistinguishable from here). This MUST NOT read as a clean pass.
func TestTemporalVerifyJob_Run_FailSafeWhenNoTemporalWitnessFound(t *testing.T) {
	chain := chainOf(1, 2, 3)
	dbA := dbAnchorFor(chain, 3)

	j := TemporalVerifyJob{
		Anchors:  fakeAnchors{anchors: []audit.Anchor{dbA}},
		Ledger:   fakeLedger{entries: chain},
		Temporal: newFakeTemporalReader(), // witnesses NOTHING
		Domain:   "governance-ledger",
	}
	err, ok := j.Run(context.Background())
	if ok || !errors.Is(err, ErrTemporalWitnessUnavailable) {
		t.Fatalf("no Temporal witness found must fail SAFE, never clean: err=%v ok=%v (want ErrTemporalWitnessUnavailable,false)", err, ok)
	}
}

// TestTemporalVerifyJob_Run_FreshSpineIsClean: zero DB anchors recorded anywhere yet is honestly "nothing to
// contradict", not a tamper — mirrors VerifyJob.Run's identical convention for a brand-new spine.
func TestTemporalVerifyJob_Run_FreshSpineIsClean(t *testing.T) {
	j := TemporalVerifyJob{
		Anchors:  fakeAnchors{anchors: nil},
		Ledger:   fakeLedger{entries: chainOf(1, 2, 3)},
		Temporal: newFakeTemporalReader(),
		Domain:   "governance-ledger",
	}
	if err, ok := j.Run(context.Background()); err != nil || !ok {
		t.Fatalf("a fresh spine with no recorded anchors must be clean: err=%v ok=%v (want nil,true)", err, ok)
	}
}

// TestTemporalVerifyJob_Run_ReadFailuresSurfaceAsUnverifiable proves every store-read failure is (err,
// ok=false) — never silently absorbed into a pass — for each of the three sources this job reads.
func TestTemporalVerifyJob_Run_ReadFailuresSurfaceAsUnverifiable(t *testing.T) {
	chain := chainOf(1, 2, 3)
	dbA := dbAnchorFor(chain, 3)
	cleanTemporal := func() *fakeTemporalReader {
		f := newFakeTemporalReader()
		f.witness("governance-ledger", dbA)
		return f
	}

	t.Run("DB anchor read fails", func(t *testing.T) {
		j := TemporalVerifyJob{Anchors: fakeAnchors{err: errors.New("db down")}, Ledger: fakeLedger{entries: chain}, Temporal: cleanTemporal(), Domain: "governance-ledger"}
		if err, ok := j.Run(context.Background()); err == nil || ok {
			t.Fatalf("DB anchor read failure must be (err, ok=false): err=%v ok=%v", err, ok)
		}
	})
	t.Run("chain read fails", func(t *testing.T) {
		j := TemporalVerifyJob{Anchors: fakeAnchors{anchors: []audit.Anchor{dbA}}, Ledger: fakeLedger{err: errors.New("db down")}, Temporal: cleanTemporal(), Domain: "governance-ledger"}
		if err, ok := j.Run(context.Background()); err == nil || ok {
			t.Fatalf("chain read failure must be (err, ok=false): err=%v ok=%v", err, ok)
		}
	})
	t.Run("temporal read fails", func(t *testing.T) {
		j := TemporalVerifyJob{Anchors: fakeAnchors{anchors: []audit.Anchor{dbA}}, Ledger: fakeLedger{entries: chain}, Temporal: &fakeTemporalReader{err: errors.New("temporal unreachable")}, Domain: "governance-ledger"}
		if err, ok := j.Run(context.Background()); err == nil || ok {
			t.Fatalf("Temporal read failure must be (err, ok=false): err=%v ok=%v", err, ok)
		}
	})
}

// TestTemporalVerifyJob_Run_RecentBoundsTheCheck proves Recent bounds the pass to the last N DB-recorded
// anchors: a divergence OUTSIDE the window is not flagged (it was legitimately never checked this pass), one
// INSIDE it is. Also pins the DefaultTemporalVerifyRecent fallback for the zero-value Recent.
func TestTemporalVerifyJob_Run_RecentBoundsTheCheck(t *testing.T) {
	chain := chainOf(1, 2, 3, 4, 5)
	var anchors []audit.Anchor
	temp := newFakeTemporalReader()
	for _, seq := range []int64{1, 2, 3, 4, 5} {
		a := dbAnchorFor(chain, seq)
		anchors = append(anchors, a)
		temp.witness("governance-ledger", a)
	}
	// Corrupt the OLDEST recorded DB anchor only (seq 1) — outside a Recent=2 window, so it must not fire.
	anchors[0].Hash = "corrupted-old"

	j := TemporalVerifyJob{
		Anchors:  fakeAnchors{anchors: anchors},
		Ledger:   fakeLedger{entries: chain},
		Temporal: temp,
		Domain:   "governance-ledger",
		Recent:   2,
	}
	if err, ok := j.Run(context.Background()); err != nil || !ok {
		t.Fatalf("a divergence outside the Recent window must not fire this pass: err=%v ok=%v (want nil,true)", err, ok)
	}

	// The SAME corruption, now inside the window (Recent=5 covers it), must fire.
	j.Recent = 5
	err, ok := j.Run(context.Background())
	if !ok || !errors.Is(err, ErrTemporalWitnessMismatch) {
		t.Fatalf("the same divergence brought inside the window must be caught: err=%v ok=%v (want ErrTemporalWitnessMismatch,true)", err, ok)
	}
}

// TestRunTemporalVerifyPeriodically_RoutesTamperVsUnavailable mirrors TestRunVerifyPeriodically_RoutesTamperVsReadGap
// (verify.go's own test): the immediate pass routes a DETECTED divergence to onTamper and every "could not
// verify" outcome — including the fail-safe ErrTemporalWitnessUnavailable — to onErr, never the wrong one.
// Uses an already-cancelled ctx so the loop runs exactly the immediate pass and returns (no sleep/flakiness).
func TestRunTemporalVerifyPeriodically_RoutesTamperVsUnavailable(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	chain := chainOf(1, 2, 3)
	dbA := dbAnchorFor(chain, 3)

	var tampers, gaps int
	temp := newFakeTemporalReader()
	temp.witness("governance-ledger", dbA)
	forged := dbA
	forged.Hash = "forged"
	tj := TemporalVerifyJob{Anchors: fakeAnchors{anchors: []audit.Anchor{forged}}, Ledger: fakeLedger{entries: chain}, Temporal: temp, Domain: "governance-ledger"}
	RunTemporalVerifyPeriodically(cancelled, tj, time.Hour, func(error) { tampers++ }, func(error) { gaps++ })
	if tampers != 1 || gaps != 0 {
		t.Fatalf("a detected divergence must route to onTamper only: tampers=%d gaps=%d", tampers, gaps)
	}

	tampers, gaps = 0, 0
	uj := TemporalVerifyJob{Anchors: fakeAnchors{anchors: []audit.Anchor{dbA}}, Ledger: fakeLedger{entries: chain}, Temporal: newFakeTemporalReader(), Domain: "governance-ledger"}
	RunTemporalVerifyPeriodically(cancelled, uj, time.Hour, func(error) { tampers++ }, func(error) { gaps++ })
	if gaps != 1 || tampers != 0 {
		t.Fatalf("fail-safe unavailable must route to onErr only, never onTamper: tampers=%d gaps=%d", tampers, gaps)
	}
	if !errors.Is(fmt.Errorf("wrap: %w", ErrTemporalWitnessUnavailable), ErrTemporalWitnessUnavailable) {
		t.Fatal("sanity: ErrTemporalWitnessUnavailable must be errors.Is-wrappable for callers that want to distinguish it")
	}
}
