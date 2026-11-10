package risk

import (
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// A STILL-OPEN sticky hold from a prior session polls the re-detected incident (TG-667). The input is otherwise
// the cleanest AUTO shape in the grammar — reversible, graduated, predicted, not novel — so the only thing
// standing between it and AUTO is the remembered hold. Killing mutation: delete the PriorHoldOpen branch in
// classifier.go and the first assertion fails (the band reads AUTO).
func autoInput() GatedInput {
	return GatedInput{
		ExternalRef: "TG-1", ActionID: "a1", PlanHash: "p1", RiskLevel: "low",
		OpClass: "start-container", Reversible: Reversible, HasPrediction: true,
	}
}

func TestPriorHoldStillOpenPolls(t *testing.T) {
	control := Classify(autoInput())
	if control.Band != safety.BandAuto {
		t.Fatalf("control: the base input must classify AUTO so the test measures the hold alone; got %s (%v)", control.Band, control.Signals)
	}
	in := autoInput()
	in.PriorHoldOpen = true
	in.PriorHoldDetail = "actor-attributed-authorized@INC-0 by tg:fallback-approver"
	d := Classify(in)
	if d.Band != safety.BandPollPause {
		t.Fatalf("an open prior hold must POLL_PAUSE, got %s", d.Band)
	}
	if d.Signals["poll_reason"] != "prior-hold-still-open" {
		t.Fatalf("poll_reason = %q, want prior-hold-still-open", d.Signals["poll_reason"])
	}
	if d.Signals["prior_hold"] != in.PriorHoldDetail {
		t.Fatalf("the audit row must name the prior decision being honoured; prior_hold = %q", d.Signals["prior_hold"])
	}
	if d.AutoApproved || d.AutoResolve || d.AutoProceedOnTimeout {
		t.Fatalf("a poll never auto-approves: %+v", d)
	}
}

// Attribution reasons record first: a suspicious actor THIS time outranks a remembered hold, so the operator
// reading the audit row sees the stronger, fresher fact.
func TestPriorHoldYieldsToAFreshAttributionSignal(t *testing.T) {
	in := autoInput()
	in.PriorHoldOpen = true
	in.AttributionSecurity = true
	d := Classify(in)
	if d.Signals["poll_reason"] != "actor-attributed-suspicious" {
		t.Fatalf("poll_reason = %q, want the fresher actor-attributed-suspicious", d.Signals["poll_reason"])
	}
}

// Without the flag the classifier is byte-identical to before: a deployment with no hold store wired reads
// PriorHoldOpen=false everywhere and bands exactly as it did.
func TestPriorHoldFlagOffIsInert(t *testing.T) {
	in := autoInput()
	in.PriorHoldDetail = "stale detail with no open hold"
	d := Classify(in)
	if d.Band != safety.BandAuto {
		t.Fatalf("detail without the flag must not poll; got %s", d.Band)
	}
	if _, ok := d.Signals["prior_hold"]; ok {
		t.Fatal("no prior_hold signal may be recorded when no hold is open")
	}
}
