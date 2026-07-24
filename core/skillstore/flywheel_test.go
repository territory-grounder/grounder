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
