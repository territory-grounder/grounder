package skills

import (
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// This file is the bridge between the two forms a skill selector takes (TG-576, Harness² learnings H2-2):
//
//   - the COMPILED selector — a pure Go func over Context (skills.go), authoritative for every compiled skill;
//   - the DECLARATIVE predicate — skillstore.AppliesWhen, the closed-vocabulary form a store row carries
//     for the console/library and for flywheel candidates to inherit (declarativeApplies in store.go).
//
// Before TG-576 the boot import derived the declarative form by probing the compiled func at ONE point
// (FAST_AGENT) and so flattened every selector to {} or {exec_classes:[STANDARD,DEEP]}: the five domain
// skills, correlated-triage and exec-safety all lost their Domains / Phases, and because the store row's
// predicate then won at compose time, every standard/deep session composed every platform playbook
// (measured 2026-10-22: each loaded on 399 of 499 sessions in 14 days). The vocabulary below is the
// closed product the declarative form can speak; equivalence is decided by EXHAUSTIVE evaluation over it,
// never by inspecting the func (INV-08 — a selector is data to evaluate, not code to interpret).

// predicateClasses includes the EMPTY class: an unclassified session is a real production value (classFor
// falls back to it) and the declarative form's "empty matches any" rule is defined on it.
var predicateClasses = []execclass.Class{"", execclass.Deterministic, execclass.FastAgent, execclass.StandardAgent, execclass.DeepInvestigation, execclass.HumanLed}

// predicateNamedClasses are the classes a declarative ExecClasses list may name (the non-empty ones).
var predicateNamedClasses = predicateClasses[1:]

// predicatePhases: every composer sets a phase (the Runner composes PhaseInvestigate, eval/execphase
// composes PhaseExecute), so the empty phase is not in the vocabulary.
var predicatePhases = []Phase{PhaseInvestigate, PhaseExecute}

// predicateDomains includes DomainUnknown (""): the most common production value, and the one the STRICT
// domain rule is defined on.
var predicateDomains = []Domain{DomainUnknown, DomainKubernetes, DomainProxmox, DomainCisco, DomainLinux, DomainStorage}

// PredicateContexts is the full closed vocabulary a selector is compared over: every (phase, class, domain).
func PredicateContexts() []Context {
	out := make([]Context, 0, len(predicatePhases)*len(predicateClasses)*len(predicateDomains))
	for _, p := range predicatePhases {
		for _, c := range predicateClasses {
			for _, d := range predicateDomains {
				out = append(out, Context{Phase: p, ExecClass: c, Domain: d})
			}
		}
	}
	return out
}

// PredicateDivergence returns every vocabulary context on which the compiled selector and the declarative
// predicate DISAGREE; nil means they are equivalent. A nil selector never applies.
func PredicateDivergence(fn func(Context) bool, aw skillstore.AppliesWhen) []Context {
	decl := declarativeApplies(aw)
	var out []Context
	for _, ctx := range PredicateContexts() {
		got := fn != nil && fn(ctx)
		if got != decl(ctx) {
			out = append(out, ctx)
		}
	}
	return out
}

// DeclarativeEquivalent reports whether two declarative predicates select the same contexts over the closed
// vocabulary — list order and duplicates are irrelevant; only what they SELECT is compared. This is the
// comparison a compiled-name store row is held to: against the DERIVED form of its compiled selector
// (DeclarativeFor), never against the func itself, so a selector the vocabulary cannot express exactly
// (correlated-triage refuses the empty class) still yields an unshadowed row when the row carries its
// closest form. The review of TG-576 found the func-direct comparison would have flagged that row forever.
func DeclarativeEquivalent(a, b skillstore.AppliesWhen) bool {
	fa, fb := declarativeApplies(a), declarativeApplies(b)
	for _, ctx := range PredicateContexts() {
		if fa(ctx) != fb(ctx) {
			return false
		}
	}
	return true
}

// DeclarativeFor derives the declarative predicate for a compiled selector by exhaustive probing: the
// phases, named classes and domains on which the selector is ever true. A selector true on the EMPTY domain
// is not domain-scoped (Domains stays empty — the STRICT rule would otherwise withhold it everywhere);
// a list that covers its whole vocabulary collapses to empty ("always"). `exact` reports whether the
// result reproduces the selector on EVERY context: a selector the closed vocabulary cannot express (one
// that distinguishes the empty class, say) still gets its closest form, with exact=false so the caller
// can say so — the compiled selector remains the authority at compose time regardless (store.go).
func DeclarativeFor(fn func(Context) bool) (aw skillstore.AppliesWhen, exact bool) {
	if fn == nil {
		return skillstore.AppliesWhen{}, false
	}
	phases := map[Phase]bool{}
	classes := map[execclass.Class]bool{}
	domains := map[Domain]bool{}
	for _, ctx := range PredicateContexts() {
		if !fn(ctx) {
			continue
		}
		phases[ctx.Phase] = true
		if ctx.ExecClass != "" {
			classes[ctx.ExecClass] = true
		}
		domains[ctx.Domain] = true
	}
	if len(phases) > 0 && len(phases) < len(predicatePhases) {
		for _, p := range predicatePhases {
			if phases[p] {
				aw.Phases = append(aw.Phases, string(p))
			}
		}
	}
	if len(classes) > 0 && len(classes) < len(predicateNamedClasses) {
		for _, c := range predicateNamedClasses {
			if classes[c] {
				aw.ExecClasses = append(aw.ExecClasses, string(c))
			}
		}
	}
	if len(domains) > 0 && !domains[DomainUnknown] {
		for _, d := range predicateDomains {
			if d != DomainUnknown && domains[d] {
				aw.Domains = append(aw.Domains, string(d))
			}
		}
	}
	return aw, len(PredicateDivergence(fn, aw)) == 0
}

// SelectorLaw is the compiled registry's answer to skillstore.CompiledSelectorLaw (TG-588): for a compiled name,
// the declarative form of its selector (DeclarativeFor); equivalence is DeclarativeEquivalent. Wired into the
// skill stores by both composition roots (the worker, before its boot import; the grounder, for console drafts) so
// a draft on a compiled name with a predicate the selector would ignore is refused where it is written.
func SelectorLaw() skillstore.CompiledSelectorLaw { return compiledLaw{reg: Default()} }

type compiledLaw struct{ reg *Registry }

func (l compiledLaw) CompiledPredicate(skillName string) (skillstore.AppliesWhen, bool) {
	for _, sk := range l.reg.All() {
		if sk.Name == skillName {
			aw, _ := DeclarativeFor(sk.AppliesWhen)
			return aw, true
		}
	}
	return skillstore.AppliesWhen{}, false
}

func (l compiledLaw) EquivalentPredicates(a, b skillstore.AppliesWhen) bool {
	return DeclarativeEquivalent(a, b)
}
