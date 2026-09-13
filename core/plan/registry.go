package plan

import (
	"fmt"
	"sort"
)

// recipes is the ONE place a transaction-plan recipe is declared — a closed compiled set, the
// modules/catalog + core/pack discipline. EMPTY AT SHIP, DELIBERATELY (REQ-3007): with zero recipes no
// plan is ever composed and the single-action path is byte-identical; the first recipe is T-030-6, an
// owner-chosen remediation, in its own reviewed MR.
func recipes() []PlanRecipe {
	return nil
}

// All returns every declared recipe, validated and ordered by name. A malformed or duplicate
// declaration errors from HERE — caught by the catalog test once, at authoring time, never composing a
// wrong plan per session.
func All() ([]PlanRecipe, error) {
	out := recipes()
	seen := map[string]bool{}
	for _, r := range out {
		if err := r.Validate(); err != nil {
			return nil, err
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("plan: duplicate recipe name %q", r.Name)
		}
		seen[r.Name] = true
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ForTriggerIn selects the recipe a proposed action's op-class OFFERS from the given catalog — a pure
// lookup, no model token (REQ-3001), first match in the catalog's order (All() orders by name). An
// unknown/empty op-class and an unmatched class select nothing: every failure direction is "no plan,
// single action as today". It takes the catalog as an argument — rather than reading All() itself —
// because the ONE production consumer (ComposePlanActivity) resolves the catalog through its
// test-injectable seam first; a second All()-reading wrapper here would be machinery no binary calls,
// which is exactly what the deadcode gate exists to refuse.
func ForTriggerIn(catalog []PlanRecipe, opClass string) (PlanRecipe, bool) {
	if opClass == "" {
		return PlanRecipe{}, false
	}
	for _, r := range catalog {
		if r.TriggerOpClass == opClass {
			return r, true
		}
	}
	return PlanRecipe{}, false
}
