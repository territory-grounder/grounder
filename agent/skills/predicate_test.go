package skills

import (
	"reflect"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// TG-576 (Harness² learnings H2-2): the declarative predicate the boot import writes for a compiled skill must
// REPRODUCE the compiled selector over the closed vocabulary — Domains and Phases included. Before this
// oracle the import probed one point (FAST_AGENT) and flattened every selector to {} or the standard/deep
// pair; the five platform playbooks, correlated-triage and exec-safety then composed into every
// standard/deep session (measured 2026-10-22: 399 of 499 sessions each).
//
// KILLING MUTATIONS (executed, RED): make DeclarativeFor drop Domains → k8s-triage expects [kubernetes] and
// fails; make it drop Phases → exec-safety expects [execute] and fails.
func TestDeclarativeForReproducesEveryCompiledSelector(t *testing.T) {
	std, deep := string(execclass.StandardAgent), string(execclass.DeepInvestigation)
	want := map[string]struct {
		aw    skillstore.AppliesWhen
		exact bool
	}{
		"proving-your-work":        {skillstore.AppliesWhen{}, true},
		"loop-red-flags":           {skillstore.AppliesWhen{}, true},
		"conservative-remediation": {skillstore.AppliesWhen{}, true},
		"debugging-protocol":       {skillstore.AppliesWhen{ExecClasses: []string{std, deep}}, true},
		"shortcuts-to-resist":      {skillstore.AppliesWhen{ExecClasses: []string{std, deep}}, true},
		"triage-protocol":          {skillstore.AppliesWhen{ExecClasses: []string{std, deep}}, true},
		"alert-class-playbooks":    {skillstore.AppliesWhen{ExecClasses: []string{std, deep}}, true},
		"k8s-triage":               {skillstore.AppliesWhen{ExecClasses: []string{std, deep}, Domains: []string{string(DomainKubernetes)}}, true},
		"proxmox-triage":           {skillstore.AppliesWhen{ExecClasses: []string{std, deep}, Domains: []string{string(DomainProxmox)}}, true},
		"cisco-triage":             {skillstore.AppliesWhen{ExecClasses: []string{std, deep}, Domains: []string{string(DomainCisco)}}, true},
		"linux-triage":             {skillstore.AppliesWhen{ExecClasses: []string{std, deep}, Domains: []string{string(DomainLinux)}}, true},
		"storage-triage":           {skillstore.AppliesWhen{ExecClasses: []string{std, deep}, Domains: []string{string(DomainStorage)}}, true},
		"exec-safety":              {skillstore.AppliesWhen{Phases: []string{string(PhaseExecute)}}, true},
		// correlated-triage distinguishes the EMPTY class (it refuses an unclassified session; the
		// declarative "empty matches any" rule cannot say that), so its closest form is inexact — recorded,
		// not rounded. The compiled selector decides at compose time regardless (store.go).
		"correlated-triage": {skillstore.AppliesWhen{Phases: []string{string(PhaseInvestigate)}, ExecClasses: []string{deep}}, false},
	}
	seen := map[string]bool{}
	for _, sk := range Default().All() {
		w, ok := want[sk.Name]
		if !ok {
			t.Fatalf("compiled skill %q has no expectation in this table — add it (every compiled selector must be pinned)", sk.Name)
		}
		seen[sk.Name] = true
		got, exact := DeclarativeFor(sk.AppliesWhen)
		if !reflect.DeepEqual(got, w.aw) {
			t.Errorf("%s: derived predicate %+v, want %+v", sk.Name, got, w.aw)
		}
		if exact != w.exact {
			t.Errorf("%s: exact=%v, want %v (divergence: %v)", sk.Name, exact, w.exact, PredicateDivergence(sk.AppliesWhen, got))
		}
		if err := skillstore.ValidatePredicate(got); err != nil {
			t.Errorf("%s: derived predicate must pass the store's write-time vocabulary check: %v", sk.Name, err)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("expectation for %q names no compiled skill", name)
		}
	}
}

// The derived predicate, evaluated through the SAME declarativeApplies the store path uses, agrees with the
// compiled selector on every vocabulary context for every exactly-representable skill — the end-to-end
// statement of parity, independent of the expectation table above.
func TestDerivedPredicateAgreesWithCompiledSelectorEverywhere(t *testing.T) {
	for _, sk := range Default().All() {
		aw, exact := DeclarativeFor(sk.AppliesWhen)
		div := PredicateDivergence(sk.AppliesWhen, aw)
		if exact && len(div) != 0 {
			t.Errorf("%s: exact=true but %d context(s) diverge: %v", sk.Name, len(div), div)
		}
		if !exact && len(div) == 0 {
			t.Errorf("%s: exact=false with no divergence", sk.Name)
		}
		if !exact {
			// Every divergence of an inexact derivation must be on the EMPTY class — the one value the
			// declarative vocabulary cannot distinguish — never on a domain or a phase.
			for _, c := range div {
				if c.ExecClass != "" {
					t.Errorf("%s: diverges on a NAMED class %+v — Domains/Phases were not derived faithfully", sk.Name, c)
				}
			}
		}
	}
}

// The live-bug predicate shape {exec_classes:[STANDARD,DEEP]} for a domain skill DIVERGES from its compiled
// selector on every non-matching domain — the exact contexts on which the flattened row over-composed.
func TestFlattenedPredicateDivergesOnForeignDomains(t *testing.T) {
	var k8s Skill
	for _, sk := range Default().All() {
		if sk.Name == "k8s-triage" {
			k8s = sk
		}
	}
	flat := skillstore.AppliesWhen{ExecClasses: []string{string(execclass.StandardAgent), string(execclass.DeepInvestigation)}}
	div := PredicateDivergence(k8s.AppliesWhen, flat)
	if len(div) == 0 {
		t.Fatal("the flattened predicate must diverge from the compiled k8s selector")
	}
	var foreign int
	for _, c := range div {
		if c.Domain != DomainKubernetes {
			foreign++
		}
		if c.Domain == DomainKubernetes {
			t.Errorf("flattened and compiled must AGREE on the kubernetes domain, diverged at %+v", c)
		}
	}
	if foreign == 0 {
		t.Fatal("divergence must be on the foreign domains (where the flat row over-composes)")
	}
	if aw, _ := DeclarativeFor(k8s.AppliesWhen); strings.Join(aw.Domains, ",") != "kubernetes" {
		t.Fatalf("sanity: k8s-triage derives Domains=%q", aw.Domains)
	}
}
