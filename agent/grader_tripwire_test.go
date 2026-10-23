package agent

// TG-577 (Harness² learnings H2-3) — THE GRADER-VERDICT TRIPWIRE, system-message half. Harness²
// (core/context.py:assert_no_grader_verdicts) refuses any context that carries the grader's own output, by
// file name AND by content, and its message says: "this is a leak, not a false positive to widen around —
// find the read path that produced it." TG-476 sealed the judge-rubric seed leak by artifact CLASS
// (runbook_never_composes_test.go pins that with sentinels); this oracle renders the REAL model-facing text
// and asserts the REAL rubric's calibration prose is absent from it, so a new read path that bypasses the
// class filter — or a rubric sentence pasted into a prompt file — is red on content, not on label.
//
// Markers are DERIVED from core/judge/rubric.json at test time (never a hand-kept list that drifts):
// the judge's intro sentence, the head of every calibration paragraph, and every scored dimension name.

import (
	"regexp"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/judge"
)

// graderMarkers returns the rubric fragments whose presence in model-facing text IS a grader leak: every
// SENTENCE of at least 40 characters from each of the six calibration fields (so a pasted tail of a long
// paragraph is caught, not only its head — a TG-577 review finding), plus every scored dimension name.
// Each field must contribute at least one marker; a silently empty field is a renamed rubric key, not a
// quieter rubric, and assertEveryRubricFieldContributes says so.
func graderMarkers() []string {
	r := judge.LoadedRubric()
	split := regexp.MustCompile(`[.!?]\s+|\n+`)
	var out []string
	for _, field := range []string{r.Intro, r.ReplyInstruction, r.Guidance, r.HollowProposalRule, r.EstateRule, r.DiagnosisRule} {
		for _, s := range split.Split(field, -1) {
			if s = strings.TrimSpace(s); len(s) >= 40 {
				out = append(out, s)
			}
		}
	}
	out = append(out, r.Dimensions...)
	out = append(out, r.DeterministicDimensions...)
	return out
}

// assertEveryRubricFieldContributes asserts every calibration field contributed at least one marker, so a renamed JSON key
// cannot silently shrink the tripwire.
func assertEveryRubricFieldContributes(t *testing.T) {
	t.Helper()
	r := judge.LoadedRubric()
	split := regexp.MustCompile(`[.!?]\s+|\n+`)
	for name, field := range map[string]string{"intro": r.Intro, "reply_instruction": r.ReplyInstruction, "guidance": r.Guidance, "hollow_proposal_rule": r.HollowProposalRule, "estate_rule": r.EstateRule, "diagnosis_rule": r.DiagnosisRule} {
		n := 0
		for _, s := range split.Split(field, -1) {
			if len(strings.TrimSpace(s)) >= 40 {
				n++
			}
		}
		if n == 0 {
			t.Fatalf("rubric field %s contributed no marker — a renamed or emptied rubric key would silently shrink the tripwire", name)
		}
	}
	if len(r.Dimensions) < 5 {
		t.Fatalf("rubric declares %d scored dimensions, expected at least 5", len(r.Dimensions))
	}
}

func assertNoGraderMarker(t *testing.T, surface, text string) {
	t.Helper()
	for _, m := range graderMarkers() {
		if strings.Contains(text, m) {
			t.Errorf("GRADER-VERDICT TRIPWIRE: rubric fragment %q reached the model-facing %s — this is a LEAK: find the read path that produced it; do not widen or trim the marker list", m, surface)
		}
	}
}

// Every embedded prompt part and every rendered system preamble (real op-class catalog included) is free
// of the judge's calibration prose.
func TestGraderVerdictNeverEntersTheSystemPreamble(t *testing.T) {
	for _, part := range []string{"base-prompt-protocol", "base-prompt-guidance", "base-prompt-empty-opclass"} {
		assertNoGraderMarker(t, "prompt part "+part, promptPart(part))
	}
	guidance, _ := BasePromptGuidance()
	catalog := opschema.Catalog()
	if catalog == "" {
		catalog = emptyOpClassCatalog()
	}
	const fakeTools = "- probe-tool: a fixture tool\n    host (string, required): target"
	for name, text := range map[string]string{
		"preamble(tools, live op-class catalog, embedded guidance)": renderPreamble(fakeTools, catalog, guidance),
		"preamble(no tools, empty catalog, embedded guidance)":      renderPreamble("none", emptyOpClassCatalog(), guidance),
		"op-class catalog": catalog,
	} {
		assertNoGraderMarker(t, name, text)
	}
}

// The tripwire can FIRE on EVERY marker (an oracle that cannot fail is a defect): each marker, injected
// alone into the guidance half, is detected — not only the intro.
func TestGraderTripwireFiresOnEveryLeakedFragment(t *testing.T) {
	assertEveryRubricFieldContributes(t)
	markers := graderMarkers()
	if len(markers) < 20 {
		t.Fatalf("expected at least 20 derived markers (sentences + dimension names), got %d", len(markers))
	}
	for _, m := range markers {
		leaked := renderPreamble("none", emptyOpClassCatalog(), "## Guidance\n"+m+"\n")
		if !strings.Contains(leaked, m) {
			t.Errorf("marker %q injected into the guidance half was not detected in the rendered preamble", m)
		}
	}
}
