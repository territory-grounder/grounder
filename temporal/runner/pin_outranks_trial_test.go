package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// TG-218. REQ-1305 (a pinned skill's compiled body can never be overridden) and REQ-1306 (deterministic
// trial arm assignment) met at the composer and nobody had written down which one wins.
//
// The body was never actually at risk: NewFromStore composes the compiled skill for any row with
// Pinned set, whatever applyTrialArms wrote into that row. What leaked was the RECORD. AssignArm
// PERSISTS a skill_trial_assignment row and armNotes stamps the load `trial<N>/arm<K>`, so a session
// that ran the pinned compiled floor was booked into an arm whose candidate body it never saw, and its
// judged score fed that arm's mean through ArmScores.
//
// Measured in the production corpus 2026-08-07, over all 3383 sessions carrying skill_loads — the two
// entries that ever composed a pinned triage-protocol were:
//
//	2026-07-30 13:44:25  triage-protocol@1.3.0:pinned:trial12/arm0
//	2026-07-30 13:44:29  triage-protocol@1.3.0:pinned:trial12/control
//
// and both external_refs hold real skill_trial_assignment rows in trial 12 (variant 0 and -1). The
// compiled floor was scored as if it were the candidate, on both sides of the comparison.
//
// The fix does NOT record the assignment "for interpretability". An arm sample that did not run the
// arm is worse than a missing one: nothing downstream can distinguish them, whereas a missing sample
// is simply a smaller n. The shadowing is made loud in the load record instead.

// pinTrialStore is a TrialStore that counts assignment attempts. The count is the whole point — this
// defect is invisible in the composed text and only shows up in what the trial WROTE.
type pinTrialStore struct {
	*skillstore.MemTrialStore
	assigns int
}

func (s *pinTrialStore) Assign(ctx context.Context, ref string, trialID int64, variant int) (int, error) {
	s.assigns++
	return s.MemTrialStore.Assign(ctx, ref, trialID, variant)
}

// pinTrialFixture wires one active trial on skillName with one candidate version, plus the production
// row set the composer sees. pinnedRow controls the pin on that production row.
func pinTrialFixture(t *testing.T, skillName string, pinnedRow bool) (*Activities, *pinTrialStore, []skillstore.ProductionRow) {
	t.Helper()

	mem := skillstore.NewMemTrialStore(100)
	st := &pinTrialStore{MemTrialStore: mem}
	tr, err := mem.CreateTrial(context.Background(), skillstore.Trial{
		SkillName: skillName, CandidateIDs: []int64{33}, ControlVersionID: 21,
		Dimension: "falsifiable_prediction", MinSamplesPerArm: 5, MinLift: 0.1, PThreshold: 0.05,
		EndsAt: time.Now().Add(72 * time.Hour), Status: "active",
	})
	if err != nil {
		t.Fatalf("fixture: create trial: %v", err)
	}
	if tr.ID == 0 {
		t.Fatal("fixture: trial got no id")
	}

	candBody := "CANDIDATE BODY — the arm under test"
	cand := skillstore.Version{
		ID: 33, SkillName: skillName, Version: "1.1.0-cand3-cand1", Status: skillstore.StatusTrial,
		Body: candBody, ContentHash: skillstore.ContentHash(candBody, skillstore.AppliesWhen{}),
	}
	a := &Activities{D: Deps{
		SkillTrials: st,
		SkillVersionByID: func(_ context.Context, id int64) (skillstore.Version, error) {
			if id != 33 {
				t.Errorf("fixture asked for unexpected version id %d", id)
			}
			return cand, nil
		},
	}}

	prodBody := "PRODUCTION BODY — the control"
	rows := []skillstore.ProductionRow{{
		VersionID: 21, SkillName: skillName, Version: "1.1.0-cand3", Body: prodBody,
		ContentHash: skillstore.ContentHash(prodBody, skillstore.AppliesWhen{}),
		Pinned:      pinnedRow, Position: 5,
	}}
	return a, st, rows
}

// refThatDrawsTheCandidateArm finds an external_ref whose deterministic assignment lands on arm 0
// rather than control. Hard-coding a ref would make the unpinned control case below pass for the wrong
// reason — "no swap happened" is the assertion, and a ref that drew control never swaps either.
func refThatDrawsTheCandidateArm(t *testing.T, tr skillstore.Trial) string {
	t.Helper()
	probe := skillstore.NewMemTrialStore(100)
	for i := 0; i < 200; i++ {
		ref := "tg-probe-" + strings.Repeat("x", i%7) + "-" + time.Unix(int64(i), 0).UTC().Format("150405")
		arm, err := skillstore.AssignArm(context.Background(), probe, ref, tr)
		if err != nil {
			continue
		}
		if arm == 0 {
			return ref
		}
	}
	t.Fatal("no probe ref drew the candidate arm in 200 tries — the assignment hash is not distributing " +
		"and every assertion below about a swap would be vacuous")
	return ""
}

// TestAPinnedSkillTakesNoTrialAssignment is the finding.
func TestAPinnedSkillTakesNoTrialAssignment(t *testing.T) {
	a, st, rows := pinTrialFixture(t, "triage-protocol", true)
	trials, err := a.D.SkillTrials.ActiveTrials(context.Background())
	if err != nil || len(trials) != 1 {
		t.Fatalf("fixture: want 1 active trial, got %d (%v)", len(trials), err)
	}
	ref := refThatDrawsTheCandidateArm(t, trials[0])

	notes := map[string]string{}
	out := a.applyTrialArms(context.Background(), ref, rows, notes)

	if st.assigns != 0 {
		t.Errorf("the composer took %d trial assignment(s) for a PINNED skill. The pinned compiled body "+
			"is what composes, so every one of those rows books a session into an arm it never ran and "+
			"feeds that arm's mean through ArmScores — which is how trial 12 scored the compiled floor as "+
			"both its candidate and its control on 2026-07-30.", st.assigns)
	}
	if out[0].Body != rows[0].Body {
		t.Errorf("the candidate body was swapped into a pinned row: %q", out[0].Body)
	}
	note := notes["triage-protocol"]
	if strings.Contains(note, "arm") {
		t.Errorf("the load record says %q — that is indistinguishable from a session that genuinely ran "+
			"the arm. The shadowing has to be legible in the provenance or a pin set mid-trial is silent.", note)
	}
	if !strings.Contains(note, "pinned") {
		t.Errorf("the load record for a shadowed trial is %q — it names neither the trial nor the pin, so "+
			"nothing downstream can tell this trial lost samples to the pin", note)
	}
}

// TestAnUnpinnedTrialStillAssignsAndSwaps is the vacuity floor. The cheapest way to pass the test above
// is to stop applying trial arms at all, which would silently end every experiment in the system.
func TestAnUnpinnedTrialStillAssignsAndSwaps(t *testing.T) {
	a, st, rows := pinTrialFixture(t, "triage-protocol", false)
	trials, err := a.D.SkillTrials.ActiveTrials(context.Background())
	if err != nil || len(trials) != 1 {
		t.Fatalf("fixture: want 1 active trial, got %d (%v)", len(trials), err)
	}
	ref := refThatDrawsTheCandidateArm(t, trials[0])

	notes := map[string]string{}
	out := a.applyTrialArms(context.Background(), ref, rows, notes)

	if st.assigns != 1 {
		t.Fatalf("an UNPINNED trial took %d assignments, want 1 — the arm machinery is off, so the test "+
			"above proves nothing", st.assigns)
	}
	if out[0].VersionID != 33 || !strings.Contains(out[0].Body, "CANDIDATE BODY") {
		t.Errorf("the candidate arm did not compose for an unpinned skill: version %d body %q",
			out[0].VersionID, out[0].Body)
	}
	if note := notes["triage-protocol"]; !strings.Contains(note, "arm0") {
		t.Errorf("the unpinned load record is %q, want it to name arm0 — the real experiment must stay "+
			"attributable", note)
	}
}

// TestStartTrialRefusesAPinnedSkill closes the other door. CreationDeps.start already skips pinned
// skills, but that is ONE caller's check and the engine entry point was open: the console, a backfill
// or any future opener could arm a trial on the floor. The refusal belongs at StartTrial.
func TestStartTrialRefusesAPinnedSkill(t *testing.T) {
	st := skillstore.NewMemTrialStore(100)
	st.SetPinned("conservative-remediation", true)

	tr := skillstore.Trial{
		SkillName: "conservative-remediation", CandidateIDs: []int64{7}, Dimension: "appropriate_band",
		MinSamplesPerArm: 5, EndsAt: time.Now().Add(72 * time.Hour),
	}
	if _, err := skillstore.StartTrial(context.Background(), st, tr, time.Now()); err == nil {
		t.Fatal("StartTrial opened a trial on a PINNED skill. The floor is not experimentable (REQ-1305): " +
			"every session composes the compiled body regardless, so the trial can only ever accumulate " +
			"samples that did not run its arms.")
	} else if !strings.Contains(err.Error(), "pinned") {
		t.Errorf("StartTrial refused the pinned skill with %v — the caller cannot tell this apart from the "+
			"traffic refusal, which is retried next run", err)
	}

	// Vacuity floor: the same trial must open once the pin is off, or the refusal above is just a broken
	// StartTrial.
	st.SetPinned("conservative-remediation", false)
	if _, err := skillstore.StartTrial(context.Background(), st, tr, time.Now()); err != nil {
		t.Fatalf("StartTrial refused an UNPINNED trial too (%v) — the pin assertion above proves nothing", err)
	}
}
