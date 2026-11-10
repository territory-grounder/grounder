package runner

import (
	"testing"

	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
)

// TG-685: the fallback approver released disk-grow twice as "op-class-not-graduated" — it cleared the ungraduated
// flag, re-classified, and the counterfactual read the model's "reversible" claim as reversible. Now the registry
// declares disk-grow irreversible: the counterfactual still polls, and the approver HOLDS naming irreversible-op-class.
// The positive control: an ungraduated start-guest (registry-reversible) still ACTS.
func TestTheFallbackApproverHoldsAnIrreversibleClassAndActsOnAReversibleOne(t *testing.T) {
	hold := risk.GatedInput{OpClass: "disk-grow", Reversible: risk.Reversible, HasPrediction: true, UngraduatedClass: true}
	r := resolvePollAutonomously(hold, risk.Classify(hold))
	if !r.Resolved || r.Approve || r.Key != "irreversible-op-class" {
		t.Fatalf("an ungraduated disk-grow must be HELD as irreversible-op-class, got %+v", r)
	}
	act := risk.GatedInput{OpClass: "start-guest", Reversible: risk.Reversible, HasPrediction: true, UngraduatedClass: true}
	polled := risk.Classify(act)
	if polled.Band != safety.BandPollPause {
		t.Fatalf("fixture: an ungraduated class polls, got %s", polled.Band)
	}
	if r := resolvePollAutonomously(act, polled); !r.Resolved || !r.Approve || r.Key != "op-class-not-graduated" {
		t.Fatalf("an ungraduated start-guest must still ACT (positive control), got %+v", r)
	}
}
