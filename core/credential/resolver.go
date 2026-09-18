package credential

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
)

// nativeStore is the standalone in-memory rule table — the config-not-code native store that is the
// FALLBACK when nothing is synced from an external source (REQ-1600, and the standalone-fallback intent of
// REQ-1610; cross-source sync/precedence is a later slice, T-016-7). It holds operator-declared Rules only;
// it is immutable after construction. Its ZERO value (no rules) refuses every target (fail closed).
type nativeStore struct {
	rules []Rule
}

// resolve applies match + most-specific-wins precedence over the native rule table (REQ-1600/1606). It
// returns the resolved Bundle (tagged with its rule id) or a fail-closed refusal — never a default identity.
func (s nativeStore) resolve(t Target) (Bundle, error) {
	r, err := selectRule(s.rules, t)
	if err != nil {
		return Bundle{}, err
	}
	return r.Bundle, nil
}

// ParseRules parses operator-declared resolver config (config-not-code, REQ-1600) into Rules. The format is
// ';'-separated entries, each '|'-separated:
//
//	kind:pattern | user | port | scheme | sshKeyRef | apiTokenRef | become
//
// where kind is host / host-glob / group / device-class / resource and every secret is a sealed SecretRef
// (env:/file:/store:/…), never a literal (REQ-1603). Trailing secret fields are optional; the scheme
// determines which is required (validated by NewBundle). A malformed entry is an ERROR — a resolver rule
// that cannot be understood is refused rather than silently granting a wildcard identity. This is the
// generalization of hostdiag.ParseAccess into a full per-target resolver grammar.
func ParseRules(spec string) ([]Rule, error) {
	var out []Rule
	for _, entry := range strings.Split(spec, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		f := strings.Split(entry, "|")
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		if len(f) < 4 {
			return nil, fmt.Errorf("credential: malformed rule %q: need at least kind:pattern|user|port|scheme", entry)
		}
		sel, err := parseSelector(f[0])
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(f[2])
		if err != nil {
			return nil, fmt.Errorf("credential: rule %q: invalid port %q: %w", entry, f[2], err)
		}
		bs := BundleSpec{User: f[1], Port: port, Scheme: ConnectionScheme(f[3])}
		if len(f) > 4 {
			bs.SSHKeyRef = config.SecretRef(f[4])
		}
		if len(f) > 5 {
			bs.APITokenRef = config.SecretRef(f[5])
		}
		if len(f) > 6 {
			bs.Become = config.SecretRef(f[6])
		}
		b, err := NewBundle(bs)
		if err != nil {
			return nil, fmt.Errorf("credential: rule %q: %w", entry, err)
		}
		out = append(out, Rule{ID: f[0], Selector: sel, Bundle: b})
	}
	return out, nil
}

// parseSelector parses a "kind:pattern" token into a Selector, validating the kind.
func parseSelector(tok string) (Selector, error) {
	i := strings.IndexByte(tok, ':')
	if i < 0 {
		return Selector{}, fmt.Errorf("credential: malformed selector %q: want kind:pattern", tok)
	}
	kind := SelectorKind(strings.TrimSpace(tok[:i]))
	pattern := strings.TrimSpace(tok[i+1:])
	if pattern == "" {
		return Selector{}, fmt.Errorf("credential: selector %q has an empty pattern", tok)
	}
	switch kind {
	case KindHost, KindResource, KindHostGlob, KindGroup, KindDeviceClass:
		return Selector{Kind: kind, Pattern: pattern}, nil
	default:
		return Selector{}, fmt.Errorf("credential: selector %q has unknown kind %q (use host/host-glob/group/device-class/resource)", tok, kind)
	}
}
