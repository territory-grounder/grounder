package credential

import "strings"

// ---------------------------------------------------------------------------------------------------------
// SHARED ESTATE OBJECT-MODEL (REQ-1605).
//
// Selector, Target, and Match are the ONE estate object-model that BOTH this credential resolver AND the
// policy engine (spec/015 REQ-1505) key resolution off. It is built once here and referenced by both — there
// is deliberately no second, divergent inventory grammar. When spec/015 lands, `core/policy` imports these
// types (Selector/Target/Match/Specificity) rather than defining its own host/glob/group/device-class
// matcher. The primitive is intentionally dependency-free (only the standard library) so either engine can
// consume it without pulling in the other's package.
//
// Provenance: [F] generalizes hostdiag.globMatch + the `site|hostglob|sshuser|keyref` allowlist
// (modules/observability/hostdiag) — the same leading/trailing '*' glob semantics — into a typed selector
// grammar over host / host-glob / group / device-class / resource.
// ---------------------------------------------------------------------------------------------------------

// SelectorKind is the dimension a Selector matches on. Ordered coarsest-last; the numeric values are NOT the
// specificity ranking (see Specificity) but a stable, closed enumeration.
type SelectorKind string

const (
	// KindHost matches one exact canonical host name.
	KindHost SelectorKind = "host"
	// KindResource matches one exact named resource (a service / logical resource id).
	KindResource SelectorKind = "resource"
	// KindHostGlob matches a host name by a leading/trailing '*' glob (e.g. "dc1*").
	KindHostGlob SelectorKind = "host-glob"
	// KindGroup matches a target that is a member of the named estate group.
	KindGroup SelectorKind = "group"
	// KindDeviceClass matches a target whose device-class equals the pattern (e.g. "cisco-asa").
	KindDeviceClass SelectorKind = "device-class"
)

// Selector is one rule's matcher over the shared object-model. Pattern is interpreted per Kind.
type Selector struct {
	Kind    SelectorKind
	Pattern string
}

// Target is the concrete estate object a resolution is about — the shared inventory primitive (REQ-1605).
// A host may carry group memberships and a device-class; a named resource carries its Resource id. The same
// Target shape is what the policy engine matches its rules against.
type Target struct {
	Host        string   // canonical host name (may be empty for a pure-resource target)
	Resource    string   // named resource id (optional)
	Groups      []string // estate group memberships (optional)
	DeviceClass string   // device-class (optional, e.g. "cisco-asa")
}

// Match reports whether sel matches t over the shared object-model. Matching is case-insensitive on names,
// mirroring the hostdiag glob semantics it generalizes.
func Match(sel Selector, t Target) bool {
	switch sel.Kind {
	case KindHost:
		return t.Host != "" && eqFold(sel.Pattern, t.Host)
	case KindResource:
		return t.Resource != "" && eqFold(sel.Pattern, t.Resource)
	case KindHostGlob:
		return t.Host != "" && GlobMatch(sel.Pattern, t.Host)
	case KindGroup:
		for _, g := range t.Groups {
			if eqFold(sel.Pattern, g) {
				return true
			}
		}
		return false
	case KindDeviceClass:
		return t.DeviceClass != "" && eqFold(sel.Pattern, t.DeviceClass)
	default:
		// An unknown selector kind matches NOTHING — fail closed by construction (REQ-1602): a malformed rule
		// grants no access rather than a wildcard.
		return false
	}
}

// Specificity ranks a matching selector for most-specific-wins precedence (REQ-1606). Larger = more
// specific. The tiers are, from most to least specific: exact host / exact resource > host-glob (narrower
// glob = more specific, measured by the count of fixed, non-'*' characters) > group > device-class. Two
// selectors returning the SAME specificity are a tie that fails closed rather than picking arbitrarily
// (resolved in precedence.go).
func Specificity(sel Selector) int {
	switch sel.Kind {
	case KindHost, KindResource:
		return 1_000_000
	case KindHostGlob:
		// A more-anchored glob (more fixed characters) is more specific. Cap the fixed-char contribution
		// below the exact-host tier so a glob can never tie or beat an exact host.
		fixed := len(strings.ReplaceAll(sel.Pattern, "*", ""))
		if fixed > 100_000 {
			fixed = 100_000
		}
		return 500_000 + fixed
	case KindGroup:
		return 200_000
	case KindDeviceClass:
		return 100_000
	default:
		return 0
	}
}

// GlobMatch does a case-insensitive glob against a leading and/or trailing '*' (enough for site prefixes
// like "dc1*" and suffixes like "*.edge"). No '*' ⇒ exact match; "*" ⇒ any host. This is the shared
// generalization of hostdiag.globMatch, exported so both engines use one implementation.
func GlobMatch(glob, host string) bool {
	glob = strings.ToLower(strings.TrimSpace(glob))
	host = strings.ToLower(strings.TrimSpace(host))
	switch {
	case glob == "*":
		return true
	case strings.HasPrefix(glob, "*") && strings.HasSuffix(glob, "*"):
		return strings.Contains(host, strings.Trim(glob, "*"))
	case strings.HasSuffix(glob, "*"):
		return strings.HasPrefix(host, strings.TrimSuffix(glob, "*"))
	case strings.HasPrefix(glob, "*"):
		return strings.HasSuffix(host, strings.TrimPrefix(glob, "*"))
	default:
		return glob == host
	}
}

func eqFold(a, b string) bool { return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) }
