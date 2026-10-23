package skillstore

import (
	"errors"
	"fmt"
	"strings"
)

// THE SELECTOR LAW AT WRITE TIME (TG-588, follow-up to TG-576).
//
// TG-576 states the law the composer applies: for a skill whose NAME is compiled into the agent, the compiled
// selector decides WHEN the skill loads and a store row supplies only the BODY; a row's own applies_when is
// ignored there (and the session records `predicate-shadowed:<name>` when it diverges). Until this file, nothing
// said so at WRITE time: an operator could draft a compiled name with any applies_when and nothing told them the
// predicate would be ignored, and the flywheel copied the production row's (possibly flattened) predicate into
// every candidate, so a shadowed predicate propagated into graduates for ever. Both are closed here, and the
// knowledge they need — which names are compiled, what their selectors mean in the declarative vocabulary — is
// INJECTED (core/skillstore cannot import agent/skills: the registry imports this package), so the stores stay
// pure and a wiring that forgets the law simply has no law (the gate is a no-op, exactly as before).

// CompiledSelectorLaw is what the write-time gate and the candidate generator need to know about compiled names:
// the declarative form of a compiled name's selector, and predicate equivalence under the vocabulary (two
// declarative predicates are equivalent when they decide every context alike — agent/skills.DeclarativeEquivalent).
type CompiledSelectorLaw interface {
	CompiledPredicate(skillName string) (AppliesWhen, bool)
	EquivalentPredicates(a, b AppliesWhen) bool
}

// ErrPredicateShadowed is the write-time refusal for a draft on a compiled name whose applies_when the compiled
// selector would ignore. The message names both predicates so the operator can supply the compiled one (an EMPTY
// applies_when means "always", which is itself a divergence for every narrower selector — there is no "omit").
var ErrPredicateShadowed = errors.New("skillstore: applies_when diverges from the compiled selector for this name — the compiled selector decides WHEN the skill loads and would IGNORE this predicate at compose (TG-576 selector law); supply the compiled-derived predicate named below, the only one that decides anything for this name")

// selectorLawHolder is the optional surface a Store exposes when a law was wired into it.
type selectorLawHolder interface{ SelectorLaw() CompiledSelectorLaw }

// lawOf returns the store's law, or nil when none was wired (then there is nothing to enforce — a store that knows
// no compiled names cannot tell a shadowed predicate from a free one, and refusing blindly would stall every write).
func lawOf(st any) CompiledSelectorLaw {
	if h, ok := st.(selectorLawHolder); ok {
		return h.SelectorLaw()
	}
	return nil
}

// checkSelectorLaw refuses a draft on a compiled name whose predicate the compiled selector would ignore.
func checkSelectorLaw(st any, v Version) error {
	law := lawOf(st)
	if law == nil {
		return nil
	}
	want, compiled := law.CompiledPredicate(v.SkillName)
	if !compiled || law.EquivalentPredicates(v.AppliesWhen, want) {
		return nil
	}
	return fmt.Errorf("%w: %s — compiled selector = %s, draft applies_when = %s", ErrPredicateShadowed, v.SkillName, describePredicate(want), describePredicate(v.AppliesWhen))
}

// CandidatePredicate is the predicate a generated candidate for skillName must carry: the compiled-derived one
// when the store's law knows the name (so a flattened production row stops propagating its predicate into
// graduates), else the production row's own. Pure; the generator's only predicate decision.
func CandidatePredicate(st any, skillName string, current AppliesWhen) AppliesWhen {
	if law := lawOf(st); law != nil {
		if want, compiled := law.CompiledPredicate(skillName); compiled {
			return want
		}
	}
	return current
}

func describePredicate(aw AppliesWhen) string {
	part := func(k string, v []string) string {
		if len(v) == 0 {
			return k + "=*"
		}
		return k + "=[" + strings.Join(v, ",") + "]"
	}
	return "{" + part("phases", aw.Phases) + " " + part("exec_classes", aw.ExecClasses) + " " + part("domains", aw.Domains) + "}"
}

// WithSelectorLaw wires a law into the in-memory store (chainable; nil clears it).
func (m *MemStore) WithSelectorLaw(law CompiledSelectorLaw) *MemStore {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.law = law
	return m
}

// SelectorLaw returns the law the in-memory store was wired with (nil = none).
func (m *MemStore) SelectorLaw() CompiledSelectorLaw {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.law
}
