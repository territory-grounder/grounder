package gate

import (
	"encoding/json"
	"strings"
	"testing"
)

// TG-671 (F11): the base-arm collapse warning is measured against the corpus's own expected-propose share —
// what a perfect policy on a stand-down-heavy corpus produces, and cannot exceed — never against the
// candidate's 0.25 constant. On the 36-incident corpus with 8 propose labels the constant was unreachable by
// construction (ceiling 0.222), so the warning fired on every nightly and TG-658 could not be settled.
func TestBaseDegradedFloor_HoldsMainToTheCandidatesOwnBar(t *testing.T) {
	th := DefaultThresholds()
	// The committed trend baseline's shape (2026-11-06): 3 runs, 108 sessions, 24 expected-propose, rate 0.12,
	// recall 0.54. Recall clears the candidate's 0.50 floor, so main is NOT degraded — a rate 0.12 under a 0.25
	// constant (or under the 0.22 perfect-policy ceiling) said nothing a reader could act on.
	trend := Baseline{Runs: 3, N: 108, Scorecard: Scorecard{N: 108, ExpectedProposeN: 24, LabeledStanddownN: 95, ProposalRate: 0.12, ProposalRecall: 0.54}}
	if d, why := BaseDegradedFloor(trend, th); d || !strings.Contains(why, "recall floor 0.50") || !strings.Contains(why, "24/108 = 0.22") {
		t.Fatalf("committed-baseline shape: degraded=%v why=%q, want not degraded, held to the 0.50 recall floor with the 24/108 share named", d, why)
	}
	// The same card with recall under the floor IS degraded.
	low := trend
	low.Scorecard.ProposalRecall = 0.40
	if d, _ := BaseDegradedFloor(low, th); !d {
		t.Fatal("recall 0.40 under the 0.50 floor must read degraded")
	}
	// A change-mode comparator: Pool SUMS ExpectedProposeN across runs and stores the per-run MEAN in Scorecard.N;
	// Baseline.N is the total. With no recorded recall the line is share × floor = 24/108 × 0.50 = 0.11.
	pooled := Baseline{Runs: 3, N: 108, Scorecard: Scorecard{N: 36, ExpectedProposeN: 24, LabeledStanddownN: 60, ProposalRate: 0.10}}
	if d, why := BaseDegradedFloor(pooled, th); !d || !strings.Contains(why, "0.11 = the corpus's expected-propose share 24/108 (0.22)") {
		t.Fatalf("no recall recorded, rate 0.10: degraded=%v why=%q, want degraded under 0.11 (dividing by the per-run mean would say 0.33)", d, why)
	}
	pooled.Scorecard.ProposalRate = 0.12
	if d, _ := BaseDegradedFloor(pooled, th); d {
		t.Fatal("rate 0.12 at or above the 0.11 line must not read degraded")
	}
	// An older artifact without Baseline.N: Scorecard.N × Runs.
	old := Baseline{Runs: 2, Scorecard: Scorecard{N: 36, ExpectedProposeN: 16, LabeledStanddownN: 40, ProposalRate: 0.05}}
	if d, why := BaseDegradedFloor(old, th); !d || !strings.Contains(why, "16/72") {
		t.Fatalf("no Baseline.N: degraded=%v why=%q, want degraded against 16/72", d, why)
	}
	// A card with no label information at all keeps the legacy constant — the literal 2026-07-25 shape.
	legacy := Baseline{Runs: 1, N: 20, Scorecard: Scorecard{N: 20, ProposalRate: 0.0}}
	if d, why := BaseDegradedFloor(legacy, th); !d || !strings.Contains(why, "legacy 0.25 raw floor") {
		t.Fatalf("unlabelled collapsed card: degraded=%v why=%q, want degraded under the legacy 0.25", d, why)
	}
	legacy.Scorecard.ProposalRate = 0.45
	if d, _ := BaseDegradedFloor(legacy, th); d {
		t.Fatal("an unlabelled card at 0.45 is not degraded")
	}
}

// The warning fires only when main fails the candidate's own bar, it never fails the candidate, and the
// candidate's floors (recall 0.50, prediction coverage) are untouched by the change.
func TestCompare_BaseArmDegradedWarningUsesTheCandidatesBar(t *testing.T) {
	base := baselineFixture()
	base.N = 36
	base.Scorecard.N, base.Scorecard.ExpectedProposeN, base.Scorecard.LabeledStanddownN = 36, 8, 20
	cand := card(3.09, base.Scorecard.DimMeans)
	cand.N, cand.ExpectedProposeN, cand.LabeledStanddownN, cand.ProposalRecall = 36, 8, 20, 0.75

	base.Scorecard.ProposalRate, base.Scorecard.ProposalRecall = 0.12, 0.54
	v := Compare(base, []Scorecard{cand}, nil, DefaultThresholds())
	if !v.Pass {
		t.Fatalf("a healthy candidate must PASS, got %v", v.Reasons)
	}
	for _, w := range v.Warnings {
		if strings.Contains(w, "BASE-ARM DEGRADED") {
			t.Fatalf("a base at recall 0.54 clears the 0.50 bar and must not be called degraded (the 0.25 constant is gone): %q", w)
		}
	}
	base.Scorecard.ProposalRate, base.Scorecard.ProposalRecall = 0.08, 0.38
	v = Compare(base, []Scorecard{cand}, nil, DefaultThresholds())
	if !v.Pass {
		t.Fatalf("a healthy candidate against a degraded base must PASS, got %v", v.Reasons)
	}
	joined := strings.Join(v.Warnings, "\n")
	if !strings.Contains(joined, "BASE-ARM DEGRADED") || !strings.Contains(joined, "proposal_recall 0.38") || strings.Contains(joined, "0.25 raw floor") {
		t.Fatalf("base at recall 0.38 must warn against the recall floor, not the constant: %q", joined)
	}
	// The candidate's recall floor is unchanged: a candidate under 0.50 recall still FAILS.
	cand.ProposalRecall = 0.25
	if v := Compare(base, []Scorecard{cand}, nil, DefaultThresholds()); v.Pass {
		t.Fatal("the candidate's recall floor (0.50) must be untouched by the base-arm change")
	}
}

// The instrument identity rides every pooled card and every verdict (TG-671): the corpus digest when the
// cards agree, a named mixture when they do not, the harness fingerprints as a sorted union.
func TestPoolAndVerdict_CarryCorpusAndHarnessIdentity(t *testing.T) {
	a := card(3.1, baselineFixture().Scorecard.DimMeans)
	a.RubricVersion, a.CorpusSHA256, a.HarnessFingerprints = "r1", "aaaa", []string{"h2", "h1"}
	b := a
	b.HarnessFingerprints = []string{"h1", "h3"}
	pooled := Pool([]Scorecard{a, b})
	if pooled.CorpusSHA256 != "aaaa" || strings.Join(pooled.HarnessFingerprints, ",") != "h1,h2,h3" {
		t.Fatalf("pooled identity = %q %v, want aaaa and h1,h2,h3", pooled.CorpusSHA256, pooled.HarnessFingerprints)
	}
	c := a
	c.CorpusSHA256 = "bbbb"
	if mixed := Pool([]Scorecard{a, c}); mixed.CorpusSHA256 != "mixed:aaaa,bbbb" {
		t.Fatalf("two corpora in one pool must be named, got %q", mixed.CorpusSHA256)
	}
	v := Compare(baselineFixture(), []Scorecard{a, b}, nil, DefaultThresholds())
	if v.RubricVersion != "r1" || v.CorpusSHA256 != "aaaa" || strings.Join(v.HarnessFingerprints, ",") != "h1,h2,h3" {
		t.Fatalf("verdict identity = %q %q %v", v.RubricVersion, v.CorpusSHA256, v.HarnessFingerprints)
	}
	raw, _ := json.Marshal(v)
	for _, key := range []string{`"corpus_sha256":"aaaa"`, `"rubric_version":"r1"`, `"harness_fingerprints":["h1","h2","h3"]`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("verdict.json lacks %s: %s", key, raw)
		}
	}
	// A card without identity pools to no identity — never to an invented one.
	if empty := Pool([]Scorecard{card(3, nil)}); empty.CorpusSHA256 != "" || empty.HarnessFingerprints != nil {
		t.Fatalf("an identity-less pool invented identity: %q %v", empty.CorpusSHA256, empty.HarnessFingerprints)
	}
}
