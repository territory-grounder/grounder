package risk

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// A REFUSAL NOBODY WAS ASKED ABOUT IS A DEAD END, NOT A GATE.
//
// The BAND decides whether a poll is opened. The POLICY VERDICT decides whether a human approval is required.
// Those are set in different places, and when an ungraduated op-class landed in an AUTO-banded incident the
// policy engine correctly refused it — while no poll existed, so nobody could ever approve it.
//
// Measured 2026-07-28 over 24h:
//
//	band          verdict   sessions   polls opened
//	AUTO          auto      175        0    correct, executes hands-off
//	AUTO_NOTICE   auto       43        0    correct
//	AUTO          approve    13        0    <-- dead end
//	POLL_PAUSE    approve    11       11    correct, resolvable
//
// Found via a real case: TG chose the brand-new `start-container` verb entirely on its own for a container-down
// on dc1mealie01, correctly distinguishing it from `restart-container` — then the session died with
// "policy verdict approve — needs a human approval, none recorded (no auto-execute)". The verb worked; the
// governance path had no way to say yes.
//
// Consequence: an ungraduated class could only ever accrue clean runs when the classifier HAPPENED to band the
// incident POLL_PAUSE, so graduation was left to chance.

func ungradInput() GatedInput {
	return GatedInput{
		ExternalRef: "TG-1", ActionID: "a1", PlanHash: "p1", RiskLevel: "low",
		OpClass: "start-container", Reversible: Reversible,
		HasPrediction: true, UngraduatedClass: true,
	}
}

// TestAnUngraduatedClassIsPolledSoItsApprovalCanBeGiven is the live defect as an oracle.
func TestAnUngraduatedClassIsPolledSoItsApprovalCanBeGiven(t *testing.T) {
	d := Classify(ungradInput())
	if d.Band != safety.BandPollPause {
		t.Fatalf("an ungraduated op-class banded %v — the policy engine will demand a human approval at "+
			"execute time, and with no poll there is nobody to give it: the action is refused and the "+
			"graduation opportunity is lost forever", d.Band)
	}
	if got := d.Signals["poll_reason"]; got != "op-class-not-graduated" {
		t.Errorf("poll_reason = %q, want op-class-not-graduated — an operator reading the audit row must be "+
			"able to tell WHY they are being asked", got)
	}
}

// TestAGraduatedClassIsNotPolledByThisRule — the guard must not clamp everything. A class that HAS earned auto
// composes to `auto`, needs no approval, and must keep running hands-off; otherwise graduation buys nothing.
func TestAGraduatedClassIsNotPolledByThisRule(t *testing.T) {
	in := ungradInput()
	in.UngraduatedClass = false
	if d := Classify(in); d.Band == safety.BandPollPause && d.Signals["poll_reason"] == "op-class-not-graduated" {
		t.Error("a GRADUATED class was polled as ungraduated — earning auto must actually buy hands-off " +
			"execution, or the ladder is decorative")
	}
}

// TestTheUngraduatedRuleOnlyRAISESReview — it must never lower a band another rule already raised. Every
// safety poll reason that outranks it must survive, or a review-raising flag becomes a review-LOWERING one.
func TestTheUngraduatedRuleOnlyRAISESReview(t *testing.T) {
	for name, mut := range map[string]func(*GatedInput){
		"suspicious actor": func(i *GatedInput) { i.AttributionSecurity = true },
		// stateful clamps only a MUTATING op — a fully-reversible read-only op on a stateful target is exempt
		// by design, so the fixture must mutate for this rule to be the stronger one.
		"stateful mutation":  func(i *GatedInput) { i.StatefulTarget = true; i.Reversible = ReversibleMixed },
		"server destructive": func(i *GatedInput) { i.ServerDestructive = true },
		"self-protected":     func(i *GatedInput) { i.SelfProtectedRestart = true },
		"jailbreak":          func(i *GatedInput) { i.Jailbreak = true },
	} {
		in := ungradInput()
		mut(&in)
		d := Classify(in)
		if d.Band != safety.BandPollPause {
			t.Errorf("%s: band %v — a stronger safety rule must still force POLL_PAUSE", name, d.Band)
		}
		if r := d.Signals["poll_reason"]; r == "op-class-not-graduated" {
			t.Errorf("%s: poll_reason was demoted to %q — the ungraduated rule must not MASK a stronger "+
				"reason, or the audit row misreports why a human was asked", name, r)
		}
	}
}

// TestAnEmptyOpClassIsNotTreatedAsUngraduated — the flag is set by the activity from a resolver; a blank class
// means "no resolver answer", which must stay inert rather than polling everything.
func TestAnEmptyOpClassIsNotTreatedAsUngraduated(t *testing.T) {
	in := ungradInput()
	in.OpClass = ""
	in.UngraduatedClass = false // what the activity sets for a blank class
	if d := Classify(in); d.Signals["poll_reason"] == "op-class-not-graduated" {
		t.Error("a blank op-class was polled as ungraduated")
	}
}

// TestThePollReasonIsAuditable — the audit row is what an operator reads at 3am. It must name this rule and
// not be silently empty.
func TestThePollReasonIsAuditable(t *testing.T) {
	d := Classify(ungradInput())
	if !strings.Contains(d.Signals["poll_reason"], "graduat") {
		t.Errorf("poll_reason %q does not identify the graduation rule", d.Signals["poll_reason"])
	}
}
