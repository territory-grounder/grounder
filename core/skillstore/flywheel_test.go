package skillstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
)

type scriptedGen struct{ replies []string }

func (s *scriptedGen) Complete(_ context.Context, _, _ string, _ string) (string, error) {
	if len(s.replies) == 0 {
		return "", errors.New("exhausted")
	}
	r := s.replies[0]
	s.replies = s.replies[1:]
	return r, nil
}

func genStore(t *testing.T) (*MemStore, *audit.Ledger, Version) {
	t.Helper()
	m := NewMemStore()
	m.PutSkill(Skill{Name: "triage-protocol", Kind: "behavioral", Position: 5})
	m.PutSkill(Skill{Name: "conservative-remediation", Kind: "catalog", Pinned: true, Position: 4})
	lg := audit.NewLedger()
	ctx := context.Background()
	v := draft(t, m, "triage-protocol", "1.0.0", "current production body")
	if _, err := Transition(ctx, m, lg, v.ID, StatusTrial, "gate"); err != nil {
		t.Fatal(err)
	}
	v, err := Transition(ctx, m, lg, v.ID, StatusProduction, "initial")
	if err != nil {
		t.Fatal(err)
	}
	return m, lg, v
}

// REQ-1312: generation is draft-only with rationale + source + lineage; duplicates and oversized
// replies are dropped; composition is untouched by drafts.
func TestGenerateCandidatesDraftOnly(t *testing.T) {
	m, _, prod := genStore(t)
	gen := &scriptedGen{replies: []string{
		"rewritten body A",
		"current production body", // paraphrase of production — deduped by hash
		strings.Repeat("x", 9000), // oversized — dropped
	}}
	trig := GenTrigger{SkillName: "triage-protocol", Dimension: "correct_diagnosis",
		Mean: 2.9, Threshold: 3.5, Window: 30, Source: "flywheel:eval-failure:run-7"}
	out, err := GenerateCandidates(context.Background(), m, gen, trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 surviving candidate (dedup + cap dropped 2), got %d", len(out))
	}
	c := out[0]
	if c.Status != StatusDraft || c.Author != "flywheel" || c.ParentVersionID != prod.ID {
		t.Fatalf("candidate must be a lineage-linked flywheel draft, got %+v", c)
	}
	if !strings.Contains(c.Rationale, "correct_diagnosis mean 2.90 fell below 3.50") {
		t.Fatalf("the rationale must state the trigger, got %q", c.Rationale)
	}
	got, _, _ := m.ProductionVersion(context.Background(), "triage-protocol")
	if got.Body != "current production body" {
		t.Fatal("a draft must not touch production")
	}
}

// capturingGen records each prompt so a test can assert the generation FRAMING.
type capturingGen struct {
	replies []string
	prompts []string
}

func (c *capturingGen) Complete(_ context.Context, _, _ string, prompt string) (string, error) {
	c.prompts = append(c.prompts, prompt)
	if len(c.replies) == 0 {
		return "", errors.New("exhausted")
	}
	r := c.replies[0]
	c.replies = c.replies[1:]
	return r, nil
}

// REQ-1312 (the resolved-incident half): a lesson-sourced trigger frames generation as INCORPORATING the
// procedure a resolved incident showed was missing — not improving a weak dimension. Dormant until a producer
// fires a lesson trigger; the eval-failure framing (TestGenerateCandidatesDraftOnly) is byte-identical.
func TestGenerateCandidatesLessonSource(t *testing.T) {
	m, _, prod := genStore(t)
	gen := &capturingGen{replies: []string{"body incorporating the lesson A", "body incorporating the lesson B", "body incorporating the lesson C"}}
	const lesson = "a disk-full incident was resolved by growing the LV; check LV free space before proposing"
	trig := GenTrigger{SkillName: "triage-protocol", Source: "flywheel:lesson:librenms-nl-1", Lesson: lesson}
	out, err := GenerateCandidates(context.Background(), m, gen, trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("a lesson trigger must produce candidates")
	}
	if out[0].ParentVersionID != prod.ID || out[0].Author != "flywheel" || out[0].Source != trig.Source {
		t.Fatalf("candidate must be a lineage-linked flywheel draft carrying the lesson source, got %+v", out[0])
	}
	p := gen.prompts[0]
	if !strings.Contains(p, lesson) || !strings.Contains(p, "INCORPORATE that lesson") {
		t.Fatalf("lesson prompt must present the incident lesson to incorporate, got:\n%s", p)
	}
	if strings.Contains(p, "score weakly on the dimension") {
		t.Fatal("lesson prompt must NOT use the eval-failure dimension framing")
	}
	if !strings.Contains(out[0].Rationale, "resolved incident") {
		t.Fatalf("lesson rationale must state the resolved-incident source, got %q", out[0].Rationale)
	}
}

// Fail-safe: a lesson-sourced trigger with EMPTY Lesson text falls back to the eval-failure framing (never a
// blank prompt) — a producer that fires without distilling a lesson must not degrade to a contentless rewrite.
func TestGenerateCandidatesLessonFallsBackWhenEmpty(t *testing.T) {
	m, _, _ := genStore(t)
	gen := &capturingGen{replies: []string{"body A"}}
	trig := GenTrigger{SkillName: "triage-protocol", Dimension: "correct_diagnosis", Mean: 2.9, Threshold: 3.5, Window: 30, Source: "flywheel:lesson:x", Lesson: "  "}
	if _, err := GenerateCandidates(context.Background(), m, gen, trig); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gen.prompts[0], "score weakly on the dimension") {
		t.Fatal("empty-lesson trigger must fall back to the eval-failure framing")
	}
}

// LessonSource carries a TARGET dimension so a lesson candidate flows through the SAME dimension-keyed admit
// gate + trial as an eval-failure candidate — the fix for the "lesson drafts are un-admittable" gap (a bare
// lesson source made dimensionFromSource return "", so the admit gate skipped the draft). (REQ-1312 lesson half.)
func TestLessonSourceCarriesDimension(t *testing.T) {
	if got := dimensionFromSource(LessonSource("correct_diagnosis", "librenms-nl-9")); got != "correct_diagnosis" {
		t.Fatalf("a lesson source must recover its target dimension for admit/trial, got %q", got)
	}
	if got := dimensionFromSource(GenSource("evidence_grounded", "run-3")); got != "evidence_grounded" {
		t.Fatalf("the eval-failure source must still recover its dimension, got %q", got)
	}
	// A bare/foreign/dimensionless source has NO recoverable dimension → still not admittable (unchanged).
	for _, bare := range []string{"flywheel:lesson:ref-no-dim", "flywheel:eval-failure:nodim", "handwritten", ""} {
		if got := dimensionFromSource(bare); got != "" {
			t.Fatalf("source %q must have no recoverable dimension, got %q", bare, got)
		}
	}
}

// A pinned skill is never a generation target (the floor is not experimentable).
func TestGenerateRefusesPinned(t *testing.T) {
	m, _, _ := genStore(t)
	_, err := GenerateCandidates(context.Background(), m, &scriptedGen{}, GenTrigger{SkillName: "conservative-remediation"})
	if !errors.Is(err, ErrPinnedSkill) {
		t.Fatalf("pinned generation must refuse, got %v", err)
	}
}

type fakeRunner struct{ res OfflineResult }

func (f fakeRunner) RunOffline(context.Context, Version, string) (OfflineResult, error) {
	return f.res, nil
}

// REQ-1307: a passing offline run admits the draft to trial with the scores stored; a regressing run
// keeps it a draft with the refusal stored.
func TestAdmitToTrialGate(t *testing.T) {
	m, lg, _ := genStore(t)
	ctx := context.Background()
	cand := draft(t, m, "triage-protocol", "2.0.0", "candidate body")

	pass := fakeRunner{OfflineResult{RunID: "off-1", RegressionPass: true, DiscoveryDelta: 0.4}}
	v, err := AdmitToTrial(ctx, m, lg, pass, cand.ID, "correct_diagnosis")
	if err != nil || v.Status != StatusTrial {
		t.Fatalf("a passing run must admit, got %v %v", v.Status, err)
	}
	stored, _ := m.GetVersion(ctx, cand.ID)
	var res OfflineResult
	if json.Unmarshal(stored.OfflineEval, &res) != nil || res.RunID != "off-1" {
		t.Fatalf("the offline scores must be stored, got %s", stored.OfflineEval)
	}

	cand2 := draft(t, m, "triage-protocol", "3.0.0", "regressing candidate")
	fail := fakeRunner{OfflineResult{RunID: "off-2", RegressionPass: false, DiscoveryDelta: 0.4}}
	if _, err := AdmitToTrial(ctx, m, lg, fail, cand2.ID, "correct_diagnosis"); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("a regressing run must refuse, got %v", err)
	}
	stored2, _ := m.GetVersion(ctx, cand2.ID)
	if stored2.Status != StatusDraft || stored2.OfflineEval == nil {
		t.Fatalf("a refused draft stays draft with the refusal stored, got %v", stored2.Status)
	}
}

// FlywheelDrafts must return NEVER-SCORED drafts before already-scored (refused) ones, so the bounded
// per-run admit evaluates every candidate at least once instead of re-scoring the oldest refused drafts
// forever and STARVING newer candidates (the live bug: 9 refused drafts blocked 4 never-scored).
func TestFlywheelDraftsNeverScoredFirst(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	m.PutSkill(Skill{Name: "s"})
	mk := func(ver string, scored bool) int64 {
		v := Version{SkillName: "s", Version: ver, Body: "body-" + ver, Rationale: "seed", Author: AuthorFlywheel}
		v.ContentHash = ContentHash(v.Body, v.AppliesWhen)
		got, err := m.CreateVersion(ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		if scored {
			if err := m.SetOfflineEval(ctx, got.ID, json.RawMessage(`{"detail":"refused"}`)); err != nil {
				t.Fatal(err)
			}
		}
		return got.ID
	}
	// creation (id) order: scored, scored, UNSCORED, scored, UNSCORED
	a := mk("1.0.0", true)
	_ = mk("1.0.1", true)
	c := mk("1.0.2", false)
	_ = mk("1.0.3", true)
	e := mk("1.0.4", false)

	drafts, err := m.FlywheelDrafts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(drafts))
	for i, d := range drafts {
		ids[i] = d.ID
	}
	if len(ids) != 5 {
		t.Fatalf("want 5 drafts, got %d", len(ids))
	}
	// never-scored first, oldest-within-group: c, e ...
	if ids[0] != c || ids[1] != e {
		t.Fatalf("never-scored drafts (%d,%d) must come first, got order %v", c, e, ids)
	}
	// ... then the scored ones, oldest-first (a before the rest)
	if ids[2] != a {
		t.Fatalf("scored drafts must follow the unscored, oldest-first; got %v", ids)
	}
}
