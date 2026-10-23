package skillstore

import (
	"context"
	"errors"
	"testing"
)

// TG-475 (REQ-1316) — the flywheel's class gates: an ineligible class (runbook here; rubric is doubly
// walled by ErrRubricNeverDrafts) neither GENERATES against nor ADMITS to trial. Red-first: both tests
// written against the ungated code fail (the generator drafts; admission runs the offline eval). Killing
// mutations: remove either FlywheelEligible guard and its test reddens.

// panicRunner proves admission's class gate sits BEFORE the offline run — an ineligible class must not
// spend an eval to be refused.
type panicRunner struct{}

func (panicRunner) RunOffline(context.Context, Version, string) (OfflineResult, error) {
	panic("the offline runner must not be reached for an ineligible class")
}

func TestGenerateRefusesIneligibleClass(t *testing.T) {
	m, _, _ := genStore(t)
	m.PutSkill(Skill{Name: "dmz-recovery", Kind: "catalog", Position: 50, Class: ClassRunbook})
	if _, err := GenerateCandidates(context.Background(), m, &scriptedGen{}, GenTrigger{SkillName: "dmz-recovery"}); !errors.Is(err, ErrClassNotTrialEligible) {
		t.Fatalf("runbook generation must refuse with the class sentinel, got %v", err)
	}
}

func TestAdmitRefusesIneligibleClassBeforeTheOfflineRun(t *testing.T) {
	m, lg, _ := genStore(t)
	ctx := context.Background()
	m.PutSkill(Skill{Name: "dmz-recovery", Kind: "catalog", Position: 50, Class: ClassRunbook})
	// Runbooks DRAFT legitimately (the wiki promote flow) — the wall is at trial admission.
	v, err := m.CreateVersion(ctx, Version{SkillName: "dmz-recovery", Version: "1.0.0", Body: "recover the DMZ", Rationale: "library content", ContentHash: ContentHash("recover the DMZ", AppliesWhen{})})
	if err != nil {
		t.Fatalf("a runbook draft must be creatable: %v", err)
	}
	if _, err := AdmitToTrial(ctx, m, lg, panicRunner{}, v.ID, "correct_diagnosis"); !errors.Is(err, ErrClassNotTrialEligible) {
		t.Fatalf("runbook admission must refuse with the class sentinel, got %v", err)
	}
	if got, _ := m.GetVersion(ctx, v.ID); got.Status != StatusDraft {
		t.Fatalf("a refused draft must stay a draft, got %s", got.Status)
	}
}
