package trace

import (
	"strings"
	"testing"
)

// spec/023 REQ-2311 — "The decision tracer surfaces the attribution as a named trace step."
//
// FOUND BY PRODUCER SCAN, not by reading the requirement. Assemble emitted twelve step kinds — ingest,
// correlate, classify, screen, rag, agent-cycle, credential, propose, predict, policy, gate, verify —
// and none of them was `attribute`. The taxonomy has been derived and durably persisted on
// session_triage for 3,383 sessions and appeared on NO step of the walk.
//
// That is the present-but-not-reaching shape in its most consequential place. The tracer IS the
// explanation surface: it is what an operator reads at 3am to understand why TG did what it did. A
// determination that never reaches it may as well not have been made, for the human reading it — and
// this particular determination is the one that decides whether the estate self-heals, stands down, or
// escalates a suspected intrusion.
//
// The step is emitted BEFORE propose deliberately. Attribution is an INPUT to the decision, not a
// report on it: a carve-out heals a manufactured pool fault, and a suspicious actor forces POLL_PAUSE
// with the security escalation. Ordering it after propose would render the cause as a consequence.

func TestAttributeStepIsEmittedBeforeProposeAndCarriesTheTaxonomy(t *testing.T) {
	rec := SpineRecords{
		Classification: ClassificationRecord{Present: true, Band: "POLL_PAUSE", ActionID: "act-9", CreatedAt: ts(100)},
		Triage: TriageRecord{
			Present: true, Host: "web01", AlertRule: "NginxDown", Band: "POLL_PAUSE",
			Proposed: true, Op: "restart-service", Conclusion: "nginx down", CreatedAt: ts(110),
			Attribution:         "attributed-suspicious",
			AttributionEvidence: []string{"pve:UPID:dc1pve01:0029D107", "journal:c-4821"},
		},
	}
	tr := Assemble("ext-attr", rec)

	var attrIdx, proposeIdx = -1, -1
	for i, s := range tr.Steps {
		switch s.Kind {
		case StepAttribute:
			attrIdx = i
		case StepPropose:
			proposeIdx = i
		}
	}
	if attrIdx < 0 {
		var kinds []string
		for _, s := range tr.Steps {
			kinds = append(kinds, string(s.Kind))
		}
		t.Fatalf("no attribute step in the walk — the taxonomy is derived and persisted on every session and "+
			"reaches the operator nowhere. Kinds emitted: %v", kinds)
	}
	if proposeIdx < 0 {
		t.Fatal("precondition: this fixture proposes, so a propose step must exist to order against")
	}
	if attrIdx > proposeIdx {
		t.Errorf("the attribute step must precede propose (attribution is an INPUT to the decision, not a "+
			"report on it) — got attribute at %d, propose at %d", attrIdx, proposeIdx)
	}

	st := tr.Steps[attrIdx]
	if st.Verdict != "attributed-suspicious" {
		t.Errorf("the step must carry the TAXONOMY VALUE, got verdict=%q", st.Verdict)
	}
	if !strings.Contains(st.Label, "attributed-suspicious") {
		t.Errorf("the label must name the taxonomy so the walk reads without opening the step: %q", st.Label)
	}
	// evidence REFERENCES — the pointers an operator follows to check the finding.
	if len(st.Tools) != 2 {
		t.Fatalf("the step must carry the evidence references, got %v", st.Tools)
	}
	for _, want := range []string{"pve:UPID:dc1pve01:0029D107", "journal:c-4821"} {
		var found bool
		for _, got := range st.Tools {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("evidence reference %q missing from the step: %v", want, st.Tools)
		}
	}
	// The reason must state the CONSEQUENCE, not restate the taxonomy — the value already rides on Verdict,
	// and a reason that only spells it out again costs a line and adds nothing.
	if st.Reason == "" || strings.EqualFold(strings.TrimSpace(st.Reason), st.Verdict) {
		t.Errorf("the reason must say what the taxonomy MEANT for the decision, not repeat it: %q", st.Reason)
	}
	if !strings.Contains(strings.ToLower(st.Reason), "escalation") {
		t.Errorf("a suspicious attribution's reason must name the security escalation — that is what it did "+
			"to the decision: %q", st.Reason)
	}
}

// ABSENT IS NOT UNATTRIBUTABLE. A session recorded before attribution existed carries an empty column,
// and rendering that as "unattributable" would put a VERDICT on the walk that nobody reached.
// "Nobody asked" and "asked and could not tell" are different facts about the estate.
func TestNoAttributionRecordedEmitsNoStepRatherThanAFabricatedVerdict(t *testing.T) {
	rec := SpineRecords{
		Classification: ClassificationRecord{Present: true, Band: "AUTO", ActionID: "act-8", CreatedAt: ts(100)},
		Triage: TriageRecord{
			Present: true, Host: "web01", AlertRule: "NginxDown", Proposed: true,
			Op: "restart-service", CreatedAt: ts(110), Attribution: "", // pre-attribution session
		},
	}
	tr := Assemble("ext-old", rec)

	for _, s := range tr.Steps {
		if s.Kind == StepAttribute {
			t.Fatalf("a session that recorded NO taxonomy produced an attribute step (%q/%q) — an absent "+
				"determination must not render as a reached one (INV-15)", s.Label, s.Verdict)
		}
	}
	// And the walk is otherwise intact — the omission removes a step, never the session.
	if len(tr.Steps) == 0 {
		t.Fatal("omitting the attribute step must not empty the walk")
	}
}

// A whitespace-only taxonomy is the same absence wearing a different shape, and it is the one a column
// default or a trimmed write actually produces.
func TestABlankTaxonomyIsTreatedAsAbsent(t *testing.T) {
	rec := SpineRecords{
		Classification: ClassificationRecord{Present: true, Band: "AUTO", ActionID: "act-7", CreatedAt: ts(100)},
		Triage: TriageRecord{
			Present: true, Host: "web01", AlertRule: "NginxDown", Proposed: true,
			Op: "restart-service", CreatedAt: ts(110), Attribution: "   ",
		},
	}
	for _, s := range Assemble("ext-blank", rec).Steps {
		if s.Kind == StepAttribute {
			t.Fatalf("a whitespace-only taxonomy produced an attribute step labelled %q", s.Label)
		}
	}
}

// Every taxonomy the attributor can emit must render a MEANING, and an unknown one must be named rather
// than defaulted into a known meaning. A vocabulary drift that silently rendered as
// "no admissible evidence named an actor" would misreport a security-path outcome as a benign one.
func TestEveryTaxonomyRendersItsConsequenceAndAnUnknownOneIsNamed(t *testing.T) {
	for _, tc := range []struct{ taxonomy, wantSubstring string }{
		{"attributed-self", "already remediated"},
		{"attributed-authorized", "stand down"},
		{"authorized-test", "carve-out"},
		{"attributed-suspicious", "escalation"},
		{"unattributable", "absence of evidence"},
	} {
		got := attributionReason(tc.taxonomy)
		if !strings.Contains(strings.ToLower(got), tc.wantSubstring) {
			t.Errorf("%s must render its CONSEQUENCE containing %q, got %q", tc.taxonomy, tc.wantSubstring, got)
		}
		if strings.EqualFold(strings.TrimSpace(got), tc.taxonomy) {
			t.Errorf("%s renders only its own name, which adds nothing to the Verdict field", tc.taxonomy)
		}
	}
	// The vacuity floor on the switch: an unrecognised value must be NAMED, not silently mapped.
	unknown := attributionReason("attributed-by-vibes")
	if !strings.Contains(unknown, "attributed-by-vibes") {
		t.Errorf("an unknown taxonomy must be named in the reason, got %q", unknown)
	}
	if !strings.Contains(strings.ToLower(unknown), "drift") {
		t.Errorf("an unknown taxonomy must say the vocabulary has drifted rather than reading as a meaning: %q", unknown)
	}
	// Case/whitespace folding, because the column is written by more than one path.
	if attributionReason("  Attributed-Suspicious  ") != attributionReason("attributed-suspicious") {
		t.Error("the taxonomy match must fold case and trim — a writer's spelling must not change the rendered meaning")
	}
}
