package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// TG-585: the pgx lister names ONLY the losers/abortees of TERMINAL trials. Four shapes, all in `trial` status:
//
//	admitted — no skill_trial row at all (AdmitToTrial's "awaiting a start")        → NOT listed
//	carried  — candidate of an ACTIVE trial                                            → NOT listed
//	loser    — candidate of an aborted trial                                            → listed
//	winner   — winner_version_id of a completed trial whose promotion did not happen    → NOT listed
//
// The first shape is the one a plain "no active trial carries it" query would have swept: the flywheel's
// live candidate pool (the TG-585 review). DSN-gated (two-DB recipe); rows are uniquely named and LEFT.
func TestStaleTrialVersionsListsOnlyTerminalTrialLosers(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	st := NewSkillStore(p)
	if _, err := st.EnsureChain(ctx); err != nil {
		t.Fatalf("EnsureChain: %v", err)
	}
	name := fmt.Sprintf("tg585test-skill-%d", time.Now().UnixNano())
	if err := st.PutSkill(ctx, skillstore.Skill{Name: name, Kind: "behavioral", Class: skillstore.ClassSkill}); err != nil {
		t.Fatalf("PutSkill: %v", err)
	}
	mk := func(ver string) int64 {
		aw := skillstore.AppliesWhen{}
		body := "## body " + ver
		v, err := st.CreateVersion(ctx, skillstore.Version{SkillName: name, Version: ver, Status: skillstore.StatusDraft, Body: body,
			AppliesWhen: aw, ContentHash: skillstore.ContentHash(body, aw), Author: "test", Source: "test", Rationale: "[draft] tg585 fixture"})
		if err != nil {
			t.Fatalf("CreateVersion %s: %v", ver, err)
		}
		v.Status = skillstore.StatusTrial
		if err := st.UpdateVersion(ctx, v); err != nil {
			t.Fatalf("UpdateVersion %s: %v", ver, err)
		}
		return v.ID
	}
	admitted, carried, loser, winner := mk("1.0.0-cand1"), mk("1.0.0-cand2"), mk("1.0.0-cand3"), mk("1.0.0-cand4")
	trial := func(cands []int64) skillstore.Trial {
		tr, err := st.CreateTrial(ctx, skillstore.Trial{SkillName: name, CandidateIDs: cands, Dimension: "correct_diagnosis",
			MinSamplesPerArm: 4, MinLift: 0.2, PThreshold: 0.1, EndsAt: time.Now().Add(time.Hour), Status: "active"})
		if err != nil {
			t.Fatalf("CreateTrial: %v", err)
		}
		return tr
	}
	// skill_trial_one_active admits ONE active trial per skill, so the terminal trials are created and ended
	// FIRST and the active one last — the same order production reaches this state in.
	aborted := trial([]int64{loser})
	if err := st.FinalizeTrial(ctx, aborted.ID, "aborted_timeout", 0, 0, 0, "tg585 fixture: timed out"); err != nil {
		t.Fatalf("finalize aborted: %v", err)
	}
	completed := trial([]int64{winner})
	if err := st.FinalizeTrial(ctx, completed.ID, "completed", winner, 4.1, 0.01, "tg585 fixture: winner left un-promoted"); err != nil {
		t.Fatalf("finalize completed: %v", err)
	}
	_ = trial([]int64{carried}) // stays active
	ids, err := st.StaleTrialVersions(ctx)
	if err != nil {
		t.Fatalf("StaleTrialVersions: %v", err)
	}
	has := map[int64]bool{}
	for _, id := range ids {
		has[id] = true
	}
	if !has[loser] {
		t.Errorf("the abortee of a terminal trial must be listed (id %d); got %v", loser, ids)
	}
	for label, id := range map[string]int64{"admitted (no trial row)": admitted, "carried (active trial)": carried, "winner (completed, un-promoted)": winner} {
		if has[id] {
			t.Errorf("%s must NOT be listed (id %d); got %v", label, id, ids)
		}
	}
}
