package risk

// TG-701: an action on a different host than the incident subject is never autonomous. Actor attribution and the
// sticky-hold memory describe the alerted subject, so for any other host they answered the wrong question.

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// KILLING MUTATION: remove the ActionOnOtherHost branch from classify — RED here.
func TestAnActionOnAnotherHostPollsHoweverGraduated(t *testing.T) {
	in := GatedInput{OpClass: "start-guest", Reversible: Reversible, HasPrediction: true}
	if d := Classify(in); d.Band == safety.BandPollPause {
		t.Fatalf("vacuity control: this input is autonomous without the fact, got %s %v", d.Band, d.Signals)
	}
	in.ActionOnOtherHost, in.ActionOnOtherHostDetail = true, "incident subject a, action resource host b"
	d := Classify(in)
	if d.Band != safety.BandPollPause || d.Signals["poll_reason"] != "action-targets-a-different-host" {
		t.Fatalf("an action on another host must poll on its own reason, got %s %v", d.Band, d.Signals)
	}
	if !strings.Contains(d.Signals["action_other_host"], "incident subject a") {
		t.Fatalf("the detail must reach the signals so a reviewer can see both hosts, got %v", d.Signals)
	}
}

func TestThePollDoesNotNeedADetailToFire(t *testing.T) {
	d := Classify(GatedInput{OpClass: "start-guest", Reversible: Reversible, HasPrediction: true, ActionOnOtherHost: true})
	if d.Band != safety.BandPollPause || d.Signals["poll_reason"] != "action-targets-a-different-host" {
		t.Fatalf("the fact alone must poll, got %s %v", d.Band, d.Signals)
	}
}
