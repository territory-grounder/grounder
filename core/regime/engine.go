package regime

import (
	"errors"
	"fmt"

	"github.com/territory-grounder/grounder/core/credential"
)

// Engine is the RegimeResolver — the single entry point the actuation path consults BEFORE selecting an
// effect channel, in place of a hardcoded direct-SSH effect (REQ-1700). It holds three pieces of
// operator-declared configuration: the config-not-code regime Rules (which estate selector maps to which
// regime), the lane registry (which Lane is wired for each regime), and an optional default Lane (native-ssh)
// for the unknown-regime case (REQ-1701).
//
// FAIL-CLOSED BY CONSTRUCTION (REQ-1701): SelectLane returns a typed Lane OR a typed ErrNoRegime /
// ErrAmbiguousRegime — never an arbitrary or guessed lane. The ZERO Engine (no rules, no lanes, no default)
// refuses every target. There is no setter that installs a wildcard lane; ambiguity never falls back to the
// default; a resolved regime with no wired lane refuses. Regime resolution keys off the SHARED estate
// object-model (credential.Selector/Target/Match/Specificity, REQ-1703) — no second inventory grammar.
type Engine struct {
	rules       []Rule
	lanes       map[Regime]Lane
	defaultLane Lane // operator-declared default (native-ssh) for the unknown-regime case; nil ⇒ refuse
}

// EngineOption configures an Engine at construction (config-not-code).
type EngineOption func(*Engine)

// WithDefaultLane declares the operator's default lane (native-ssh per REQ-1701) used WHEN a target matches no
// regime rule. Without it an unknown target is refused (ErrNoRegime). A nil lane is ignored (stays refuse).
// The default is applied ONLY to the unknown-regime case — never to an ambiguous target, which always fails
// closed.
func WithDefaultLane(l Lane) EngineOption {
	return func(e *Engine) {
		if l != nil {
			e.defaultLane = l
		}
	}
}

// NewEngine builds a RegimeResolver over operator-declared regime rules and a lane registry (config-not-code,
// REQ-1700). The registry is indexed by each lane's Regime(); a later-registered lane for the same regime
// replaces an earlier one (an operator wiring choice, not a security decision). Passing no rules and no
// default yields a valid engine that refuses everything (fail closed). Rules that name an unknown regime are
// still refused at resolution time (resolver.go), so a malformed rule never routes a target down an undefined
// channel.
func NewEngine(rules []Rule, lanes []Lane, opts ...EngineOption) *Engine {
	reg := make(map[Regime]Lane, len(lanes))
	for _, l := range lanes {
		if l != nil {
			reg[l.Regime()] = l
		}
	}
	e := &Engine{rules: rules, lanes: reg}
	for _, o := range opts {
		o(e)
	}
	return e
}

// SelectLane resolves a target to exactly one effect lane, or refuses (REQ-1700/1701). It is the typed entry
// point the actuation path consumes in place of a hardcoded direct-SSH effect. The returned Lane's effect leaf
// is UNEXPORTED and reachable ONLY through the spec/013 interceptor via LaneEffect (REQ-1702) — SelectLane
// grants no authority, it only names the channel.
//
// The exact rule (fail-closed):
//  1. Resolve the target's regime by most-specific-wins over the shared object-model rules.
//  2. If the top-specificity rules name MORE THAN ONE distinct regime → refuse ErrAmbiguousRegime (never
//     guess a lane; ambiguity does NOT fall back to the default).
//  3. If exactly one regime resolves → return the Lane bound to it in the registry; if that regime has no
//     wired lane → refuse ErrNoRegime (a resolved-but-unwired regime is not actuatable).
//  4. If NO rule matches → return the operator-declared default lane (native-ssh) if one is declared;
//     otherwise refuse ErrNoRegime.
func (e *Engine) SelectLane(t credential.Target) (Lane, error) {
	res := e.Resolve(t)
	if !res.Resolved {
		return nil, res.Err
	}
	if res.Default {
		return e.defaultLane, nil // non-nil whenever Resolve set Default
	}
	lane, ok := e.lanes[res.Regime]
	if !ok {
		return nil, fmt.Errorf("%w: regime %q resolved but has no wired lane", ErrNoRegime, res.Regime)
	}
	return lane, nil
}

// LaneForRegime returns the wired lane for a SPECIFIC regime, or ok=false if that regime has no lane in the
// registry. It is the entry point for EFFECT-KIND-driven routing (REQ-1700): where SelectLane routes by the
// TARGET's management regime (a host managed by ssh / awx / …), some op-classes name their effect channel by
// their KIND — an AWX-launch op runs through the awx-job lane and a hypervisor guest-lifecycle op through the
// proxmox lane REGARDLESS of the target host's management regime (the same guest is native-ssh for a service
// restart but hypervisor-mediated for start/stop). The caller (the runner) maps an op-class's effect kind to
// its regime and asks for that lane HERE; ok=false ⇒ the caller fails closed (a resolved-but-unwired lane never
// actuates). Like SelectLane it grants NO authority — it only names the channel; the lane's effect leaf stays
// UNEXPORTED and reachable only through the spec/013 interceptor via LaneEffect.
func (e *Engine) LaneForRegime(r Regime) (Lane, bool) {
	lane, ok := e.lanes[r]
	return lane, ok
}

// Resolve performs regime resolution and returns the non-secret Resolution (REQ-1700/1701) the audit layer
// (T-017-6) records and the console (T-017-7) renders — the target's resolved regime, the matched rule id, or
// the typed refusal. It applies the same fail-closed rule as SelectLane: a matched regime resolves; an
// ambiguous target refuses (never defaulting); an unmatched target takes the operator default lane if declared
// and otherwise refuses. Resolve reports the regime DATA outcome; SelectLane additionally requires the
// resolved regime to have a wired lane.
func (e *Engine) Resolve(t credential.Target) Resolution {
	reg, id, err := resolveRegime(e.rules, t)
	switch {
	case err == nil:
		return Resolution{Regime: reg, RuleID: id, Resolved: true}
	case errors.Is(err, ErrAmbiguousRegime):
		// Ambiguity fails closed and NEVER falls back to the default (REQ-1701).
		return Resolution{Resolved: false, Err: err}
	default:
		// No rule matched: apply the operator-declared default lane if one exists, else refuse.
		if e.defaultLane != nil {
			return Resolution{Regime: e.defaultLane.Regime(), Default: true, Resolved: true}
		}
		return Resolution{Resolved: false, Err: err}
	}
}
