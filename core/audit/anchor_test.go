package audit

import (
	"errors"
	"fmt"
	"testing"
)

// buildChain appends n decisions to a fresh in-memory ledger and returns the persisted rows — a real
// hash-chain the anchor math and VerifyChain both walk.
func buildChain(t *testing.T, n int) []LedgerEntry {
	t.Helper()
	l := NewLedger()
	for i := 1; i <= n; i++ {
		if _, err := l.Append(GovDecision{Decision: "AUTO", Reason: fmt.Sprintf("reason-%d", i), ActionID: fmt.Sprintf("act-%d", i)}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	return l.Entries()
}

// headState projects the HEAD + trailing window from a prefix of a chain, the way db.LedgerStore.Head does.
func headState(entries []LedgerEntry, window int) HeadState {
	if len(entries) == 0 {
		return HeadState{}
	}
	start := len(entries) - window
	if start < 0 {
		start = 0
	}
	var recent []RowRef
	for _, e := range entries[start:] {
		recent = append(recent, RowRef{Seq: e.Seq, Hash: e.Hash})
	}
	head := entries[len(entries)-1]
	return HeadState{Seq: head.Seq, Hash: head.Hash, Recent: recent}
}

func TestComputeAnchor_DeterministicAndBindsTheHead(t *testing.T) {
	entries := buildChain(t, 6)
	hs := headState(entries, DefaultAnchorWindow)

	a1 := ComputeAnchor(hs)
	a2 := ComputeAnchor(hs)
	if a1.Digest != a2.Digest {
		t.Fatalf("ComputeAnchor is not deterministic: %s != %s", a1.Digest, a2.Digest)
	}
	if a1.Seq != 6 || a1.Hash != entries[5].Hash || a1.WindowSize != 6 {
		t.Fatalf("anchor did not bind the HEAD: got seq=%d hash=%s window=%d, want seq=6 hash=%s window=6",
			a1.Seq, a1.Hash, a1.WindowSize, entries[5].Hash)
	}
	// A different HEAD (one more row) must move the digest, or the anchor is not committing to the HEAD.
	if b := ComputeAnchor(headState(buildChain(t, 7), DefaultAnchorWindow)); b.Digest == a1.Digest {
		t.Fatal("a 7-row HEAD produced the same digest as a 6-row HEAD — the digest does not bind the HEAD")
	}
	// A within-window edit (flip a recent row's hash) must move the digest too — the window is covered.
	tampered := headState(entries, DefaultAnchorWindow)
	tampered.Recent[len(tampered.Recent)-2].Hash = "deadbeef"
	if ComputeAnchor(tampered).Digest == a1.Digest {
		t.Fatal("altering a windowed row's hash did not change the digest — the window is not covered")
	}
}

func TestVerifyAgainstAnchors_CleanChainAndCleanAppend(t *testing.T) {
	entries := buildChain(t, 8)

	// An anchor over the whole current chain verifies clean.
	full := ComputeAnchor(headState(entries, DefaultAnchorWindow))
	if err := VerifyAgainstAnchors(RowRefsOf(entries), []Anchor{full}); err != nil {
		t.Fatalf("a matching anchor reported tamper on an intact chain: %v", err)
	}
	// A LEGITIMATE APPEND: anchor taken at seq 6, chain has since grown to 8 with 1..6 intact — clean.
	at6 := ComputeAnchor(headState(entries[:6], DefaultAnchorWindow))
	if err := VerifyAgainstAnchors(RowRefsOf(entries), []Anchor{at6}); err != nil {
		t.Fatalf("a forward append (HEAD 6 -> 8, anchored rows intact) was misread as tamper: %v", err)
	}
}

// TestVerifyAgainstAnchors_TailTruncationDetected is the killing oracle in pure form: the exact tamper
// VerifyChain cannot see (a truncated tail still hash-verifies) is caught by the anchor.
func TestVerifyAgainstAnchors_TailTruncationDetected(t *testing.T) {
	entries := buildChain(t, 8)
	anchor := ComputeAnchor(headState(entries, DefaultAnchorWindow)) // witnesses HEAD seq 8

	truncated := entries[:5] // rows 6,7,8 deleted — HEAD regresses to seq 5

	// The control: VerifyChain is BLIND to this — the surviving prefix is a perfect chain.
	if err := VerifyChain(truncated); err != nil {
		t.Fatalf("precondition failed: a truncated tail should still pass VerifyChain, but it did not: %v", err)
	}
	// The catch: the anchor sees the HEAD regressed below a witness.
	err := VerifyAgainstAnchors(RowRefsOf(truncated), []Anchor{anchor})
	if !errors.Is(err, ErrAnchorMismatch) {
		t.Fatalf("a truncated tail (HEAD 8 -> 5) was NOT caught by the anchor — VerifyChain's blind spot is "+
			"still open: got %v", err)
	}
}

// TestVerifyAgainstAnchors_HeadRewriteDetected: a re-linked HEAD (valid chain, different content) passes
// VerifyChain but contradicts the anchored hash.
func TestVerifyAgainstAnchors_HeadRewriteDetected(t *testing.T) {
	entries := buildChain(t, 6)
	anchor := ComputeAnchor(headState(entries, DefaultAnchorWindow))

	// Rewrite row 6 with different content, correctly re-linked to row 5 — a self-consistent forgery.
	l := NewLedgerFromTail(5, entries[4].Hash)
	e6b, err := l.Append(GovDecision{Decision: "AUTO", Reason: "FORGED", ActionID: "act-6"})
	if err != nil {
		t.Fatalf("building the re-linked row: %v", err)
	}
	forged := append(append([]LedgerEntry{}, entries[:5]...), e6b)

	if err := VerifyChain(forged); err != nil {
		t.Fatalf("precondition failed: a re-linked HEAD should still pass VerifyChain, but it did not: %v", err)
	}
	if err := VerifyAgainstAnchors(RowRefsOf(forged), []Anchor{anchor}); !errors.Is(err, ErrAnchorMismatch) {
		t.Fatalf("a rewritten HEAD row was NOT caught by the anchor: got %v", err)
	}
}

func TestVerifyAgainstAnchors_EmptyAnchorIsInert(t *testing.T) {
	entries := buildChain(t, 3)
	// An anchor of the empty ledger (Seq 0) witnesses nothing and must never fire against any chain.
	if err := VerifyAgainstAnchors(RowRefsOf(entries), []Anchor{{Seq: 0}}); err != nil {
		t.Fatalf("an empty-ledger anchor fired against a real chain: %v", err)
	}
}
