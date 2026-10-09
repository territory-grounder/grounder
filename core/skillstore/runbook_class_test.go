package skillstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
)

func runbookStore(t *testing.T) (*MemStore, *audit.Ledger, Version) {
	t.Helper()
	m := NewMemStore()
	m.PutSkill(Skill{Name: "disk-full-runbook", Kind: "catalog", Position: 40, Class: ClassRunbook})
	aw := AppliesWhen{}
	v, err := m.CreateVersion(context.Background(), Version{
		SkillName: "disk-full-runbook", Version: "1.0.0", Body: "## Guest disk full\n\n1. df -h …",
		AppliesWhen: aw, ContentHash: ContentHash("## Guest disk full\n\n1. df -h …", aw),
		Author: "operator:kyriakosp", Source: "console", Rationale: "wiki runbook for guest disk-full",
	})
	if err != nil {
		t.Fatalf("a runbook-class draft is legitimate: %v", err)
	}
	return m, audit.NewLedger(), v
}

// REQ-1316 + TG-476 (ADR-0017): a runbook-class version NEVER enters trial — the refusal binds at the
// TRANSITION chokepoint (the console's closed verb table and any other caller), not only in the
// flywheel's own filtering. Killing mutation: drop the class seam from Transition and this reddens
// (draft -> trial is base-machine legal).
func TestRunbookClassNeverTrials(t *testing.T) {
	m, lg, v := runbookStore(t)
	_, err := Transition(context.Background(), m, lg, v.ID, StatusTrial, "try to A/B the wiki page")
	if !errors.Is(err, ErrClassNotTrialEligible) {
		t.Fatalf("runbook draft -> trial: got %v, want ErrClassNotTrialEligible", err)
	}
	if got, _ := m.GetVersion(context.Background(), v.ID); got.Status != StatusDraft {
		t.Fatalf("a refused trial admission must leave the draft untouched, got %s", got.Status)
	}
}

// TG-476: a runbook draft promotes STRAIGHT to production — trial is the base machine's only road to
// production and runbooks never trial, so without this lane the class could never publish. The
// one-production invariant holds: promoting a second version retires the incumbent (the wiki serves
// exactly one production page per name), and the whole move is ledger-recorded like every transition.
func TestRunbookDraftPromotesDirectly(t *testing.T) {
	ctx := context.Background()
	m, lg, v1 := runbookStore(t)

	p1, err := Transition(ctx, m, lg, v1.ID, StatusProduction, "publish to the wiki")
	if err != nil {
		t.Fatalf("runbook draft -> production must be legal: %v", err)
	}
	if p1.Status != StatusProduction || p1.LedgerSeq == 0 {
		t.Fatalf("promotion must land in production with a ledger seq, got %+v", p1)
	}
	if !strings.Contains(p1.Rationale, "[production] publish to the wiki") {
		t.Fatalf("the promote rationale must append to the version log, got %q", p1.Rationale)
	}

	aw := AppliesWhen{}
	v2, err := m.CreateVersion(ctx, Version{
		SkillName: "disk-full-runbook", Version: "1.1.0", Body: "## Guest disk full (rev)",
		AppliesWhen: aw, ContentHash: ContentHash("## Guest disk full (rev)", aw),
		Author: "operator:kyriakosp", Source: "console", Rationale: "revised steps",
	})
	if err != nil {
		t.Fatalf("second draft: %v", err)
	}
	if _, err := Transition(ctx, m, lg, v2.ID, StatusProduction, "publish the revision"); err != nil {
		t.Fatalf("promoting the revision must supersede, not conflict: %v", err)
	}
	if old, _ := m.GetVersion(ctx, p1.ID); old.Status != StatusRetired {
		t.Fatalf("the incumbent runbook page must retire on supersede, got %s", old.Status)
	}
	if cur, ok, _ := m.ProductionVersion(ctx, "disk-full-runbook"); !ok || cur.ID != v2.ID {
		t.Fatalf("exactly the revision must be the production page, got %+v ok=%v", cur, ok)
	}
}

// The direct-promote lane is RUNBOOK-only: a skill-class draft still earns production through a trial
// (killing mutation: widen the lane's class check and this reddens), and trial admission for the
// eligible classes is untouched.
func TestSkillDraftStillEarnsProductionThroughTrial(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	m.PutSkill(Skill{Name: "triage-protocol", Kind: "behavioral", Position: 5}) // class absent = skill
	aw := AppliesWhen{}
	v, err := m.CreateVersion(ctx, Version{
		SkillName: "triage-protocol", Version: "2.0.0", Body: "b", AppliesWhen: aw,
		ContentHash: ContentHash("b", aw), Author: "operator:test", Source: "hand", Rationale: "r",
	})
	if err != nil {
		t.Fatal(err)
	}
	lg := audit.NewLedger()
	if _, err := Transition(ctx, m, lg, v.ID, StatusProduction, "skip the trial"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("skill draft -> production must stay refused, got %v", err)
	}
	if _, err := Transition(ctx, m, lg, v.ID, StatusTrial, "offline gate passed"); err != nil {
		t.Fatalf("skill draft -> trial must stay legal: %v", err)
	}
}
