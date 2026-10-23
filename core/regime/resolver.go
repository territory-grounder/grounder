package regime

import (
	"errors"
	"fmt"

	"github.com/territory-grounder/grounder/core/credential"
)

// ---------------------------------------------------------------------------------------------------------
// Typed fail-closed refusals (REQ-1701). SelectLane returns exactly ONE of: a typed Lane, ErrNoRegime, or
// ErrAmbiguousRegime. It NEVER returns an arbitrary/guessed lane. ErrAmbiguousRegime wraps ErrNoRegime so a
// caller can treat every refusal uniformly with IsRefused — one-regime-per-target, ambiguity fails closed.
// ---------------------------------------------------------------------------------------------------------

// ErrNoRegime is the fail-closed sentinel for a target that resolves to no known regime AND no operator
// default lane is declared, or whose resolved regime has no wired lane (REQ-1701). It carries the ZERO Lane
// (nil): the target is not actuatable through any lane rather than an arbitrary one.
var ErrNoRegime = errors.New("regime: target resolves to no regime and no default lane — refuse (fail closed)")

// ErrAmbiguousRegime is returned when a target matches more than one DISTINCT regime at the top specificity
// tier (REQ-1701): the engine refuses rather than choosing a lane arbitrarily. It WRAPS ErrNoRegime so
// errors.Is(err, ErrNoRegime) holds for every fail-closed outcome — ambiguity never falls back to a default.
var ErrAmbiguousRegime = errAmbiguousRegime{}

type errAmbiguousRegime struct{}

func (errAmbiguousRegime) Error() string {
	return "regime: target matches more than one regime — refuse (ambiguity fails closed, never guessed)"
}

// Unwrap makes ErrAmbiguousRegime satisfy errors.Is(err, ErrNoRegime): every refusal is fail-closed.
func (errAmbiguousRegime) Unwrap() error { return ErrNoRegime }

// IsRefused reports whether err is any fail-closed refusal from the resolver (no-regime or ambiguous). On
// true, the caller MUST NOT actuate the target through any lane.
func IsRefused(err error) bool { return err != nil && errors.Is(err, ErrNoRegime) }

// ---------------------------------------------------------------------------------------------------------
// Config-not-code regime rules (REQ-1700/1703). A Rule maps an estate Selector to exactly one Regime. The
// Selector is the SHARED estate object-model built once in core/credential (Selector/Target/Match/
// Specificity, REQ-1605) that the policy engine (spec/015 REQ-1505) and credential engine (spec/016
// REQ-1605) also key off — regime keys off the SAME grammar and defines no second inventory grammar
// (REQ-1703). Which regime a target belongs to is DATA, not a code branch.
// ---------------------------------------------------------------------------------------------------------

// Rule is one operator-declared regime-resolution entry: a shared-object-model Selector bound to one Regime.
// ID is a stable, non-secret label recorded for audit/provenance (the matched rule id, REQ-1715).
type Rule struct {
	ID       string
	Selector credential.Selector
	Regime   Regime
}

// Resolution is the non-secret outcome of a regime resolution, surfaced so the audit layer (T-017-6) records
// exactly what was matched (REQ-1715) and the console renders the per-target regime map (REQ-1716). A
// resolution is either Resolved with a Regime + RuleID (RuleID empty ⇒ the operator default lane was used) or
// not Resolved with an Err.
type Resolution struct {
	Regime   Regime // the resolved regime (zero if refused)
	RuleID   string // the matched rule id ("" ⇒ operator default lane; unset on refusal)
	Default  bool   // true ⇒ resolved via the operator-declared default lane, not a matching rule
	Resolved bool   // true ⇒ a lane is returnable; false ⇒ Err carries the fail-closed refusal
	Err      error  // the typed refusal (ErrNoRegime / ErrAmbiguousRegime) when !Resolved
}

// resolveRegime applies most-specific-wins precedence over the rules matching t (REQ-1700/1701/1703), reusing
// the spec/016 shared object-model and precedence model:
//   - collect every matching rule (credential.Match over the shared grammar);
//   - if none match → (unknown, ErrNoRegime) so the caller can apply the operator default lane;
//   - keep the rules at the highest credential.Specificity, then:
//   - if those top-specificity rules name MORE THAN ONE distinct regime → ErrAmbiguousRegime (fail closed:
//     one-regime-per-target, never guess);
//   - otherwise the single agreed regime wins, tagged with a matched rule id (the most-specific one).
//
// It never returns a valid regime together with a nil error unless exactly one regime won at the top tier.
func resolveRegime(rules []Rule, t credential.Target) (Regime, string, error) {
	bestSpec := -1
	var topRegimes map[Regime]string // distinct regime -> a matched rule id at the top specificity tier
	matched := false

	for _, r := range rules {
		if !credential.Match(r.Selector, t) {
			continue
		}
		matched = true
		spec := credential.Specificity(r.Selector)
		switch {
		case spec > bestSpec:
			bestSpec = spec
			topRegimes = map[Regime]string{r.Regime: r.ID}
		case spec == bestSpec:
			if _, seen := topRegimes[r.Regime]; !seen {
				topRegimes[r.Regime] = r.ID
			}
		}
	}

	if !matched {
		return "", "", ErrNoRegime
	}
	if len(topRegimes) > 1 {
		return "", "", fmt.Errorf("%w: target %q matched %d distinct regimes at equal specificity (%d)",
			ErrAmbiguousRegime, targetLabel(t), len(topRegimes), bestSpec)
	}
	for reg, id := range topRegimes { // exactly one entry
		if !reg.Valid() {
			// A rule that named an unknown regime is a poisoned/malformed rule — refuse rather than route down
			// an undefined channel (REQ-1700 closed set).
			return "", "", fmt.Errorf("%w: matched rule %q names unknown regime %q", ErrNoRegime, id, reg)
		}
		return reg, id, nil
	}
	return "", "", ErrNoRegime // unreachable (matched ⇒ at least one topRegime), keeps the compiler honest
}

// targetLabel renders a non-secret identifier for a target for error/audit text (mirrors the credential
// resolver's targetLabel over the shared object-model).
func targetLabel(t credential.Target) string {
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
