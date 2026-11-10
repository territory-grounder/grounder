package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/territory-grounder/grounder/agent/skills"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// TG-576: the boot import's predicate derivation carries Domains and Phases. The pre-fix one-point probe
// wrote {exec_classes:[STANDARD,DEEP]} for k8s-triage and for exec-safety alike; this pins the import's
// OWN call site (importPredicateFor), not a look-alike. Killing mutation (executed, RED): revert
// importPredicateFor to the one-point probe.
func TestImportPredicateIsFaithful(t *testing.T) {
	std, deep := string(execclass.StandardAgent), string(execclass.DeepInvestigation)
	want := map[string]skillstore.AppliesWhen{
		"k8s-triage":        {ExecClasses: []string{std, deep}, Domains: []string{string(skills.DomainKubernetes)}},
		"storage-triage":    {ExecClasses: []string{std, deep}, Domains: []string{string(skills.DomainStorage)}},
		"exec-safety":       {Phases: []string{string(skills.PhaseExecute)}},
		"triage-protocol":   {ExecClasses: []string{std, deep}},
		"proving-your-work": {},
	}
	for _, sk := range skills.Default().All() {
		w, ok := want[sk.Name]
		if !ok {
			continue
		}
		got, _ := importPredicateFor(sk)
		if !reflect.DeepEqual(got, w) {
			t.Errorf("%s: import would write %+v, want %+v", sk.Name, got, w)
		}
	}
}

// recordingImportStore is the oracle's store: the in-memory skillstore for the Store surface Transition
// drives, plus the two boot-import writes recorded by name so the test can read what the import WROTE.
type recordingImportStore struct {
	*skillstore.MemStore
	imported map[string]skillstore.AppliesWhen
}

func (r *recordingImportStore) PutSkill(_ context.Context, sk skillstore.Skill) error {
	r.MemStore.PutSkill(sk)
	return nil
}

func (r *recordingImportStore) ImportCompiledVersion(_ context.Context, name, _ string, _ string, aw skillstore.AppliesWhen) error {
	r.imported[name] = aw
	return nil
}

// TG-576 review: the import LOOP itself must hand importPredicateFor's result to ImportCompiledVersion —
// for every compiled skill, the predicate written equals the derived one. Killing mutation (executed, RED):
// replace `aw, exact := importPredicateFor(sk)` with the empty predicate — k8s-triage loses its Domains.
func TestImportCompiledSkillsWritesTheDerivedPredicates(t *testing.T) {
	st := &recordingImportStore{MemStore: skillstore.NewMemStore(), imported: map[string]skillstore.AppliesWhen{}}
	importCompiledSkills(context.Background(), st, audit.NewLedger())
	for _, sk := range skills.Default().All() {
		want, _ := skills.DeclarativeFor(sk.AppliesWhen)
		got, ok := st.imported[sk.Name]
		if !ok {
			t.Errorf("%s: the boot import wrote no version row", sk.Name)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: import wrote %+v, want the derived %+v", sk.Name, got, want)
		}
	}
	// The two selectors the pre-fix one-point probe destroyed, pinned by VALUE (not by round-trip).
	if got := st.imported["k8s-triage"].Domains; !reflect.DeepEqual(got, []string{string(skills.DomainKubernetes)}) {
		t.Errorf("k8s-triage: import wrote Domains=%v, want [kubernetes]", got)
	}
	if got := st.imported["exec-safety"].Phases; !reflect.DeepEqual(got, []string{string(skills.PhaseExecute)}) {
		t.Errorf("exec-safety: import wrote Phases=%v, want [execute]", got)
	}
}
