package runner

// TG-681 — "alert host not in the graph" end to end: the alert names a host the estate snapshot cannot place
// (renamed guest, decommissioned device, a host the CMDB never carried, a k8s alert with no host at all). The
// eligibility oracle answers false, the classifier polls with reason no-committed-prediction, and the autonomous
// fallback approver (spec/012 REQ-1127) HOLDS rather than acts — because clearing the first-look reasons does not
// clear this one. The chain is three existing checks; this oracle pins that they compose to a HOLD, so the
// documented posture ("an unplaceable target never reaches an effect") is tested rather than asserted.
//
// KILLING MUTATIONS (executed 2026-11-07, each RED, then reverted):
//  1. make eligible() return true when the oracle is nil → TestAnUnwiredEligibilityOracleIsNotEligible fails.
//  2. add "no-committed-prediction" to the reasons resolvePollAutonomously clears → the HOLD assertion in
//     TestAnAlertHostAbsentFromTheGraphIsHeldNotActed fails ("Approve=true").

import (
	"testing"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
)

func sharedInputsHolder() *estate.Holder {
	g := estate.NewGraph()
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeLXC, Name: "web01"}, To: estate.Entity{Type: estate.TypePVENode, Name: "pve01"},
		Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	return estate.NewHolder(g)
}

func TestAnAlertHostAbsentFromTheGraphIsHeldNotActed(t *testing.T) {
	h := sharedInputsHolder()
	// The composition root's oracle shape (cmd/worker/main.go): eligible ⇔ the name resolves in the LIVE snapshot.
	a := &Activities{D: Deps{PredictionEligible: func(host string) bool { _, ok := h.Graph().Resolve(host); return ok }}}

	if !a.eligible("web01") {
		t.Fatal("fixture: the seeded guest is eligible")
	}
	for _, host := range []string{"web01-renamed", "decommissioned01", ""} {
		if a.eligible(host) {
			t.Fatalf("%q is not in the snapshot and must be INELIGIBLE", host)
		}
		gi := risk.GatedInput{OpClass: "start-guest", Reversible: risk.Reversible, HasPrediction: a.eligible(host)}
		polled := risk.Classify(gi)
		if polled.Band != safety.BandPollPause || polled.Signals["poll_reason"] != "no-committed-prediction" {
			t.Fatalf("%q: an ineligible target must poll for no-committed-prediction, got %s %v", host, polled.Band, polled.Signals)
		}
		r := resolvePollAutonomously(gi, polled)
		if !r.Resolved || r.Approve {
			t.Fatalf("%q: the autonomous approver must HOLD an unplaceable target (it is not a missing first look), got %+v", host, r)
		}
		if r.Key != "no-committed-prediction" {
			t.Fatalf("%q: the hold must name the reason that remains, got %q", host, r.Key)
		}
	}
}

// The same hold when the graph has gone away entirely (a boot before the topology readers seed, or a holder
// whose every refresh was rejected into the empty placeholder): with nothing to resolve against, every host is
// ineligible and every ACT polls into a hold. The empty estate is fail-closed, not fail-open.
func TestAnEmptyEstateHoldsEveryTarget(t *testing.T) {
	h := estate.NewHolder(nil)
	a := &Activities{D: Deps{PredictionEligible: func(host string) bool { _, ok := h.Graph().Resolve(host); return ok }}}
	if a.eligible("web01") {
		t.Fatal("an empty snapshot places nothing")
	}
	gi := risk.GatedInput{OpClass: "restart-service", Reversible: risk.Reversible, HasPrediction: a.eligible("web01")}
	if r := resolvePollAutonomously(gi, risk.Classify(gi)); !r.Resolved || r.Approve {
		t.Fatalf("an empty estate must HOLD, got %+v", r)
	}
}

func TestAnUnwiredEligibilityOracleIsNotEligible(t *testing.T) {
	a := &Activities{D: Deps{}}
	if a.eligible("web01") {
		t.Fatal("no oracle wired ⇒ no host is eligible (never assume a prediction exists for a host we cannot place)")
	}
}
