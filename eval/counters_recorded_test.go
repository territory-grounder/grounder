package eval

import (
	"encoding/json"
	"testing"

	"github.com/territory-grounder/grounder/eval/gate"
)

// TG-678 — the PRODUCER stamps its counters. Aggregate is the one writer of a harness scorecard (the on-box
// harness and tools/rejudge both go through it), so the stamp it sets is what lets gate.VerifyIntegrity tell a
// fresh card from a legacy one.
func TestAggregateStampsCountersRecorded(t *testing.T) {
	full := map[string]int{}
	for _, d := range Dimensions {
		full[d] = 4
	}
	sessions := []Session{{Ref: "a"}, {Ref: "b"}}
	card := Aggregate(sessions, []Score{{Ref: "a", Scores: full}, {Ref: "b", Scores: full}})
	if !card.CountersRecorded || card.Judged != 2 || card.Errors != 0 {
		t.Fatalf("Aggregate must stamp counters_recorded with judged/errors counted: %+v", card)
	}
	var gc gate.Scorecard
	if err := json.Unmarshal(ScorecardJSON(card), &gc); err != nil {
		t.Fatal(err)
	}
	if !gc.CountersRecorded || gc.Judged != 2 {
		t.Fatalf("the stamp must round-trip into gate.Scorecard: %+v", gc)
	}
	if probs, notes := gate.VerifyIntegrity("candidate", []gate.Scorecard{gc}, 2), gate.IntegrityDisclosures("candidate", []gate.Scorecard{gc}); len(probs) != 0 || len(notes) != 0 {
		t.Errorf("a complete fresh card verifies silently: %v %v", probs, notes)
	}
}

// CAN A CURRENT PRODUCER REACH THE LEGACY BRANCH? Aggregate cannot: when nothing was judged it leaves Overall at
// 0 (the integrity signal the gate refuses on BEFORE the judged check), and it stamps the card anyway, so even a
// hand-edited overall would be refused as "only 0/N judged" rather than tolerated. The one producer that did
// reach it was gate.Pool (archived pooled cards: judged 0, overall > 0), fixed in the same change.
func TestAZeroJudgedAggregateCannotReachTheLegacyBranch(t *testing.T) {
	card := Aggregate([]Session{{Ref: "a"}, {Ref: "b"}}, nil)
	if card.Judged != 0 || card.Overall != 0 || !card.CountersRecorded {
		t.Fatalf("a zero-judged aggregate must read overall 0 and still be stamped: %+v", card)
	}
	var gc gate.Scorecard
	if err := json.Unmarshal(ScorecardJSON(card), &gc); err != nil {
		t.Fatal(err)
	}
	probs, notes := gate.VerifyIntegrity("candidate", []gate.Scorecard{gc}, 2), gate.IntegrityDisclosures("candidate", []gate.Scorecard{gc})
	if len(probs) == 0 || len(notes) != 0 {
		t.Fatalf("a zero-judged fresh card must be a problem, never a legacy note: %v %v", probs, notes)
	}
	// Even with the overall forged positive, the stamp keeps it out of the legacy branch.
	gc.Overall = 3.0
	if probs, notes := gate.VerifyIntegrity("candidate", []gate.Scorecard{gc}, 2), gate.IntegrityDisclosures("candidate", []gate.Scorecard{gc}); len(probs) == 0 || len(notes) != 0 {
		t.Fatalf("a stamped zero-judged card with a positive overall must be refused: %v %v", probs, notes)
	}
}
