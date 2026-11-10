package skillstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
)

func draft(t *testing.T, m *MemStore, name, ver, body string) Version {
	t.Helper()
	aw := AppliesWhen{Phases: []string{"investigate"}, ExecClasses: []string{"STANDARD_AGENT", "DEEP_INVESTIGATION"}}
	v, err := m.CreateVersion(context.Background(), Version{
		SkillName: name, Version: ver, Body: body, AppliesWhen: aw,
		ContentHash: ContentHash(body, aw), Author: "operator:test", Source: "hand",
		Rationale: "authored in test",
	})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	return v
}

func store(t *testing.T) (*MemStore, *audit.Ledger) {
	t.Helper()
	m := NewMemStore()
	m.PutSkill(Skill{Name: "triage-protocol", Kind: "behavioral", Position: 5})
	m.PutSkill(Skill{Name: "conservative-remediation", Kind: "catalog", Pinned: true, Position: 4})
	return m, audit.NewLedger()
}

// REQ-1301: creation requires a rationale, a bounded body, a valid predicate, and a matching hash.
func TestCreateDraftValidation(t *testing.T) {
	m, _ := store(t)
	ctx := context.Background()
	aw := AppliesWhen{Phases: []string{"investigate"}}
	cases := []struct {
		name string
		v    Version
		want error
	}{
		{"no rationale", Version{SkillName: "triage-protocol", Version: "2.0.0", Body: "b", AppliesWhen: aw, ContentHash: ContentHash("b", aw)}, ErrRationaleRequired},
		{"empty body", Version{SkillName: "triage-protocol", Version: "2.0.0", Rationale: "r", AppliesWhen: aw, ContentHash: ContentHash("", aw)}, ErrBodyBounds},
		{"oversized body", Version{SkillName: "triage-protocol", Version: "2.0.0", Rationale: "r", Body: strings.Repeat("x", 9000), AppliesWhen: aw}, ErrBodyBounds},
		{"unknown class", Version{SkillName: "triage-protocol", Version: "2.0.0", Rationale: "r", Body: "b", AppliesWhen: AppliesWhen{ExecClasses: []string{"TURBO"}}}, ErrBadPredicate},
		{"unknown phase", Version{SkillName: "triage-protocol", Version: "2.0.0", Rationale: "r", Body: "b", AppliesWhen: AppliesWhen{Phases: []string{"dream"}}}, ErrBadPredicate},
		{"pinned skill", Version{SkillName: "conservative-remediation", Version: "2.0.0", Rationale: "r", Body: "b", AppliesWhen: aw, ContentHash: ContentHash("b", aw)}, ErrPinnedSkill},
	}
	for _, c := range cases {
		if _, err := m.CreateVersion(ctx, c.v); !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", c.name, c.want, err)
		}
	}
}

// REQ-1301: the transition matrix is exhaustive — everything not explicitly allowed is refused, and a
// terminal state (retired, rejected) has no exit.
func TestTransitionMatrixExhaustive(t *testing.T) {
	all := []Status{StatusDraft, StatusTrial, StatusProduction, StatusRetired, StatusRejected}
	allowed := map[[2]Status]bool{
		{StatusDraft, StatusTrial}:        true,
		{StatusDraft, StatusRejected}:     true,
		{StatusTrial, StatusProduction}:   true,
		{StatusTrial, StatusRejected}:     true,
		{StatusTrial, StatusDraft}:        true,
		{StatusProduction, StatusRetired}: true,
	}
	for _, from := range all {
		for _, to := range all {
			got := transitionAllowed(from, to)
			if got != allowed[[2]Status{from, to}] {
				t.Errorf("transition %s -> %s: allowed=%v, want %v", from, to, got, allowed[[2]Status{from, to}])
			}
		}
	}
}

// REQ-1301: every transition carries a rationale and appends a hash-chained ledger entry.
func TestTransitionLedgersEveryMove(t *testing.T) {
	m, lg := store(t)
	ctx := context.Background()
	v := draft(t, m, "triage-protocol", "2.0.0", "body v2")

	if _, err := Transition(ctx, m, lg, v.ID, StatusTrial, ""); !errors.Is(err, ErrRationaleRequired) {
		t.Fatalf("empty rationale must be refused, got %v", err)
	}
	v2, err := Transition(ctx, m, lg, v.ID, StatusTrial, "offline gate passed: discovery +0.4, regression held")
	if err != nil {
		t.Fatal(err)
	}
	if v2.LedgerSeq == 0 {
		t.Fatal("transition must record its ledger seq")
	}
	if !strings.Contains(v2.Rationale, "[trial]") {
		t.Fatalf("rationale log must grow per transition, got %q", v2.Rationale)
	}
	if err := lg.Verify(); err != nil {
		t.Fatalf("ledger chain must verify: %v", err)
	}
}

// REQ-1302: graduation retires the incumbent in the same logical step; exactly one production remains.
func TestGraduationStructurallySupersedes(t *testing.T) {
	m, lg := store(t)
	ctx := context.Background()
	v1 := draft(t, m, "triage-protocol", "2.0.0", "body v2")
	if _, err := Transition(ctx, m, lg, v1.ID, StatusTrial, "gate passed"); err != nil {
		t.Fatal(err)
	}
	if _, err := Transition(ctx, m, lg, v1.ID, StatusProduction, "welch p=0.03 lift=0.3"); err != nil {
		t.Fatal(err)
	}
	v2 := draft(t, m, "triage-protocol", "3.0.0", "body v3")
	if _, err := Transition(ctx, m, lg, v2.ID, StatusTrial, "gate passed"); err != nil {
		t.Fatal(err)
	}
	if _, err := Transition(ctx, m, lg, v2.ID, StatusProduction, "welch p=0.01 lift=0.4"); err != nil {
		t.Fatal(err)
	}
	var prod, retired int
	for _, v := range m.VersionsOf("triage-protocol") {
		switch v.Status {
		case StatusProduction:
			prod++
		case StatusRetired:
			retired++
		}
	}
	if prod != 1 || retired != 1 {
		t.Fatalf("want exactly 1 production + 1 retired, got %d/%d", prod, retired)
	}
}

// REQ-1301: a rework of a rejected/retired version is a NEW draft (append-only history) — the terminal
// rows themselves cannot transition anywhere.
func TestTerminalStatesHaveNoExit(t *testing.T) {
	m, lg := store(t)
	ctx := context.Background()
	v := draft(t, m, "triage-protocol", "2.0.0", "body")
	if _, err := Transition(ctx, m, lg, v.ID, StatusRejected, "not admitted"); err != nil {
		t.Fatal(err)
	}
	for _, to := range []Status{StatusDraft, StatusTrial, StatusProduction, StatusRetired} {
		if _, err := Transition(ctx, m, lg, v.ID, to, "resurrect"); !errors.Is(err, ErrBadTransition) {
			t.Errorf("rejected -> %s must be refused, got %v", to, err)
		}
	}
}
