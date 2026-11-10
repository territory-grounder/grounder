package skills

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// TG-588: the compiled registry's law answers for compiled names with the selector's declarative form and for no
// one else; equivalence is the exhaustive one (DeclarativeEquivalent), so spelling never matters.
func TestSelectorLawAnswersForCompiledNamesOnly(t *testing.T) {
	law := SelectorLaw()
	k8s, ok := law.CompiledPredicate("k8s-triage")
	if !ok || len(k8s.Domains) != 1 || k8s.Domains[0] != "kubernetes" {
		t.Fatalf("k8s-triage must derive Domains=[kubernetes], got %+v ok=%v", k8s, ok)
	}
	exec, ok := law.CompiledPredicate("exec-safety")
	if !ok || len(exec.Phases) != 1 || exec.Phases[0] != "execute" {
		t.Fatalf("exec-safety must derive Phases=[execute], got %+v ok=%v", exec, ok)
	}
	if _, ok := law.CompiledPredicate("house-runbook-that-is-not-compiled"); ok {
		t.Fatal("a store-only name must not be claimed by the law")
	}
	if !law.EquivalentPredicates(k8s, k8s) || law.EquivalentPredicates(k8s, exec) {
		t.Fatal("equivalence must be the exhaustive declarative one")
	}
}

// Every compiled name's derived predicate passes the write-time gate with the REAL law wired — including the
// exact=false case (correlated-triage), whose closest form is what the boot import writes. A fake law proving the
// gate's mechanics is not this: this is the registry against itself.
func TestEveryCompiledNameDraftsItsOwnDerivedPredicate(t *testing.T) {
	ctx := context.Background()
	law := SelectorLaw()
	m := skillstore.NewMemStore().WithSelectorLaw(law)
	for _, sk := range Default().All() {
		m.PutSkill(skillstore.Skill{Name: sk.Name, Kind: "behavioral", Class: skillstore.ClassSkill})
		aw, ok := law.CompiledPredicate(sk.Name)
		if !ok {
			t.Fatalf("%s: the law must know every compiled name", sk.Name)
		}
		if _, err := m.CreateVersion(ctx, skillstore.Version{SkillName: sk.Name, Version: "9.9.9", Body: "body", Status: skillstore.StatusDraft,
			AppliesWhen: aw, ContentHash: skillstore.ContentHash("body", aw), Author: "test", Source: "test", Rationale: "[draft] round trip"}); err != nil {
			t.Fatalf("%s: its own derived predicate %+v must pass the gate: %v", sk.Name, aw, err)
		}
		if _, err := m.CreateVersion(ctx, skillstore.Version{SkillName: sk.Name, Version: "9.9.10", Body: "body2", Status: skillstore.StatusDraft,
			AppliesWhen: skillstore.AppliesWhen{Domains: []string{"never-a-domain"}}, ContentHash: skillstore.ContentHash("body2", skillstore.AppliesWhen{Domains: []string{"never-a-domain"}}),
			Author: "test", Source: "test", Rationale: "[draft] divergent"}); err == nil {
			t.Fatalf("%s: a divergent predicate must be refused", sk.Name)
		}
	}
}
