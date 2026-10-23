package risk

// THE RATIONALE MUST NOT CONTRADICT THE ARGV UNCHALLENGED (TG-317, TG-154 §2/T7).
//
// Nothing compared the model's stated prose to the mechanically-chosen target. A proposal whose rationale
// reads "restart nginx on web01" could carry target db01 and pass every gate: grammar valid, op-class
// allowlisted, argv built deterministically from the structured params, evidence bound by target equality
// (TG-166), prediction committed.
//
// The rationale is the ONE field a human reviewer on a POLL_PAUSE actually reads. So a poll notice can say
// one thing while the sealed action does another, and the vote authorizes the action, not the prose. That
// is the cheapest seam for a prompt-injected or subverted agent.
//
// These are the ticket's three acceptance cases, plus the two properties that decide whether the check
// survives contact with production: it must ESCALATE rather than refuse, and it must carry the detail that
// makes the poll adjudicable.

import (
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// ACCEPTANCE 1 — a rationale naming a DIFFERENT host than the sealed target escalates to POLL_PAUSE.
// This is the case the killing mutation is executed on.
func TestRationaleNamingADifferentHostEscalatesToPoll(t *testing.T) {
	in := autoEligible()
	in.RationaleHostMismatch = true
	in.RationaleMismatchDetail = "rationale names web01 but the sealed action targets db01"

	d := Classify(in)
	if d.Band != safety.BandPollPause {
		t.Fatalf("band = %v, want BandPollPause. An action whose stated reason names a different machine "+
			"than it touches reached a band that does not require a human — which is the entire seam this "+
			"check exists to close.", d.Band)
	}
	if d.AutoApproved || d.AutoResolve {
		t.Errorf("a mismatched proposal was auto-approved/auto-resolved: %+v", d)
	}
	if d.Signals["poll_reason"] != "rationale-names-a-different-host" {
		t.Errorf("poll_reason = %q, want rationale-names-a-different-host. The audit row must name the "+
			"real reason: an operator reading it at 3am has to see that the prose contradicted the argv.",
			d.Signals["poll_reason"])
	}
}

// ACCEPTANCE 2 — a rationale naming the SAME host does not escalate. The check must not tax honest work.
func TestRationaleNamingTheSameHostDoesNotEscalate(t *testing.T) {
	in := autoEligible() // RationaleHostMismatch stays false: the finding agreed
	if d := Classify(in); d.Band != safety.BandAuto {
		t.Fatalf("band = %v, want BandAuto — an agreeing rationale must cost nothing", d.Band)
	}
}

// ACCEPTANCE 3 — a rationale naming NO host does not escalate. Abstention is the common case: plenty of
// honest rationales never name a machine. Treating silence as disagreement would poll everything and the
// check would be switched off inside a week.
func TestRationaleNamingNoHostAbstainsRatherThanEscalating(t *testing.T) {
	in := autoEligible() // the abstain finding sets neither field
	in.RationaleMismatchDetail = ""
	if d := Classify(in); d.Band != safety.BandAuto {
		t.Fatalf("band = %v, want BandAuto — abstention must not poll", d.Band)
	}
}

// The disagreement must reach the NOTICE. A poll that says "the rationale disagrees" without saying how is
// a poll nobody can adjudicate, and an unadjudicatable poll gets approved on reflex — which would leave
// the seam open with an extra click in front of it.
func TestTheDisagreementReachesThePollNotice(t *testing.T) {
	in := autoEligible()
	in.RationaleHostMismatch = true
	in.RationaleMismatchDetail = "rationale names web01 but the sealed action targets db01"

	d := Classify(in)
	got := d.Signals["rationale_mismatch"]
	if got == "" {
		t.Fatal("the mismatch detail is absent from the decision signals, so the reviewer sees a poll " +
			"reason with no way to check the claim")
	}
	if got != in.RationaleMismatchDetail {
		t.Errorf("signal = %q, want the detail verbatim (%q)", got, in.RationaleMismatchDetail)
	}
}

// It ESCALATES, it never REFUSES. A refusal on a text heuristic takes the estate offline on a wording
// change; the ticket is explicit that the verdict is a band escalation.
func TestTheMismatchPollsRatherThanRefusing(t *testing.T) {
	in := autoEligible()
	in.RationaleHostMismatch = true

	d := Classify(in)
	if d.Band != safety.BandPollPause {
		t.Fatalf("band = %v, want BandPollPause", d.Band)
	}
	// A poll is a request for a human decision — the action must remain approvable, not be dead-ended.
	if d.AutoProceedOnTimeout {
		t.Error("auto_proceed_on_timeout is set: a mismatch would proceed unattended after a timeout, " +
			"which converts the escalation back into no check at all")
	}
}
