package skillstore

import (
	"context"
	"errors"
	"testing"
)

// TG-474 (ADR-0017) — a rubric-class artifact NEVER takes a draft: the embedded judge rubric is the sole
// authority and a store-side edit would be a second rubric the judge never reads. Killing mutations:
// (a) drop the class rule from ValidateDraft — the unpinned-rubric case below still passes the pin check
// and reddens; (b) reorder it after the pin check — same case reddens (the law must not depend on the row's
// pin flag surviving an out-of-band edit).
func TestRubricClassRefusesDrafts(t *testing.T) {
	st := NewMemStore()
	st.PutSkill(Skill{Name: "judge-rubric", Kind: "catalog", Pinned: true, Position: 100, Class: ClassRubric})
	_, err := st.CreateVersion(context.Background(), Version{SkillName: "judge-rubric", Version: "x", Body: "tamper", Rationale: "r"})
	if !errors.Is(err, ErrRubricNeverDrafts) {
		t.Fatalf("pinned rubric draft: got %v, want ErrRubricNeverDrafts", err)
	}
	// The class rule holds even when the pin flag is LOST (out-of-band SQL edit) — the law is the class.
	st2 := NewMemStore()
	st2.PutSkill(Skill{Name: "judge-rubric", Kind: "catalog", Pinned: false, Position: 100, Class: ClassRubric})
	_, err = st2.CreateVersion(context.Background(), Version{SkillName: "judge-rubric", Version: "x", Body: "tamper", Rationale: "r"})
	if !errors.Is(err, ErrRubricNeverDrafts) {
		t.Fatalf("unpinned rubric draft: got %v, want ErrRubricNeverDrafts (the class, not the pin, is the law)", err)
	}
}
