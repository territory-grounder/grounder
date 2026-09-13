package trace

import (
	"strings"
	"testing"
)

// TG-142: action_verdict is content-addressed by action_id and append-only first-wins, so ONE row serves every
// session that proposed the identical action — a session could see a sibling's verdict/timestamp. deriveStatus
// already stops the executed-LIFECYCLE from inheriting a stranger's verdict (the 22/30 live measurement,
// 2026-07-29, documented at deriveStatus). This pins the remaining DISPLAY half: the verify step must LABEL its
// verdict as content-addressed so an auditor does not read the shared timestamp/verdict as this session's own
// execution. A full per-session verdict row would need re-keying action_verdict off (action_id, external_ref),
// which changes the verdict/heal-rate surface (consequential) — deferred; the honest label is the safe fix.
//
// Killing mutation: revert the StepVerify Reason to a plain "deterministic verifier" → RED.
func TestVerifyStepLabelsContentAddressedProvenance(t *testing.T) {
	rec := SpineRecords{
		Classification: ClassificationRecord{Present: true, Band: "AUTO", ActionID: "act-x", PlanHash: "ph-x", CreatedAt: ts(100)},
		Verdict:        VerdictRecord{Present: true, Verdict: "match", CreatedAt: ts(120)},
	}
	tr := Assemble("ext-142", rec)

	var vs *Step
	for i := range tr.Steps {
		if tr.Steps[i].Kind == StepVerify {
			vs = &tr.Steps[i]
			break
		}
	}
	if vs == nil {
		t.Fatalf("no verify step assembled: %+v", tr.Steps)
	}
	if !strings.Contains(vs.Reason, "content-addressed by action_id") {
		t.Fatalf("the verify step must label its verdict as content-addressed (shared across any session that "+
			"proposed the identical action), so an auditor does not attribute the shared timestamp/verdict to this "+
			"session — got Reason=%q", vs.Reason)
	}
	// The verdict value itself is still surfaced (the label is provenance, not a redaction).
	if vs.Verdict != "match" {
		t.Fatalf("the verify step must still carry the verdict, got %q", vs.Verdict)
	}
}
