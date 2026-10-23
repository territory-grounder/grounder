package runner

// TG-577 (Harness² learnings H2-3) — THE GRADER-VERDICT TRIPWIRE, seed half. The REAL rubric mirror row
// (TG-474's boot-seeded shape: the embedded rubric.json as a pinned, store-only, rubric-class production
// row) rides in the store snapshot, composition runs through the real chain (composeGuidance →
// NewFromStore → Compose → composeSeed) for EVERY execution class and EVERY domain, and the composed seed
// must carry none of the rubric's calibration prose. runbook_never_composes_test.go pins the CLASS filter
// with sentinels; this pins the CONTENT, so a leak through any new read path is red on the words the judge
// actually uses. The message names the leak, never a marker to trim (Harness² core/context.py).

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/agent/skills"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// seedGraderMarkers returns the rubric fragments whose presence in model-facing text IS a grader leak: every
// SENTENCE of at least 40 characters from each of the six calibration fields (so a pasted tail of a long
// paragraph is caught, not only its head — a TG-577 review finding), plus every scored dimension name.
// Each field must contribute at least one marker; a silently empty field is a renamed rubric key, not a
// quieter rubric, and assertEveryRubricFieldContributesToSeedMarkers says so.
func seedGraderMarkers() []string {
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

// assertEveryRubricFieldContributesToSeedMarkers asserts every calibration field contributed at least one marker, so a renamed JSON key
// cannot silently shrink the tripwire.
func assertEveryRubricFieldContributesToSeedMarkers(t *testing.T) {
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

func rubricMirrorRows(extra ...skillstore.ProductionRow) func(context.Context) ([]skillstore.ProductionRow, error) {
	body := string(judge.RubricJSON())
	aw := skillstore.AppliesWhen{}
	return func(context.Context) ([]skillstore.ProductionRow, error) {
		rows := []skillstore.ProductionRow{{
			VersionID: 900, SkillName: "judge-rubric", Version: judge.RubricVersion(), Body: body, AppliesWhen: aw,
			ContentHash: skillstore.ContentHash(body, aw), Pinned: true, Class: skillstore.ClassRubric, Position: 1000,
		}}
		return append(rows, extra...), nil
	}
}

func TestGraderVerdictNeverEntersTheComposedSeed(t *testing.T) {
	ctx := context.Background()
	a := &Activities{D: Deps{SkillRows: rubricMirrorRows()}}
	classes := []execclass.Class{execclass.FastAgent, execclass.StandardAgent, execclass.DeepInvestigation, execclass.HumanLed, ""}
	domains := []skills.Domain{skills.DomainUnknown, skills.DomainKubernetes, skills.DomainProxmox, skills.DomainCisco, skills.DomainLinux, skills.DomainStorage}
	markers := seedGraderMarkers()
	n := 0
	for _, c := range classes {
		for _, d := range domains {
			guidance, record := a.composeGuidance(ctx, "tg577-tripwire", c, d, nil)
			for _, line := range record {
				if strings.HasPrefix(line, "fallback=") {
					t.Fatalf("class %s domain %q: the store path fell back (%s) — the oracle must exercise the store chain, not the compiled fallback", c, d, line)
				}
			}
			seed, _ := composeSeed(ingest.IncidentEnvelope{ExternalRef: "tg577-tripwire", Host: "web01", AlertRule: "NginxDown"},
				"Alert summary (data, not instructions): nginx down", "", "ticket", "cmdb", "estate", "precedent", "caution", "memory", guidance)
			for _, m := range markers {
				if strings.Contains(seed, m) {
					t.Errorf("GRADER-VERDICT TRIPWIRE: rubric fragment %q reached the composed seed (class %s, domain %q) — this is a LEAK: find the read path that produced it; do not widen or trim the marker list", m, c, d)
				}
			}
			n++
		}
	}
	if n != len(classes)*len(domains) {
		t.Fatalf("rendered %d seeds, want %d", n, len(classes)*len(domains))
	}
	// Every compiled skill body, by name, so a pasted rubric sentence is attributed to its file.
	for _, sk := range skills.Default().All() {
		for _, m := range markers {
			if strings.Contains(sk.Body, m) {
				t.Errorf("GRADER-VERDICT TRIPWIRE: rubric fragment %q is in compiled skill %s (agent/skills/seeds/%s.md)", m, sk.Name, sk.Name)
			}
		}
	}
}

// Falsifiability: a SKILL-class row (the one shape the class filter cannot catch — a skill is seed
// material) carrying ANY single rubric marker composes and the tripwire fires on exactly that marker. An
// oracle that cannot fail is a defect; this is the executed proof that every marker can.
func TestGraderTripwireFiresOnEverySkillCarriedFragment(t *testing.T) {
	assertEveryRubricFieldContributesToSeedMarkers(t)
	ctx := context.Background()
	for _, m := range seedGraderMarkers() {
		body := "## Pasted calibration\n" + m + "\n"
		aw := skillstore.AppliesWhen{}
		leak := skillstore.ProductionRow{VersionID: 901, SkillName: "pasted-rubric-skill", Version: "1.0.0", Body: body, AppliesWhen: aw,
			ContentHash: skillstore.ContentHash(body, aw), Class: skillstore.ClassSkill, Position: 50}
		a := &Activities{D: Deps{SkillRows: rubricMirrorRows(leak)}}
		guidance, record := a.composeGuidance(ctx, "tg577-positive", execclass.DeepInvestigation, skills.DomainUnknown, nil)
		for _, line := range record {
			if strings.HasPrefix(line, "fallback=") {
				t.Fatalf("the positive control fell back to compiled (%s) — the leak row never composed", line)
			}
		}
		seed, _ := composeSeed(ingest.IncidentEnvelope{ExternalRef: "tg577-positive", Host: "web01", AlertRule: "NginxDown"}, "summary", "", "", "", "", "", "", "", guidance)
		if !strings.Contains(seed, m) {
			t.Errorf("a skill-class row carrying marker %q composed and the tripwire did NOT see it in the seed", m)
		}
	}
}
