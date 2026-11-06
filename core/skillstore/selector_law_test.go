package skillstore

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
)

// fakeLaw is a CompiledSelectorLaw over a fixed map; equivalence is set-equality of the three lists.
type fakeLaw struct{ compiled map[string]AppliesWhen }

func (f fakeLaw) CompiledPredicate(name string) (AppliesWhen, bool) {
	aw, ok := f.compiled[name]
	return aw, ok
}
func (f fakeLaw) EquivalentPredicates(a, b AppliesWhen) bool {
	eq := func(x, y []string) bool {
		x, y = append([]string{}, x...), append([]string{}, y...)
		sort.Strings(x)
		sort.Strings(y)
		return strings.Join(x, ",") == strings.Join(y, ",")
	}
	return eq(a.Phases, b.Phases) && eq(a.ExecClasses, b.ExecClasses) && eq(a.Domains, b.Domains)
}

func lawStore(t *testing.T) *MemStore {
	t.Helper()
	m := NewMemStore().WithSelectorLaw(fakeLaw{compiled: map[string]AppliesWhen{
		"k8s-triage":  {Domains: []string{"kubernetes"}},
		"exec-safety": {Phases: []string{"execute"}},
	}})
	for _, name := range []string{"k8s-triage", "exec-safety", "house-runbook"} {
		m.PutSkill(Skill{Name: name, Kind: "behavioral", Class: ClassSkill})
	}
	return m
}

func lawDraft(name string, aw AppliesWhen) Version {
	return Version{SkillName: name, Version: "1.0.0", Body: "body", Status: StatusDraft, AppliesWhen: aw,
		ContentHash: ContentHash("body", aw), Author: "operator:test", Source: "hand", Rationale: "[draft] test"}
}

// TG-588: a draft on a COMPILED name is refused unless its predicate is the compiled selector's (equivalence, not
// spelling); a store-only name takes any valid predicate; a store with no law wired enforces nothing (as before).
func TestValidateDraftHoldsCompiledNamesToTheSelectorLaw(t *testing.T) {
	m := lawStore(t)
	ctx := context.Background()
	_, err := m.CreateVersion(ctx, lawDraft("k8s-triage", AppliesWhen{})) // the flattened form a pre-TG-576 import wrote
	if !errors.Is(err, ErrPredicateShadowed) {
		t.Fatalf("a divergent predicate on a compiled name must be refused with ErrPredicateShadowed, got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "domains=[kubernetes]") || !strings.Contains(err.Error(), "k8s-triage") {
		t.Fatalf("the refusal must name the skill and the compiled predicate, got %v", err)
	}
	if _, err := m.CreateVersion(ctx, lawDraft("k8s-triage", AppliesWhen{Domains: []string{"kubernetes"}})); err != nil {
		t.Fatalf("the compiled-derived predicate must be accepted: %v", err)
	}
	if _, err := m.CreateVersion(ctx, lawDraft("exec-safety", AppliesWhen{Phases: []string{"investigate"}})); !errors.Is(err, ErrPredicateShadowed) {
		t.Fatalf("a wrong phase on a compiled name must be refused, got %v", err)
	}
	if _, err := m.CreateVersion(ctx, lawDraft("house-runbook", AppliesWhen{Domains: []string{"proxmox"}})); err != nil {
		t.Fatalf("a store-only name keeps its declarative predicate: %v", err)
	}
	bare := NewMemStore()
	bare.PutSkill(Skill{Name: "k8s-triage", Kind: "behavioral", Class: ClassSkill})
	if _, err := bare.CreateVersion(ctx, lawDraft("k8s-triage", AppliesWhen{})); err != nil {
		t.Fatalf("a store with no law wired must enforce nothing (byte-identical to before): %v", err)
	}
}

// TG-588: a generated candidate for a compiled name carries the compiled-derived predicate, not the production
// row's (flattened) copy; a store-only name keeps the production row's.
func TestGeneratedCandidatesCarryTheCompiledDerivedPredicate(t *testing.T) {
	m := lawStore(t)
	ctx := context.Background()
	// A production row for k8s-triage with the FLATTENED predicate (what a pre-TG-576 import left behind). The
	// law is bypassed for the fixture by writing the row through a bare store sharing no law — the mem store's
	// map is the fixture, so build the row with the law detached and re-attach it.
	law := m.SelectorLaw()
	m.WithSelectorLaw(nil)
	flat, err := m.CreateVersion(ctx, lawDraft("k8s-triage", AppliesWhen{}))
	if err != nil {
		t.Fatal(err)
	}
	flat.Status = StatusProduction
	if err := m.UpdateVersion(ctx, flat); err != nil {
		t.Fatal(err)
	}
	runbook, err := m.CreateVersion(ctx, lawDraft("house-runbook", AppliesWhen{Domains: []string{"proxmox"}}))
	if err != nil {
		t.Fatal(err)
	}
	runbook.Status = StatusProduction
	if err := m.UpdateVersion(ctx, runbook); err != nil {
		t.Fatal(err)
	}
	m.WithSelectorLaw(law)
	gen := &scriptedGen{replies: []string{"rewritten k8s body"}}
	out, err := GenerateCandidates(ctx, m, gen, GenTrigger{SkillName: "k8s-triage", Dimension: "correct_diagnosis", Mean: 2, Threshold: 3.5, Window: 30, Source: "flywheel:eval-failure:run-1"})
	if err != nil || len(out) != 1 {
		t.Fatalf("want one candidate, got %d err=%v", len(out), err)
	}
	if got := out[0].AppliesWhen; len(got.Domains) != 1 || got.Domains[0] != "kubernetes" || out[0].ContentHash != ContentHash("rewritten k8s body", got) {
		t.Fatalf("a k8s-triage candidate must carry Domains=[kubernetes] with a matching content hash, got %+v", out[0])
	}
	gen2 := &scriptedGen{replies: []string{"rewritten runbook body"}}
	out2, err := GenerateCandidates(ctx, m, gen2, GenTrigger{SkillName: "house-runbook", Dimension: "correct_diagnosis", Mean: 2, Threshold: 3.5, Window: 30, Source: "flywheel:eval-failure:run-2"})
	if err != nil || len(out2) != 1 || len(out2[0].AppliesWhen.Domains) != 1 || out2[0].AppliesWhen.Domains[0] != "proxmox" {
		t.Fatalf("a store-only candidate keeps the production row's predicate, got %+v err=%v", out2, err)
	}
}

type nopLedger struct{}

func (nopLedger) Append(d audit.GovDecision) (audit.LedgerEntry, error) {
	return audit.LedgerEntry{}, nil
}

// TG-588 (review): the regression-watch RESTORE is a safety rollback and must never be refused by the write-time
// gate. A prior row written before TG-576 can carry a FLATTENED predicate; the restore carries the lawful one
// (compiled-derived for a compiled name) and lands as production.
func TestRegressionWatchRestoreIsNeverRefusedByTheSelectorLaw(t *testing.T) {
	m := lawStore(t)
	ctx := context.Background()
	law := m.SelectorLaw()
	m.WithSelectorLaw(nil) // the flattened prior is the FIXTURE (a pre-TG-576 row), written with the law detached
	prior, err := m.CreateVersion(ctx, lawDraft("k8s-triage", AppliesWhen{}))
	if err != nil {
		t.Fatal(err)
	}
	prior.Status = StatusRetired // superseded at graduation
	if err := m.UpdateVersion(ctx, prior); err != nil {
		t.Fatal(err)
	}
	m.WithSelectorLaw(law)
	w := WatchState{SkillName: "k8s-triage", VersionID: prior.ID + 1000, PriorVersion: prior.ID}
	if err := restorePrior(ctx, m, nopLedger{}, w); err != nil {
		t.Fatalf("the restore must never be refused by the selector law: %v", err)
	}
	got, ok, err := m.ProductionVersion(ctx, "k8s-triage")
	if err != nil || !ok {
		t.Fatalf("the restore must land as production: ok=%v err=%v", ok, err)
	}
	if got.Body != prior.Body || len(got.AppliesWhen.Domains) != 1 || got.AppliesWhen.Domains[0] != "kubernetes" || got.ContentHash != ContentHash(got.Body, got.AppliesWhen) {
		t.Fatalf("the restore must carry the prior BODY under the lawful predicate with a matching hash, got %+v", got)
	}
}
