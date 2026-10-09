package credential

// Engine is the single credential-resolution entry point the actuation interceptor (spec/013) and the
// read-only investigation modules consult in place of a single hardcoded identity env var (REQ-1600). In the
// composed flow it runs AFTER the policy engine (spec/015) returns a non-deny verdict and BEFORE execute
// (REQ-1604, wired in a later slice T-016-5); this slice builds the resolver core it will call.
//
// FAIL-CLOSED BY CONSTRUCTION (REQ-1602): the ZERO Engine has no rules and refuses every target. There is no
// setter that can install a default/catch-all identity; Resolve returns the zero Bundle with a typed refusal
// on every non-matching, ambiguous, or invalid path.
type Engine struct {
	native nativeStore
}

// NewEngine builds an engine over an operator-declared native rule table (the standalone store). The rules
// are the config-not-code resolver data (REQ-1600); pass ParseRules output or hand-built Rules. Passing no
// rules yields a valid engine that refuses everything (fail closed).
func NewEngine(rules []Rule) *Engine {
	return &Engine{native: nativeStore{rules: rules}}
}

// Resolve maps a Target (host / host-glob / resource / group / device-class) to exactly one CredentialBundle
// through the native rule table, applying most-specific-wins precedence (REQ-1606). On success it returns a
// Valid Bundle tagged with the winning rule's id. On ANY failure — no match, an equal-specificity conflict,
// or a matched rule carrying no valid bundle — it returns the ZERO Bundle and a refusal error for which
// IsRefused reports true (REQ-1602). It NEVER returns a default, global, or last-used identity.
//
// The returned bundle carries only SecretRef references; call Bundle.Resolve to load the secrets at use time.
func (e *Engine) Resolve(t Target) (Bundle, error) {
	// A nil or zero Engine is a valid fail-closed engine (defence in depth against an un-initialised caller).
	if e == nil {
		return Bundle{}, ErrUnresolved
	}
	return e.native.resolve(t)
}
