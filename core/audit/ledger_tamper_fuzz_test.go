package audit

import (
	"errors"
	"github.com/territory-grounder/grounder/core/fuzzcorpus"
	"testing"
	"time"
)

// applyLedgerMutation alters EXACTLY ONE field of e per fieldSel and reports the field name, whether that
// field is COVERED by the hash chain (fed into entryHash and re-walked by VerifyChain), and whether the
// value actually changed. {Seq, Decision, Reason, ActionID, Withheld, PrevHash, Hash} are covered;
// CreatedAt deliberately is NOT (ledger.go:46-58 — it is a read projection of Postgres's insert clock,
// outside the SHA-256 chain). The tamper-evidence invariant the fuzz asserts: a covered field that truly
// changed MUST break VerifyChain; a non-covered change or a no-op MUST NOT.
func applyLedgerMutation(e *LedgerEntry, fieldSel uint8, newVal string) (field string, covered, changed bool) {
	switch fieldSel % 8 {
	case 0:
		old := e.Seq
		e.Seq = e.Seq + 1 + int64(len(newVal)) // always a different, still-valid int64 seq
		return "Seq", true, e.Seq != old
	case 1:
		old := e.Decision
		e.Decision = newVal
		return "Decision", true, e.Decision != old
	case 2:
		old := e.Reason
		e.Reason = newVal
		return "Reason", true, e.Reason != old
	case 3:
		old := e.ActionID
		e.ActionID = newVal
		return "ActionID", true, e.ActionID != old
	case 4:
		old := e.Withheld
		e.Withheld = !e.Withheld // a flip always changes the value
		return "Withheld", true, e.Withheld != old
	case 5:
		old := e.PrevHash
		e.PrevHash = newVal
		return "PrevHash", true, e.PrevHash != old
	case 6:
		old := e.Hash
		e.Hash = newVal
		return "Hash", true, e.Hash != old
	default: // case 7
		old := e.CreatedAt
		e.CreatedAt = e.CreatedAt.Add(time.Hour) // NOT covered by the chain
		return "CreatedAt", false, !e.CreatedAt.Equal(old)
	}
}

// FuzzLedgerTamper drives the governance ledger's TAMPER-EVIDENCE invariant (INV-19, TG-5 Phase 4) over
// arbitrary content: build an honest hash-chained ledger, then mutate one field of one row and require
// VerifyChain to react correctly. audit_test.go pins tampering on a couple of hand-built rows; this
// generalizes to any decision content and every field position, because tamper-evidence is a claim about
// EVERY row and EVERY covered field, not the two an author happened to write down. The three outcomes:
//
//   - a covered field actually changed  → VerifyChain MUST return ErrChainBroken (the whole point: no edit
//     to a chained field can pass unseen; an un-sentineled or absent error is a silent ledger forgery).
//   - only CreatedAt changed            → VerifyChain MUST still pass (it is deliberately outside the chain;
//     a surface must not read the insert clock as a cryptographic guarantee — ledger.go:46-58).
//   - a no-op write (same value)        → VerifyChain MUST still pass (the bytes are identical).
//
// The fuzzer fails only if a real tamper slips through, if a tamper is reported as something other than
// ErrChainBroken, or if a non-covered / no-op change is misread as tampering. Runs the seed corpus in CI;
// drives wide with `go test -fuzz=FuzzLedgerTamper ./core/audit`.
func FuzzLedgerTamper(f *testing.F) {
	// seeds: row0 (d/r/a/w), row1 (d/r/a/w), rowSel, fieldSel, newVal
	f.Add("gate:deny", "risk:high", "act-1", true, "classify:AUTO", "low", "act-2", false, uint8(0), uint8(1), "FORGED")
	f.Add("verdict:ok", "", "a", false, "gate:allow", "", "b", true, uint8(1), uint8(6), "deadbeefdeadbeef") // tamper the Hash of row 1
	f.Add("x", "y", "z", false, "p", "q", "r", false, uint8(0), uint8(7), "")                                // CreatedAt — must NOT be read as tamper
	f.Add("d", "", "id", true, "d2", "", "id2", false, uint8(1), uint8(4), "")                               // flip Withheld on row 1
	f.Add("", "", "", false, "", "", "", false, uint8(0), uint8(0), "")                                      // all-empty (coerced); mutate Seq
	f.Add("classify:AUTO", "reason", "same", false, "gate:deny", "r2", "same", true, uint8(1), uint8(5), "") // PrevHash of row 1

	for _, h := range fuzzcorpus.Strings() {
		f.Add(h, h, h, false, "gate:allow", "", "act", true, uint8(0), uint8(0), h) // the shared §3.2 battery on the ledger text fields
	}
	f.Fuzz(func(t *testing.T, d0, r0, a0 string, w0 bool, d1, r1, a1 string, w1 bool, rowSel, fieldSel uint8, newVal string) {
		// Append fails closed on an empty Decision or ActionID (ErrIncompleteDecision), so coerce ONLY those
		// two required fields — the fuzzer still explores all content, and always reaches the tamper path
		// with a valid honest chain to attack.
		coerce := func(s, dflt string) string {
			if s == "" {
				return dflt
			}
			return s
		}
		l := NewLedger()
		rows := []GovDecision{
			{Decision: coerce(d0, "d0"), Reason: r0, ActionID: coerce(a0, "a0"), Withheld: w0},
			{Decision: coerce(d1, "d1"), Reason: r1, ActionID: coerce(a1, "a1"), Withheld: w1},
		}
		for i, d := range rows {
			if _, err := l.Append(d); err != nil {
				t.Fatalf("honest Append of row %d must succeed: %v", i, err)
			}
		}
		entries := l.Entries()

		// baseline: an untampered chain always verifies. If this fails the rest is meaningless.
		if err := VerifyChain(entries); err != nil {
			t.Fatalf("honest chain must verify but did not: %v\n%+v", err, entries)
		}

		// tamper a COPY so the honest chain stays intact for the comparison.
		m := append([]LedgerEntry(nil), entries...)
		i := int(rowSel) % len(m)
		field, covered, changed := applyLedgerMutation(&m[i], fieldSel, newVal)
		err := VerifyChain(m)

		switch {
		case covered && changed:
			if err == nil {
				t.Fatalf("TAMPER NOT DETECTED (INV-19 broken): row %d field %s := %q left the chain verifying\n%+v", i, field, newVal, m)
			}
			if !errors.Is(err, ErrChainBroken) {
				t.Fatalf("tamper detected but not classified as ErrChainBroken (caller cannot branch): row %d field %s: %v", i, field, err)
			}
		case !covered:
			if err != nil {
				t.Fatalf("non-covered field %s must not affect VerifyChain, got: %v", field, err)
			}
		default: // covered && !changed — a no-op write left the bytes identical
			if err != nil {
				t.Fatalf("no-op mutation of %s must leave the chain intact, got: %v", field, err)
			}
		}
	})
}

// TestLedgerReplayReorderDetected pins the REPLAY / REORDER half of tamper-evidence with deterministic
// structural attacks, and — just as importantly — the ONE tamper VerifyChain provably CANNOT see on its
// own. Reordering, replaying (duplicating), and mid-deleting a row each break the monotonic-seq / prev-hash
// walk and are rejected. A truncated TAIL leaves seq 1..k internally consistent, so VerifyChain passes; that
// is precisely the blind spot the ledger-HEAD anchor exists to close (anchor.go: VerifyAgainstAnchors /
// ErrAnchorMismatch, asserted in anchor_test.go). Asserting the pass here keeps the boundary between the two
// controls honest rather than papering over it.
func TestLedgerReplayReorderDetected(t *testing.T) {
	build := func() []LedgerEntry {
		l := NewLedger()
		for _, d := range []GovDecision{
			{Decision: "classify:AUTO", Reason: "low", ActionID: "a1"},
			{Decision: "gate:deny", Reason: "risk:high", ActionID: "a2", Withheld: true},
			{Decision: "verdict:ok", Reason: "clean", ActionID: "a3"},
		} {
			if _, err := l.Append(d); err != nil {
				t.Fatalf("setup Append: %v", err)
			}
		}
		return l.Entries()
	}

	if err := VerifyChain(build()); err != nil {
		t.Fatalf("honest 3-row chain must verify: %v", err)
	}

	// REORDER: swap rows 0 and 1 → seq is no longer monotonic at index 0.
	swapped := build()
	swapped[0], swapped[1] = swapped[1], swapped[0]
	if err := VerifyChain(swapped); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("a reordered chain must be rejected, got: %v", err)
	}

	// REPLAY: duplicate row 1 → the copy lands at a seq the walk does not expect.
	dup := build()
	dup = append(dup[:2:2], dup[1], dup[2]) // row0, row1, row1, row2
	if err := VerifyChain(dup); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("a duplicated/replayed row must be rejected, got: %v", err)
	}

	// MID-DELETION: drop the middle row → a seq gap at index 1.
	gapped := build()
	gapped = append(gapped[:1:1], gapped[2]) // row0, row2
	if err := VerifyChain(gapped); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("a mid-chain deletion must be rejected, got: %v", err)
	}

	// TRUNCATED TAIL — the anchor's blind spot: seq 1..2 stays internally consistent, so VerifyChain PASSES.
	// The ledger-HEAD anchor is the control that catches this (anchor_test.go); VerifyChain alone cannot.
	truncated := build()[:2]
	if err := VerifyChain(truncated); err != nil {
		t.Fatalf("VerifyChain is NOT expected to see a truncated tail (that is the anchor's job); got: %v", err)
	}
}
