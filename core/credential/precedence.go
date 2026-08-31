package credential

import (
	"fmt"
	"sort"
)

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
	matched := false
	var tied []Rule // every rule at the current best specificity, for the same-answer check below

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
			tied = []Rule{r}
		case spec == bestSpec:
			tied = append(tied, r)
		}
	}

	if !matched {
		return Rule{}, ErrUnresolved
	}
	// A TIE IS ONLY AMBIGUOUS IF THE ANSWERS DIFFER (REQ-1606a).
	//
	// Equal specificity means two rules have equal CLAIM on the target — it does not mean they disagree. A
	// source that derives rules from an external registry routinely emits several for one host: the AWX source
	// makes one group rule per INVENTORY, and AWX binds credentials to job TEMPLATES rather than to hosts, so a
	// host legitimately sitting in two credentialed inventories yields two rules BY CONSTRUCTION. That is normal
	// operator practice, not a misconfiguration, and no amount of further querying collapses it — AWX simply
	// does not store "the credential for host X".
	//
	// Refusing on the rule COUNT rather than on the resulting IDENTITY therefore fails closed over a distinction
	// that may not exist. Measured live: 7 hosts — every Proxmox node plus the Nextcloud pair — were
	// uninvestigable for exactly this reason, and both of their tied rules resolved to the SAME user and the
	// SAME key reference, because only one AWX credential is mapped to a TG SecretRef at all. TG was blind on
	// its own hypervisors to protect a choice it never had to make.
	//
	// So compare the ANSWER. Identical identities collapse to one (deterministically by rule id, so the audit
	// records a stable winner); genuinely different identities still refuse, which is the case fail-closed
	// exists for — using the wrong key is an ungoverned access path, and guessing between two real candidates
	// is exactly what must never happen.
	if len(tied) > 1 {
		sort.Slice(tied, func(i, j int) bool { return tied[i].ID < tied[j].ID })
		for _, r := range tied[1:] {
			if !sameIdentity(tied[0].Bundle, r.Bundle) {
				return Rule{}, fmt.Errorf("%w: target %q matched multiple rules of equal specificity (%d) "+
					"carrying DIFFERENT identities", ErrAmbiguous, targetLabel(t), bestSpec)
			}
		}
		best = tied[0]
	}
	// Guard: a winning rule MUST carry a valid bundle; a matched-but-invalid rule refuses (never fall open).
	if !best.Bundle.Valid() {
		return Rule{}, fmt.Errorf("%w: matched rule %q carries no valid bundle", ErrUnresolved, best.ID)
	}
	best.Bundle = best.Bundle.withRuleID(best.ID)
	return best, nil
}

// sameIdentity reports whether two bundles authenticate as the SAME principal by the SAME means. It compares
// every field deciding WHO connects and WITH WHAT — never secret material, which a bundle holds only as a
// reference. RuleID is deliberately excluded: two rules with different ids naming one identity are precisely
// the case this exists to accept.
func sameIdentity(a, b Bundle) bool {
	return a.Valid() && b.Valid() &&
		a.User() == b.User() &&
		a.Port() == b.Port() &&
		a.Scheme() == b.Scheme() &&
		a.SSHKeyRef() == b.SSHKeyRef() &&
		a.APITokenRef() == b.APITokenRef() &&
		a.BecomeRef() == b.BecomeRef()
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
