package skillstore

import (
	"strings"
	"testing"
)

// chainTestFacts builds deterministic facts for row i of a synthetic corpus.
func chainTestFacts(i int64) ChainFacts {
	return ChainFacts{
		ID:        i,
		SkillName: "drift-check",
		Version:   "1.0." + strings.Repeat("x", int(i%3)),
		ContentHash: ContentHash("body-"+strings.Repeat("b", int(i)),
			AppliesWhen{Phases: []string{"investigate"}}),
		Author: "flywheel", Source: "distill:test",
	}
}

// chainTestBuild returns a well-formed n-row chain plus its head.
func chainTestBuild(n int64) ([]ChainRow, string) {
	prev := ChainGenesis
	rows := make([]ChainRow, 0, n)
	for i := int64(1); i <= n; i++ {
		f := chainTestFacts(i)
		link := ChainLink(prev, f)
		rows = append(rows, ChainRow{Facts: f, StoredLink: link})
		prev = link
	}
	return rows, prev
}

func TestChainLinkBindsPrevAndEveryFact(t *testing.T) {
	base := chainTestFacts(1)
	link := ChainLink(ChainGenesis, base)
	if link != ChainLink(ChainGenesis, base) {
		t.Fatal("link is not deterministic")
	}
	if ChainLink("other-prev", base) == link {
		t.Fatal("link ignores prev")
	}
	mutations := []func(*ChainFacts){
		func(f *ChainFacts) { f.ID++ },
		func(f *ChainFacts) { f.SkillName += "x" },
		func(f *ChainFacts) { f.Version += "x" },
		func(f *ChainFacts) { f.ContentHash += "x" },
		func(f *ChainFacts) { f.Author += "x" },
		func(f *ChainFacts) { f.Source += "x" },
		func(f *ChainFacts) { f.ParentVersionID = 99 },
	}
	for i, mut := range mutations {
		f := base
		mut(&f)
		if ChainLink(ChainGenesis, f) == link {
			t.Fatalf("mutation %d does not change the link — that fact is unbound", i)
		}
	}
}

// The length-prefix framing must make field boundaries unforgeable: moving a byte across a field
// boundary yields a different link even though the concatenation is identical.
func TestChainLinkFieldBoundariesAreUnforgeable(t *testing.T) {
	a := ChainFacts{ID: 1, SkillName: "ab", Version: "c", ContentHash: "h", Author: "a", Source: "s"}
	b := ChainFacts{ID: 1, SkillName: "a", Version: "bc", ContentHash: "h", Author: "a", Source: "s"}
	if ChainLink(ChainGenesis, a) == ChainLink(ChainGenesis, b) {
		t.Fatal("field boundary is forgeable — length prefixing is not doing its job")
	}
}

func TestVerifyChainOKCarriesDenominator(t *testing.T) {
	rows, head := chainTestBuild(3)
	rep := VerifyChain(rows, head, 3)
	if !rep.OK || rep.Verified != 3 || rep.Total != 3 {
		t.Fatalf("healthy chain rejected: %+v", rep)
	}
	if s := rep.String(); !strings.Contains(s, "3 of 3") {
		t.Fatalf("OK verdict lacks its denominator: %q", s)
	}
}

func TestVerifyChainEmptyIsItsOwnHealthyState(t *testing.T) {
	rep := VerifyChain(nil, ChainGenesis, 0)
	if !rep.OK || rep.Total != 0 || rep.Head != ChainGenesis {
		t.Fatalf("empty corpus must verify at genesis: %+v", rep)
	}
	s := rep.String()
	if !strings.Contains(s, "0 of 0") || !strings.Contains(s, "state, not an error") {
		t.Fatalf("empty verdict must be distinct and carry 0 of 0: %q", s)
	}
	if s == (ChainReport{OK: true, Total: 3, Verified: 3, Head: "h"}).String() {
		t.Fatal("empty and non-empty OK verdicts must not share a sentence")
	}
}

func TestVerifyChainCatchesTamperedRow(t *testing.T) {
	rows, head := chainTestBuild(3)
	rows[1].Facts.ContentHash = "tampered" // the recomputed hash a body edit produces
	rep := VerifyChain(rows, head, 3)
	if rep.OK || rep.Reason != ChainReasonLinkMismatch || rep.BrokenID != rows[1].Facts.ID {
		t.Fatalf("tampered row not named: %+v", rep)
	}
	if rep.Verified != 1 {
		t.Fatalf("verified prefix should be 1, got %d", rep.Verified)
	}
	if s := rep.String(); !strings.Contains(s, "1 of 3") || !strings.Contains(s, "refused") {
		t.Fatalf("broken verdict lacks denominator or refusal: %q", s)
	}
}

func TestVerifyChainCatchesMissingLink(t *testing.T) {
	rows, head := chainTestBuild(2)
	rows[1].StoredLink = "" // a raw INSERT around the chained writer
	rep := VerifyChain(rows, head, 2)
	if rep.OK || rep.Reason != ChainReasonMissingLink || rep.BrokenID != rows[1].Facts.ID {
		t.Fatalf("missing link not named: %+v", rep)
	}
}

func TestVerifyChainCatchesDeletedMiddleRow(t *testing.T) {
	rows, head := chainTestBuild(3)
	spliced := []ChainRow{rows[0], rows[2]} // row 2 deleted out-of-band
	rep := VerifyChain(spliced, head, 3)
	if rep.OK || rep.Reason != ChainReasonLinkMismatch || rep.BrokenID != rows[2].Facts.ID {
		t.Fatalf("middle deletion not caught at the successor: %+v", rep)
	}
}

func TestVerifyChainCatchesTruncatedTail(t *testing.T) {
	rows, head := chainTestBuild(3)
	rep := VerifyChain(rows[:2], head, 3) // last row deleted, head row untouched
	if rep.OK || rep.Reason != ChainReasonCountMismatch {
		t.Fatalf("tail truncation not caught: %+v", rep)
	}
}

func TestVerifyChainCatchesStaleHead(t *testing.T) {
	rows, _ := chainTestBuild(2)
	rep := VerifyChain(rows, "not-the-head", 2)
	if rep.OK || rep.Reason != ChainReasonHeadMismatch {
		t.Fatalf("stale head not caught: %+v", rep)
	}
}

func TestUninitializedChainRefusesWithRepairNamed(t *testing.T) {
	rep := UninitializedChainReport(5)
	if rep.OK {
		t.Fatal("uninitialized chain must not verify")
	}
	if s := rep.String(); !strings.Contains(s, "EnsureChain") {
		t.Fatalf("uninitialized verdict must name the repair: %q", s)
	}
}
