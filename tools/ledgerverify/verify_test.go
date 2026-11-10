package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

func chainOf(t *testing.T, n int) []audit.LedgerEntry {
	t.Helper()
	l := audit.NewLedger()
	for i := 1; i <= n; i++ {
		if _, err := l.Append(audit.GovDecision{Decision: fmt.Sprintf("d%d", i), Reason: "r", ActionID: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	return l.Entries()
}

func anchorAt(e []audit.LedgerEntry, seq int) audit.Anchor {
	refs := audit.RowRefsOf(e[:seq])
	start := seq - 3
	if start < 0 {
		start = 0
	}
	return audit.ComputeAnchor(audit.HeadState{Seq: e[seq-1].Seq, Hash: e[seq-1].Hash, Recent: refs[start:]})
}

func TestAnIntactChainWithAnchorsPasses(t *testing.T) {
	e := chainOf(t, 12)
	r := Verify(e, []audit.Anchor{anchorAt(e, 6), anchorAt(e, 12)}, time.Now())
	if !r.ChainOK || !r.AnchorsOK || r.Rows != 12 || r.MaxSeq != 12 || r.HeadHash == "" {
		t.Fatalf("got %+v", r)
	}
}

func TestEveryTamperShapeIsCaught(t *testing.T) {
	cases := []struct {
		name      string
		tamper    func(e []audit.LedgerEntry) []audit.LedgerEntry
		wantChain bool
	}{
		{"an edited decision", func(e []audit.LedgerEntry) []audit.LedgerEntry { e[4].Decision = "forged"; return e }, false},
		{"an edited reason", func(e []audit.LedgerEntry) []audit.LedgerEntry { e[4].Reason = "forged"; return e }, false},
		{"a flipped withheld flag", func(e []audit.LedgerEntry) []audit.LedgerEntry { e[4].Withheld = !e[4].Withheld; return e }, false},
		{"a missing row", func(e []audit.LedgerEntry) []audit.LedgerEntry { return append(e[:4:4], e[5:]...) }, false},
		{"a duplicated row", func(e []audit.LedgerEntry) []audit.LedgerEntry {
			return append(append(e[:5:5], e[4]), e[5:]...)
		}, false},
		{"two rows swapped", func(e []audit.LedgerEntry) []audit.LedgerEntry { e[3], e[4] = e[4], e[3]; return e }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := chainOf(t, 12)
			a := []audit.Anchor{anchorAt(e, 12)}
			r := Verify(tc.tamper(e), a, time.Now())
			if r.ChainOK != tc.wantChain || r.ChainError == "" {
				t.Fatalf("got %+v", r)
			}
		})
	}
}

func TestASelfConsistentRewriteIsCaughtOnlyByTheAnchors(t *testing.T) {
	e := chainOf(t, 12)
	anchors := []audit.Anchor{anchorAt(e, 12)}
	forged := audit.NewLedger()
	for i, row := range e {
		d := row.Decision
		if i == 4 {
			d = "forged"
		}
		if _, err := forged.Append(audit.GovDecision{Decision: d, Reason: row.Reason, ActionID: row.ActionID}); err != nil {
			t.Fatal(err)
		}
	}
	r := Verify(forged.Entries(), anchors, time.Now())
	if !r.ChainOK {
		t.Fatalf("a rewritten chain is internally consistent, got %+v", r)
	}
	if r.AnchorsOK || r.AnchorError == "" {
		t.Fatalf("the anchors must contradict it, got %+v", r)
	}
}

func TestARollbackBehindARecordedAnchorIsCaught(t *testing.T) {
	e := chainOf(t, 12)
	anchors := []audit.Anchor{anchorAt(e, 12)}
	r := Verify(e[:8], anchors, time.Now())
	if !r.ChainOK {
		t.Fatalf("a truncated prefix still hash-verifies, got %+v", r)
	}
	if r.AnchorsOK || !strings.Contains(r.AnchorError, "truncated") {
		t.Fatalf("the anchor must report the truncation, got %+v", r)
	}
}

func TestAbsenceIsNeverClean(t *testing.T) {
	if r := Verify(nil, nil, time.Now()); r.ChainOK || r.AnchorsOK || r.ChainError == "" {
		t.Fatalf("an empty ledger verified nothing, got %+v", r)
	}
	e := chainOf(t, 3)
	if r := Verify(e, nil, time.Now()); r.AnchorsOK || r.AnchorError == "" {
		t.Fatalf("no anchors means a rollback cannot be seen, got %+v", r)
	}
}
