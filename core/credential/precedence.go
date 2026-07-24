package credential

import "fmt"

// Rule is one operator-declared resolver entry (config-not-code, REQ-1600): a Selector over the shared
// object-model bound to a resolved-identity Bundle. ID is a stable, non-secret label for audit/provenance.
type Rule struct {
	ID       string
	Selector Selector
	Bundle   Bundle
}

// selectRule applies most-specific-wins precedence (REQ-1606) over the rules that match t:
//   - collect every matching rule;
//   - if none match → ErrUnresolved (fail closed, REQ-1602);
//   - pick the highest Specificity;
//   - if TWO OR MORE matching rules share the top specificity → ErrAmbiguous (fail closed): an
//     equal-specificity conflict refuses rather than choosing an arbitrary bundle.
//
// It returns the winning rule tagged into the bundle's provenance, or a typed refusal error. It never
// returns a valid bundle together with a nil error unless exactly one most-specific rule won.
func selectRule(rules []Rule, t Target) (Rule, error) {
	var best Rule
	bestSpec := -1
	tie := false
	matched := false

	for _, r := range rules {
		if !Match(r.Selector, t) {
			continue
		}
		matched = true
		spec := Specificity(r.Selector)
		switch {
		case spec > bestSpec:
			best = r
			bestSpec = spec
			tie = false
		case spec == bestSpec:
			tie = true
		}
	}

	if !matched {
		return Rule{}, ErrUnresolved
	}
	if tie {
		return Rule{}, fmt.Errorf("%w: target %q matched multiple rules of equal specificity (%d)",
			ErrAmbiguous, targetLabel(t), bestSpec)
	}
	// Guard: a winning rule MUST carry a valid bundle; a matched-but-invalid rule refuses (never fall open).
	if !best.Bundle.Valid() {
		return Rule{}, fmt.Errorf("%w: matched rule %q carries no valid bundle", ErrUnresolved, best.ID)
	}
	best.Bundle = best.Bundle.withRuleID(best.ID)
	return best, nil
}

// targetLabel renders a non-secret identifier for a target for error/audit text.
func targetLabel(t Target) string {
	switch {
	case t.Host != "":
		return t.Host
	case t.Resource != "":
		return t.Resource
	case t.DeviceClass != "":
		return "device-class:" + t.DeviceClass
	case len(t.Groups) > 0:
		return "group:" + t.Groups[0]
	default:
		return "(empty target)"
	}
}
