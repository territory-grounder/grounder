package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/skillstore"
	"github.com/territory-grounder/grounder/core/trace"
)

// TG-582: PacksFor returns the version's judged sessions LOWEST on the dimension first, only those with a recorded
// trajectory, as packs carrying the record (steps, evidence, the judge's facts) and no score. DSN-gated.
func TestEvidenceSourceReturnsTheWeakestRecordedSessionsFirst(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	st := NewSkillStore(p)
	if _, err := st.EnsureChain(ctx); err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("tg582-%d", time.Now().UnixNano())
	st.PutSkill(ctx, skillstore.Skill{Name: tag, Kind: "behavioral", Class: skillstore.ClassSkill})
	aw := skillstore.AppliesWhen{}
	v, err := st.CreateVersion(ctx, skillstore.Version{SkillName: tag, Version: "1.0.0", Body: "body", Status: skillstore.StatusDraft,
		AppliesWhen: aw, ContentHash: skillstore.ContentHash("body", aw), Author: "test", Source: "test", Rationale: "[draft] TG-582 fixture"})
	if err != nil {
		t.Fatal(err)
	}
	load := fmt.Sprintf("%s@1.0.0#%d:store", tag, v.ID)
	ts, steps := NewTriageStore(p), NewAgentStepStore(p)
	var mkRow func(ref string, score float64, withSteps bool, loadEntry string, proposed bool, rubric, dim string)
	mkRow = func(ref string, score float64, withSteps bool, loadEntry string, proposed bool, rubric, dim string) {
		t.Helper()
		if err := ts.RecordTriage(ctx, judge.TriageRow{ExternalRef: ref, Host: "h", AlertRule: "R", Band: "ESCALATE", Outcome: "escalated", Proposed: proposed, SkillLoads: []string{loadEntry}}); err != nil {
			t.Fatal(err)
		}
		if err := ts.WriteJudgment(ctx, ref, dim, score, "", rubric); err != nil {
			t.Fatal(err)
		}
		if withSteps {
			if err := steps.Emit(ctx, trace.AgentStep{ExternalRef: ref, Cycle: 1, Tool: "get-device-status", Observation: "down", Outcome: "success"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk := func(ref string, score float64, withSteps bool) {
		mkRow(ref, score, withSteps, load, true, judge.RubricVersion(), "correct_diagnosis")
	}
	mk(tag+"-mid", 3, true)
	mk(tag+"-low", 1, true)
	mk(tag+"-lowest-but-no-trajectory", 1, false)
	mk(tag+"-high", 5, true)
	packs, err := NewEvidenceSource(p).PacksFor(ctx, v.ID, "correct_diagnosis", 7*24*time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 2 || packs[0].Incident.ExternalRef != tag+"-low" || packs[1].Incident.ExternalRef != tag+"-mid" {
		refs := []string{}
		for _, pk := range packs {
			refs = append(refs, pk.Incident.ExternalRef)
		}
		t.Fatalf("want the two weakest sessions WITH a trajectory, lowest first, got %v", refs)
	}
	if len(packs[0].Rollouts) != 1 || len(packs[0].Rollouts[0].Steps) != 1 || packs[0].Harness[tag] != "body" || packs[0].Deliverables[0].Record.Ref != tag+"-low" {
		t.Fatalf("a pack must carry the steps, the harness body and the judge's facts: %+v", packs[0])
	}
	if none, err := NewEvidenceSource(p).PacksFor(ctx, v.ID, "appropriate_band", 7*24*time.Hour, 4); err != nil || len(none) != 0 {
		t.Fatalf("a dimension nobody scored yields no packs (lens fallback), got %d err=%v", len(none), err)
	}
	// THE SAME POOL THE TRIGGER MEASURED: a row judged under another rubric version is not in the pool; a
	// stand-down's falsifiable_prediction score is N/A, not the weakest session.
	mkRow(tag+"-oldrubric", 1, true, load, true, "1999-01-01.0", "correct_diagnosis")
	packs, err = NewEvidenceSource(p).PacksFor(ctx, v.ID, "correct_diagnosis", 7*24*time.Hour, 1)
	if err != nil || len(packs) != 1 || packs[0].Incident.ExternalRef != tag+"-low" {
		t.Fatalf("an old-rubric judgment must never be the weakest pack: %+v err=%v", packs, err)
	}
	mkRow(tag+"-standdown", 1, true, load, false, judge.RubricVersion(), "falsifiable_prediction")
	mkRow(tag+"-proposed-weak", 2, true, load, true, judge.RubricVersion(), "falsifiable_prediction")
	packs, err = NewEvidenceSource(p).PacksFor(ctx, v.ID, "falsifiable_prediction", 7*24*time.Hour, 1)
	if err != nil || len(packs) != 1 || packs[0].Incident.ExternalRef != tag+"-proposed-weak" {
		t.Fatalf("a stand-down's falsifiable_prediction is N/A and must not be the weakest pack: %+v err=%v", packs, err)
	}
}

// flywheelPoolRefs reads the session set the shared pool CTE yields for a version — the trigger's view of the pool,
// through the very constant DimensionMeans prepends — so the parity oracle below can compare SETS, not counts.
func flywheelPoolRefs(t *testing.T, ctx context.Context, p *Pool, versionID int64, window time.Duration) map[string]bool {
	t.Helper()
	rows, err := p.Query(ctx, flywheelPoolCTE+` SELECT external_ref FROM pool`,
		fmt.Sprintf("%%#%d:store%%", versionID), fmt.Sprintf("%d seconds", int(window.Seconds())), judge.RubricVersion(), skillstore.DefaultGenMinPoolSessions)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			t.Fatal(err)
		}
		out[ref] = true
	}
	return out
}

// TG-599: the pack pool IS the trigger's pool. A session that loaded this production body AND some other skill's
// live A/B arm — or this body under its own trial's control label — ran this body; DimensionMeans counts it toward
// the regression mean, so the proposers must be able to read it. The first PacksFor dropped every trial-flagged
// session from the pool while the trigger used that flag only to choose the harness anchor: with three trials live,
// every judged session carried another skill's arm, the pool was empty on every cycle and the proposers never ran.
//
// Three oracles, so no single agreement can carry it: (1) the trigger's sample count on the dimension equals the
// packs; (2) the pack refs equal the pool's refs as a SET; (3) the membership the law demands is asserted by name —
// the other-skill-arm and own-control-label sessions are IN, a session of another harness fingerprint is OUT once an
// anchor exists (DefaultGenMinPoolSessions same-fingerprint sessions), so the anchor branch of the pool CTE runs.
func TestEvidenceSourcePoolIsTheTriggersPool(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	st := NewSkillStore(p)
	if _, err := st.EnsureChain(ctx); err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("tg599-%d", time.Now().UnixNano())
	st.PutSkill(ctx, skillstore.Skill{Name: tag, Kind: "behavioral", Class: skillstore.ClassSkill})
	aw := skillstore.AppliesWhen{}
	v, err := st.CreateVersion(ctx, skillstore.Version{SkillName: tag, Version: "1.0.0", Body: "body", Status: skillstore.StatusDraft,
		AppliesWhen: aw, ContentHash: skillstore.ContentHash("body", aw), Author: "test", Source: "test", Rationale: "[draft] TG-599 fixture"})
	if err != nil {
		t.Fatal(err)
	}
	load := fmt.Sprintf("%s@1.0.0#%d:store", tag, v.ID)
	ts, steps := NewTriageStore(p), NewAgentStepStore(p)
	const fp, otherFP = "fp-anchor-tg599", "fp-other-harness-tg599"
	mk := func(ref string, score float64, harnessFP string, loads ...string) {
		t.Helper()
		if err := ts.RecordTriage(ctx, judge.TriageRow{ExternalRef: ref, Host: "h", AlertRule: "R", Band: "ESCALATE", Outcome: "escalated", Proposed: true, HarnessFP: harnessFP, SkillLoads: loads}); err != nil {
			t.Fatal(err)
		}
		if err := ts.WriteJudgment(ctx, ref, "correct_diagnosis", score, "", judge.RubricVersion()); err != nil {
			t.Fatal(err)
		}
		if err := steps.Emit(ctx, trace.AgentStep{ExternalRef: ref, Cycle: 1, Tool: "get-device-status", Observation: "down", Outcome: "success"}); err != nil {
			t.Fatal(err)
		}
	}
	// Enough same-fingerprint plain sessions for an anchor to exist, so the pool's anchor branch is the one under test.
	for i := 0; i < skillstore.DefaultGenMinPoolSessions; i++ {
		mk(fmt.Sprintf("%s-plain-%02d", tag, i), 3, fp, load)
	}
	mk(tag+"-other-skill-arm", 1, fp, load, "other-skill@2.0.0-cand1#777777:store:trial170/arm0") // the live shape that emptied the pool
	mk(tag+"-own-control-label", 2, fp, load+":trial9/control")                                   // this body under its own trial's control label
	mk(tag+"-other-harness", 1, otherFP, load)                                                    // another configuration: out of the pool (TG-590)
	mk(tag+"-other-harness-arm", 1, otherFP, load, "other-skill@2.0.0-cand1#777777:store:trial170/arm0")
	// The one place the trial flag legitimately acts: a NEWER fingerprint made only of trial-flagged sessions — even
	// more of them than the threshold — must not become the anchor (an A/B arm's harness is not the configuration).
	// Newer, so ORDER BY newest DESC would pick it if the flag were ignored.
	for i := 0; i < skillstore.DefaultGenMinPoolSessions+1; i++ {
		mk(fmt.Sprintf("%s-armfp-%02d", tag, i), 1, "fp-arms-only-tg599", load, "other-skill@2.0.0-cand1#777777:store:trial170/arm0")
	}

	stats, err := st.DimensionMeans(ctx, v.ID, 7*24*time.Hour, skillstore.DefaultGenMinPoolSessions)
	if err != nil {
		t.Fatal(err)
	}
	samples := 0
	for _, s := range stats {
		if s.Dimension == "correct_diagnosis" {
			samples = s.Samples
		}
	}
	want := skillstore.DefaultGenMinPoolSessions + 2
	if samples != want {
		t.Fatalf("the trigger counts the %d anchor-fingerprint sessions (plain, other skill's arm, own control label) and no other, got %d", want, samples)
	}
	packs, err := NewEvidenceSource(p).PacksFor(ctx, v.ID, "correct_diagnosis", 7*24*time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, pk := range packs {
		got[pk.Incident.ExternalRef] = true
	}
	if len(packs) != samples {
		t.Fatalf("PARITY BROKEN: the trigger measured %d judged sessions but the proposers would read %d pack(s) — the pool definitions have drifted apart", samples, len(packs))
	}
	pool := flywheelPoolRefs(t, ctx, p, v.ID, 7*24*time.Hour)
	for ref := range pool {
		if !got[ref] {
			t.Fatalf("PARITY BROKEN: pool session %s is not among the packs", ref)
		}
	}
	for ref := range got {
		if !pool[ref] {
			t.Fatalf("PARITY BROKEN: pack %s is not in the trigger's pool", ref)
		}
	}
	for _, in := range []string{tag + "-other-skill-arm", tag + "-own-control-label", tag + "-plain-00"} {
		if !got[in] {
			t.Fatalf("%s ran this body and must be readable evidence — it is missing from the packs", in)
		}
	}
	for _, out := range []string{tag + "-other-harness", tag + "-other-harness-arm", tag + "-armfp-00", tag + "-armfp-20"} {
		if got[out] {
			t.Fatalf("%s ran under another harness fingerprint and must be outside the anchored pool (an arms-only fingerprint never anchors)", out)
		}
	}
	if packs[0].Incident.ExternalRef != tag+"-other-skill-arm" {
		t.Fatalf("the weakest pooled session (which carries another skill's arm) must come first, got %s", packs[0].Incident.ExternalRef)
	}
}
