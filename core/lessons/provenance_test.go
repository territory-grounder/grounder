package lessons

// The verified-resolution stamp must stay welded to the gates that justify it (TG-172 item 1).
//
// core/knowledge trusts this label: MergeCorpus lets a verified row refuse displacement by an unverified
// one, and the precedent block prints "verified TG resolution" to the model. Both are claims about what
// happened HERE — a clean mechanical verdict AND a confirmed-clear condition. If the stamp ever moved to a
// site that does not enforce those two gates, or the gates were relaxed under it, the label would keep
// asserting a verification nothing performed, and it would assert it inside the one block the agent leans
// on. These tests exist so that failure is loud in this package rather than invisible in the other.

import (
	"testing"

	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/safety"
)

func verifiedResolution() ResolvedIncident {
	return ResolvedIncident{
		ExternalRef:    "TG-1",
		Host:           "dc1k8s01",
		AlertRule:      "Service-up/down",
		Action:         "restarted the kubelet",
		Summary:        "kubelet had wedged after a disk stall",
		Verdict:        safety.VerdictMatch,
		ConfirmedClear: true,
	}
}

func TestAVerifiedLessonIsStampedVerified(t *testing.T) {
	got, ok := Lesson(verifiedResolution())
	if !ok {
		t.Fatal("a clean-verdict, confirmed-clear resolution was not distilled at all — this test would " +
			"otherwise assert a provenance on a lesson that does not exist")
	}
	if got.Source != knowledge.ProvenanceVerifiedResolution {
		t.Errorf("a mechanically verified resolution entered the corpus stamped %q, want %q.\n"+
			"Unstamped it ranks as unknown, so it can be displaced under its own ExternalRef by a runbook "+
			"row, and it renders to the model as 'unknown provenance' — the one class of row that IS "+
			"evidence would be the one the agent is told to distrust.",
			got.Source, knowledge.ProvenanceVerifiedResolution)
	}
}

// The stamp must be unreachable for anything the gates reject. Asserted by walking the rejections rather
// than by reading the code: a lesson that is not produced cannot carry a label.
func TestNothingTheGatesRejectCanCarryTheVerifiedStamp(t *testing.T) {
	cases := map[string]func(*ResolvedIncident){
		"verdict is not a clean match":                func(r *ResolvedIncident) { r.Verdict = safety.VerdictDeviation },
		"condition was never confirmed clear":         func(r *ResolvedIncident) { r.ConfirmedClear = false },
		"no action, so there is no precedent to cite": func(r *ResolvedIncident) { r.Action = "  " },
		"no external_ref, so it cannot be keyed":      func(r *ResolvedIncident) { r.ExternalRef = "" },
	}
	for name, mutate := range cases {
		ri := verifiedResolution()
		mutate(&ri)
		got, ok := Lesson(ri)
		if ok {
			t.Errorf("%s: distilled anyway, stamped %q — the label would assert a verification that this "+
				"outcome did not receive", name, got.Source)
		}
		if got.Source != knowledge.ProvenanceUnknown {
			t.Errorf("%s: rejected but returned a non-zero Source %q", name, got.Source)
		}
	}
}

// THE REFUSED GATE, PINNED. TG-153 recommended gating this writeback on graduation and TG-296 recorded the
// correction: a first-occurrence de-novel IS a POLL_PAUSE-band resolution, so a graduation gate would mean
// the loop only ever learned from incidents it had already learned from. TG-172 item 1 restates the
// original recommendation, so the correction needs a test and not only a comment — the next reader
// implementing that ticket verbatim should get a red build, not a silent regression.
func TestAFirstOccurrenceStillBecomesPrecedent(t *testing.T) {
	ri := verifiedResolution()
	ri.ExternalRef = "TG-first-ever" // nothing has been graduated for this host or rule
	got, ok := Lesson(ri)
	if !ok {
		t.Fatal("a first-occurrence resolution was refused. If this became a graduation gate, the " +
			"learn->retrieve loop could only learn from incidents it had already learned from, and the " +
			"de-novel path this writeback exists to feed could never fire once. See TG-296.")
	}
	if got.Source != knowledge.ProvenanceVerifiedResolution {
		t.Errorf("a first occurrence was distilled but downgraded to %q. Graduation state is not what the "+
			"stamp claims — a clean verdict and a confirmed-clear condition are.", got.Source)
	}
}
