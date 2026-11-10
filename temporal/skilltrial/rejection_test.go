package skilltrial

// TG-585 (Harness² learnings H2-V2): FinalizeActivity's own comment promised "losing candidates are
// transitioned to rejected" and nothing did it — on prod 19 skill_version rows sat in `trial` from
// 2026-07-18 on. These oracles pin the two halves of the fix: every candidate of a trial that ENDS this pass
// (timeout, no winner, or lost to the graduate) leaves `trial` through the audited Transition, and any row
// still stranded in `trial` with no active trial is reconciled on the next pass.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// seedVersions registers the skill, one production control and n candidates in `trial` status.
func seedVersions(t *testing.T, st *skillstore.MemStore, n int) (control int64, cands []int64) {
	t.Helper()
	ctx := context.Background()
	st.PutSkill(skillstore.Skill{Name: "triage-protocol", Kind: "behavioral", Class: skillstore.ClassSkill})
	mk := func(ver string, status skillstore.Status) int64 {
		aw := skillstore.AppliesWhen{}
		body := "## body " + ver
		v, err := st.CreateVersion(ctx, skillstore.Version{SkillName: "triage-protocol", Version: ver, Status: status, Body: body,
			AppliesWhen: aw, ContentHash: skillstore.ContentHash(body, aw), Author: "test", Source: "test", Rationale: "[" + string(status) + "] seeded"})
		if err != nil {
			t.Fatalf("seed %s: %v", ver, err)
		}
		if v.Status != status { // the fake may normalise a created row to draft; the oracle needs the real state
			v.Status = status
			if err := st.UpdateVersion(ctx, v); err != nil {
				t.Fatalf("set status %s: %v", status, err)
			}
		}
		return v.ID
	}
	control = mk("1.0.0", skillstore.StatusProduction)
	for i := 0; i < n; i++ {
		cands = append(cands, mk(fmt.Sprintf("1.1.0-cand%d", i+1), skillstore.StatusTrial))
	}
	return control, cands
}

func statusOf(t *testing.T, st *skillstore.MemStore, id int64) skillstore.Status {
	t.Helper()
	v, err := st.GetVersion(context.Background(), id)
	if err != nil {
		t.Fatalf("get version %d: %v", id, err)
	}
	return v.Status
}

// A trial swept by the timeout leaves NO candidate in trial status.
// KILLING MUTATION (executed, RED): drop the rejectCandidates call from the aborted_timeout arm.
func TestAbortedTrialRejectsEveryCandidate(t *testing.T) {
	now := time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)
	store := skillstore.NewMemStore()
	control, cands := seedVersions(t, store, 2)
	trials := skillstore.NewMemTrialStore(10)
	if _, err := trials.CreateTrial(context.Background(), skillstore.Trial{SkillName: "triage-protocol", CandidateIDs: cands, ControlVersionID: control,
		Dimension: "correct_diagnosis", MinSamplesPerArm: 4, MinLift: 0.2, PThreshold: 0.1, EndsAt: now.Add(-time.Hour), Status: "active"}); err != nil {
		t.Fatal(err)
	}
	acts := &Activities{D: Deps{Trials: trials, Store: store, Ledger: audit.NewLedger()}}
	out, err := acts.FinalizeActivity(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if out.Swept != 1 || out.Rejected != 2 || len(out.Errors) != 0 {
		t.Fatalf("want 1 swept, 2 rejected, no errors; got %+v", out)
	}
	for _, c := range cands {
		if got := statusOf(t, store, c); got != skillstore.StatusRejected {
			t.Errorf("candidate %d after a timeout sweep: status %s, want rejected", c, got)
		}
	}
	if got := statusOf(t, store, control); got != skillstore.StatusProduction {
		t.Errorf("the control must be untouched by a sweep, got %s", got)
	}
}

// A graduation promotes the winner, retires the incumbent (unchanged behaviour) and REJECTS the losers.
// KILLING MUTATION (executed, RED): drop the rejectCandidates call from the completed arm.
func TestGraduationRejectsTheLosers(t *testing.T) {
	now := time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)
	store := skillstore.NewMemStore()
	control, cands := seedVersions(t, store, 2)
	trials := skillstore.NewMemTrialStore(10)
	tr, err := trials.CreateTrial(context.Background(), skillstore.Trial{SkillName: "triage-protocol", CandidateIDs: cands, ControlVersionID: control,
		Dimension: "correct_diagnosis", MinSamplesPerArm: 4, MinLift: 0.2, PThreshold: 0.1, EndsAt: now.Add(24 * time.Hour), Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	trials.SetScores(tr.ID, map[int][]float64{
		-1: {3.0, 3.1, 2.9, 3.2}, // control
		0:  {4.0, 4.1, 3.9, 4.2}, // cand1 — clears lift 0.2 with a tight spread (one-sided Welch p << 0.1)
		1:  {3.0, 3.0, 3.1, 2.9}, // cand2 — no lift
	})
	acts := &Activities{D: Deps{Trials: trials, Store: store, Ledger: audit.NewLedger()}}
	out, err := acts.FinalizeActivity(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if out.Graduated != 1 || out.Rejected != 1 || len(out.Errors) != 0 {
		t.Fatalf("want 1 graduated, 1 rejected, no errors; got %+v", out)
	}
	if got := statusOf(t, store, cands[0]); got != skillstore.StatusProduction {
		t.Errorf("the winner must be production, got %s", got)
	}
	if got := statusOf(t, store, cands[1]); got != skillstore.StatusRejected {
		t.Errorf("the loser must be rejected, got %s", got)
	}
	if got := statusOf(t, store, control); got != skillstore.StatusRetired {
		t.Errorf("the incumbent must be retired by the graduation (unchanged behaviour), got %s", got)
	}
}

type fakeStale struct{ ids []int64 }

func (f fakeStale) StaleTrialVersions(context.Context) ([]int64, error) { return f.ids, nil }

// The finalizer's half of reconciliation: whatever the lister names (the pgx query is the law — see the
// DB-gated TestStaleTrialVersionsListsOnlyTerminalTrialLosers) is moved to rejected through Transition,
// and a row the state machine refuses (a production version) is RECORDED, never moved. The fixture is a
// genuine loser shape: a candidate of a trial that already ended by timeout.
// KILLING MUTATION (executed, RED): skip the reconciliation block when StaleTrials is set.
func TestStrandedTrialVersionsAreReconciled(t *testing.T) {
	now := time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)
	store := skillstore.NewMemStore()
	control, cands := seedVersions(t, store, 1)
	trials := skillstore.NewMemTrialStore(10)
	tr, err := trials.CreateTrial(context.Background(), skillstore.Trial{SkillName: "triage-protocol", CandidateIDs: cands,
		Dimension: "correct_diagnosis", MinSamplesPerArm: 4, EndsAt: now.Add(-48 * time.Hour), Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if err := trials.FinalizeTrial(context.Background(), tr.ID, "aborted_timeout", 0, 0, 0, "ended before this fix existed"); err != nil {
		t.Fatal(err)
	}
	acts := &Activities{D: Deps{Trials: trials, Store: store, Ledger: audit.NewLedger(),
		StaleTrials: fakeStale{ids: []int64{cands[0], control}}}}
	out, err := acts.FinalizeActivity(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if out.Reconciled != 1 {
		t.Fatalf("want exactly the abortee reconciled, got %+v", out)
	}
	if got := statusOf(t, store, cands[0]); got != skillstore.StatusRejected {
		t.Errorf("abortee of a terminal trial: status %s, want rejected", got)
	}
	if got := statusOf(t, store, control); got != skillstore.StatusProduction {
		t.Errorf("a production row must never be moved by reconciliation, got %s", got)
	}
	if len(out.Errors) != 1 || !strings.Contains(out.Errors[0], "reconcile stranded version") {
		t.Fatalf("the refused production row must be recorded as exactly one error, got %v", out.Errors)
	}
}

// An ADMITTED candidate awaiting its start — `trial` status, no skill_trial row yet (AdmitToTrial's state) —
// is the flywheel's live pool and must survive a finalize pass untouched. With the real lister this is the
// pgx query's law (DB-gated oracle); here the pass runs with no stale list and the per-outcome arm must
// have nothing to say about a version no outcome names.
func TestAdmittedCandidatesAwaitingAStartAreNeverRejected(t *testing.T) {
	now := time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)
	store := skillstore.NewMemStore()
	_, cands := seedVersions(t, store, 2) // both admitted: trial status, no trial row anywhere
	acts := &Activities{D: Deps{Trials: skillstore.NewMemTrialStore(10), Store: store, Ledger: audit.NewLedger(),
		StaleTrials: fakeStale{}}}
	out, err := acts.FinalizeActivity(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if out.Rejected != 0 || out.Reconciled != 0 || len(out.Errors) != 0 {
		t.Fatalf("an admitted pool must be untouched, got %+v", out)
	}
	for _, c := range cands {
		if got := statusOf(t, store, c); got != skillstore.StatusTrial {
			t.Errorf("admitted candidate %d: status %s, want trial (awaiting a start)", c, got)
		}
	}
}

// A graduation whose winner cannot be promoted never orphans the winner: the per-outcome arm rejects nothing,
// the winner stays in `trial` for the operator promote route, and the reconciliation sweep — wired here with
// a lister that mirrors the pgx law (the loser of the now-completed trial is listed, the recorded winner is
// not) — rejects the loser in the SAME pass. Before the review's fix the losers were rejected BEFORE the
// promotion attempt, which would have left a failed winner alone in a completed trial.
func TestFailedGraduationLeavesTheTrialWholeForTheOperator(t *testing.T) {
	now := time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)
	store := skillstore.NewMemStore()
	control, cands := seedVersions(t, store, 2)
	// Make the would-be winner unpromotable: an operator moved it to draft meanwhile (draft -> production is refused).
	v, err := store.GetVersion(context.Background(), cands[0])
	if err != nil {
		t.Fatal(err)
	}
	v.Status = skillstore.StatusDraft
	if err := store.UpdateVersion(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	trials := skillstore.NewMemTrialStore(10)
	tr, err := trials.CreateTrial(context.Background(), skillstore.Trial{SkillName: "triage-protocol", CandidateIDs: cands, ControlVersionID: control,
		Dimension: "correct_diagnosis", MinSamplesPerArm: 4, MinLift: 0.2, PThreshold: 0.1, EndsAt: now.Add(24 * time.Hour), Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	trials.SetScores(tr.ID, map[int][]float64{-1: {3.0, 3.1, 2.9, 3.2}, 0: {4.0, 4.1, 3.9, 4.2}, 1: {3.0, 3.0, 3.1, 2.9}})
	// The lister mirrors the pgx law for this state: cands[1] is carried by a terminal (completed) trial and
	// is not its winner; cands[0] IS the recorded winner and is never named.
	acts := &Activities{D: Deps{Trials: trials, Store: store, Ledger: audit.NewLedger(), StaleTrials: fakeStale{ids: []int64{cands[1]}}}}
	out, err := acts.FinalizeActivity(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if out.Graduated != 0 || out.Rejected != 0 || out.Reconciled != 1 || len(out.Errors) != 1 {
		t.Fatalf("a refused graduation must graduate nothing, reject nothing from its own arm, reconcile the loser, and record one error; got %+v", out)
	}
	if got := statusOf(t, store, cands[0]); got != skillstore.StatusDraft {
		t.Errorf("the un-promotable winner must be left exactly as it was for the operator, got %s", got)
	}
	if got := statusOf(t, store, cands[1]); got != skillstore.StatusRejected {
		t.Errorf("the loser must be reconciled to rejected in the same pass, got %s", got)
	}
	if got := statusOf(t, store, control); got != skillstore.StatusProduction {
		t.Errorf("the incumbent must be untouched by a refused graduation, got %s", got)
	}
}

// The no-DB oracle path wires no ledger: the pass must RECORD that it could not reject (so the candidates
// surface for reconciliation) rather than panic inside Transition.
func TestNoLedgerIsRecordedNotPanicked(t *testing.T) {
	now := time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)
	store := skillstore.NewMemStore()
	_, cands := seedVersions(t, store, 1)
	trials := skillstore.NewMemTrialStore(10)
	if _, err := trials.CreateTrial(context.Background(), skillstore.Trial{SkillName: "triage-protocol", CandidateIDs: cands,
		Dimension: "correct_diagnosis", MinSamplesPerArm: 4, EndsAt: now.Add(-time.Hour), Status: "active"}); err != nil {
		t.Fatal(err)
	}
	acts := &Activities{D: Deps{Trials: trials, Store: store}}
	out, err := acts.FinalizeActivity(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if out.Rejected != 0 || len(out.Errors) != 1 || !strings.Contains(out.Errors[0], "no ledger") {
		t.Fatalf("want 0 rejected and one 'no ledger' error, got %+v", out)
	}
	if got := statusOf(t, store, cands[0]); got != skillstore.StatusTrial {
		t.Errorf("without a ledger the candidate must be left for reconciliation, got %s", got)
	}
}
