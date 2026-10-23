package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/agent/skills"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// THE CENTRAL SAFETY ORACLE for the prose-artifact class model's compose half (TG-476, epic TG-114 C-7;
// spec/014 REQ-1315/1316, ADR-0017): a production row whose artifact class is NOT `skill` must appear
// NOWHERE in the composed agent seed. A runbook is wiki content an operator reads; a rubric is the
// judge's measuring stick; a prompt row is C-3b's separate compose destination. Any of them leaking into
// <behavioral_guidance> would hand the agent prose that was never admitted under the skill cap or the
// skill eval bar — and the runbook path is operator-writable through the console, so the leak would be
// a standing prompt-injection lane from the wiki into every session's instructions.
//
// The oracle drives the REAL worker chain, not a re-implementation: rows come from a REAL
// skillstore.MemStore via its ProductionRows snapshot (the same join shape the pgx store serves the
// worker — cmd/worker/main.go wires skillDB.ProductionRows into Deps.SkillRows), composition runs
// through Activities.composeGuidance → agent/skills.NewFromStore → Registry.Compose, and the final
// message through composeSeed. Red-first: executed against the pre-filter composer this test FAILS
// (the runbook sentinel reached the seed — NewFromStore composed every class, store-only PINNED rows
// included, since REQ-1305's pin rule only guards compiled-name matches).
func TestRunbookNeverComposes(t *testing.T) {
	ctx := context.Background()
	const (
		runbookBody = "RUNBOOK-SENTINEL: 1) df -h on the alerting guest 2) prune images 3) verify ingest resumes"
		rubricBody  = `{"RUBRIC-SENTINEL":"the judge dimensions"}`
		promptBody  = "PROMPT-SENTINEL: base-prompt trialable half"
	)

	// The runbook row is built through the REAL store write path: identity + draft (runbooks draft
	// legitimately — the class is operator-authorable wiki content) and a store-layer promotion to
	// production, the same terminal state the console's promote verb and the boot importer produce.
	st := skillstore.NewMemStore()
	st.PutSkill(skillstore.Skill{Name: "disk-full-runbook", Kind: "catalog", Class: skillstore.ClassRunbook, Position: 40})
	aw := skillstore.AppliesWhen{} // empty predicate = matches EVERY context — the strongest leak vehicle
	draft, err := st.CreateVersion(ctx, skillstore.Version{
		SkillName: "disk-full-runbook", Version: "1.0.0", Body: runbookBody, AppliesWhen: aw,
		ContentHash: skillstore.ContentHash(runbookBody, aw), Author: "operator", Source: "console",
		Rationale: "[draft] wiki runbook for guest disk-full",
	})
	if err != nil {
		t.Fatalf("a runbook-class draft is legitimate and must be storable: %v", err)
	}
	draft.Status = skillstore.StatusProduction
	if err := st.UpdateVersion(ctx, draft); err != nil {
		t.Fatalf("promote runbook to production: %v", err)
	}

	// The rubric mirror (TG-474's boot-seeded shape: PINNED, store-only, class rubric) and a prompt-class
	// row are appended the way ImportCompiledVersion mints them — direct production rows that never passed
	// ValidateDraft (rubric-class drafts are refused, so no draft path can produce this row).
	skillRows := func(ctx context.Context) ([]skillstore.ProductionRow, error) {
		rows, err := st.ProductionRows(ctx)
		if err != nil {
			return nil, err
		}
		rubricAW := skillstore.AppliesWhen{}
		rows = append(rows,
			skillstore.ProductionRow{
				VersionID: 900, SkillName: "judge-rubric", Version: "3", Body: rubricBody, AppliesWhen: rubricAW,
				ContentHash: skillstore.ContentHash(rubricBody, rubricAW), Pinned: true,
				Class: skillstore.ClassRubric, Position: 1000,
			},
			skillstore.ProductionRow{
				VersionID: 901, SkillName: "base-prompt-trialable", Version: "1.0.0", Body: promptBody, AppliesWhen: rubricAW,
				ContentHash: skillstore.ContentHash(promptBody, rubricAW),
				Class:       skillstore.ClassPrompt, Position: 1001,
			})
		return rows, nil
	}

	a := &Activities{D: Deps{SkillRows: skillRows}}
	guidance, record := a.composeGuidance(ctx, "tg-runbook-oracle-1", execclass.DeepInvestigation, skills.DomainUnknown, nil)

	for _, sentinel := range []string{"RUNBOOK-SENTINEL", "RUBRIC-SENTINEL", "PROMPT-SENTINEL"} {
		if strings.Contains(guidance, sentinel) {
			t.Fatalf("%s composed into the behavioral guidance — a non-skill-class production row reached the seed", sentinel)
		}
	}
	// The exclusion must be a FILTER, not a fallback: the compiled library still composes in full, and
	// no load-record line books the excluded artifacts as loaded skills.
	if !strings.Contains(guidance, "HARD FLOOR") {
		t.Fatal("the compiled skill library must still compose when non-skill classes are excluded (filter, not fallback)")
	}
	for _, line := range record {
		if strings.HasPrefix(line, "fallback=") {
			t.Fatalf("non-skill classes must be excluded WITHOUT failing the store path to compiled: %q", line)
		}
		for _, name := range []string{"disk-full-runbook", "judge-rubric", "base-prompt-trialable"} {
			if strings.Contains(line, name) {
				t.Fatalf("the skill_load record must not book an excluded %s row as a loaded skill: %q", name, line)
			}
		}
	}

	// End to end: the FULL composed seed message (preamble + typed blocks) carries no sentinel either —
	// the property holds on the exact bytes the model receives, not just the guidance fragment.
	seed, _ := composeSeed(ingest.IncidentEnvelope{
		ExternalRef: "tg-runbook-oracle-1", AlertRule: "guest-disk-full", Host: "nl-guest-01",
	}, "disk 98% on /", "", "", "", "", "", "", "", guidance)
	for _, sentinel := range []string{"RUNBOOK-SENTINEL", "RUBRIC-SENTINEL", "PROMPT-SENTINEL"} {
		if strings.Contains(seed, sentinel) {
			t.Fatalf("%s reached the composed seed message", sentinel)
		}
	}
	if !strings.Contains(seed, "<behavioral_guidance>") {
		t.Fatal("the seed must still carry the trusted guidance envelope")
	}
}

// A skill-class row with an ABSENT class (every pre-0089 row) still composes — the exclusion is a closed
// per-class law (ADR-0017), not a "class field must be present" accident that would silently strip the
// whole graduated library from the seed on a reader that predates the column.
func TestAbsentClassStillComposesAsSkill(t *testing.T) {
	ctx := context.Background()
	const body = "## Triage protocol v9 (store-graduated)"
	aw := skillstore.AppliesWhen{}
	rows := func(context.Context) ([]skillstore.ProductionRow, error) {
		return []skillstore.ProductionRow{{
			VersionID: 7, SkillName: "triage-protocol", Version: "9.0.0", Body: body,
			AppliesWhen: aw, ContentHash: skillstore.ContentHash(body, aw), Position: 5,
			// Class deliberately absent — DefaultClass reads it as ClassSkill (REQ-1315 back-compat).
		}}, nil
	}
	a := &Activities{D: Deps{SkillRows: rows}}
	guidance, _ := a.composeGuidance(ctx, "tg-runbook-oracle-2", execclass.DeepInvestigation, skills.DomainUnknown, nil)
	if !strings.Contains(guidance, "v9 (store-graduated)") {
		t.Fatal("an absent-class (pre-0089) production row is a skill and must keep composing")
	}
}
