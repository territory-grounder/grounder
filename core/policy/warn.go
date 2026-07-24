package policy

// Warn-don't-block guard + the admin engine toggle — spec/015 task T-015-11 (REQ-1517, REQ-1519). This file
// encodes the paradigm's load-bearing product rule: THE OPERATOR OWNS THE PARANOIA DIAL. The engine WARNS
// loudly on a permissive or self-guardrail-removing posture — an allow-all rule, a removed policy floor, a
// Full-auto mode, an admin disabling the engine — but it NEVER refuses to apply the operator's chosen posture
// (REQ-1517). A single allow-all policy is reachable behind a red double-confirmation and is never blocked.
//
// Two DISTINCT floors — do not conflate them (as floor.go documents): the CONSTITUTIONAL mechanical never-auto
// floor (core/safety, INV-09) sits BENEATH this engine and is non-configurable, non-removable, and untouched
// by anything here; the OPERATOR policy layer (rules, the deny-floor, the engine toggle) is the dial the
// operator owns. Warning about / disabling the operator layer NEVER lifts the constitutional floor — a
// never-auto action still cannot resolve to `auto`, even with the engine disabled (proven in warn_test.go).
//
// The admin engine toggle (REQ-1519) lets an authenticated admin OVERRIDE the per-mode engine default: forcing
// the engine ON in a read-only mode (Shadow / HITL) records a warning; DISABLING it in an actuating mode
// (Semi-auto / Full-auto) records a double-confirmation warning. WHEN the engine is disabled, the decision
// wrapper (audit.go) defers to the mechanical floors — the engine-off verdict is `approve`, never `auto` — so
// disabling the engine is NOT a bypass of the never-auto floor or the mode chokepoint beneath it.
//
// Provenance: [R] paradigm-rule 4 (warn, don't block; the operator owns the dial) · [O] INV-09. See
// spec/015-policy-engine requirements.md REQ-1517/1519 and design.md (`policy.Engine`).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/credential"
)

// ---------------------------------------------------------------------------------------------------------
// Warn-don't-block surfaced warnings. A PolicyWarning is a NON-BLOCKING notice the engine surfaces about a
// permissive posture — it is RECORDED and RETURNED, never a hard block. It is deliberately DISTINCT from the
// Warning type in floor.go, which is the operator's acknowledgement INPUT (Text/Acknowledged/DoubleConfirm);
// this is the engine's OUTPUT notice about a risky-but-permitted choice.
// ---------------------------------------------------------------------------------------------------------

// WarnCode is the closed set of permissive-posture conditions the engine warns about (REQ-1517).
type WarnCode string

const (
	// WarnAllowAllRule — an operator rule with a permissive verdict (auto/approve) that constrains NO dimension,
	// i.e. it matches every action (an allow-all). Permitted (the operator owns the dial); warned, never blocked.
	WarnAllowAllRule WarnCode = "allow-all-rule"
	// WarnFloorRemoved — an execution deny-floor entry has been removed-with-warning, lowering the dial.
	WarnFloorRemoved WarnCode = "floor-entry-removed"
	// WarnFullAuto — the active mode is Full-auto (any allow/auto verdict actuates except the never-auto floor).
	WarnFullAuto WarnCode = "full-auto-mode"
	// WarnEngineForcedOn — an admin forced the engine ON in a read-only mode (Shadow / HITL), overriding the default.
	WarnEngineForcedOn WarnCode = "engine-forced-on"
	// WarnEngineDisabled — an admin DISABLED the engine in an actuating mode (Semi-auto / Full-auto).
	WarnEngineDisabled WarnCode = "engine-disabled"
)

// PolicyWarning is one non-blocking warning about a permissive posture (REQ-1517). It carries a stable Code
// and a human-readable Message for the console banner + the audit trail. It NEVER carries a secret. A warning
// existing does not change any verdict — it is advisory-and-loud, and the posture still applies.
type PolicyWarning struct {
	Code    WarnCode
	Message string
}

// WarnFor inspects an operator's current posture — the loaded rule DATA, the execution deny-floor, and the
// active mode — and returns the NON-BLOCKING warnings that apply (REQ-1517). It is a PURE read: it changes no
// rule, no floor, and no verdict, and it NEVER returns an error or refuses a posture — a permissive
// configuration yields warnings, not a block. An empty result means nothing risky is configured. A nil floor
// contributes no floor-removal warnings.
func WarnFor(rs RuleSet, floor *Floor, mode Mode) []PolicyWarning {
	var out []PolicyWarning

	// Allow-all rule: a permissive-verdict rule whose ONLY constraint is a wildcard host-glob "*" (the shape of
	// the `bare` template's single rule) matches EVERY host, i.e. every action. A DENY is a fail-closed
	// protection, not a permissive risk, so it is never warned about; a dimension-less Match is rejected at load
	// (ErrMalformedRule) and matches nothing, so it is not an allow-all either — only a wildcard is.
	for _, r := range rs.Rules {
		if r.IsDefault || r.Verdict == VerdictDeny {
			continue
		}
		if isAllowAllMatch(r.Match) {
			out = append(out, PolicyWarning{
				Code: WarnAllowAllRule,
				Message: fmt.Sprintf(
					"rule %q is an allow-all (%s verdict, wildcard host-glob) — it matches every action; permitted (the operator owns the dial) but the constitutional never-auto floor (INV-09) still clamps beneath it",
					r.ID, r.Verdict),
			})
		}
	}

	// Removed floor entries: each lowering of the operator deny-floor is surfaced (it was already audited at
	// removal time in floor.go; this re-surfaces the standing risk for the console banner).
	if floor != nil {
		for _, e := range floor.Entries() {
			if e.Removed {
				out = append(out, PolicyWarning{
					Code:    WarnFloorRemoved,
					Message: fmt.Sprintf("execution deny-floor entry %q has been removed-with-warning — the operator lowered the dial; the constitutional never-auto floor (INV-09) still clamps floor-class ops", e.ID),
				})
			}
		}
	}

	// Full-auto mode: the most permissive actuation posture.
	if mode == ModeFullAuto {
		out = append(out, PolicyWarning{
			Code:    WarnFullAuto,
			Message: "the active mode is Full-auto — any allow/auto verdict actuates except the constitutional never-auto floor (INV-09), which still clamps beneath",
		})
	}

	return out
}

// ---------------------------------------------------------------------------------------------------------
// Admin engine toggle (REQ-1519). An authenticated admin may enable/disable the policy engine, OVERRIDING the
// per-mode default. The toggle is warn-don't-block: it never refuses the posture, but a permissive/risky
// override is loud (acknowledged, and double-confirmed when disabling an actuating mode) and always audited.
// ---------------------------------------------------------------------------------------------------------

var (
	// ErrUnauthorizedEngineToggle is returned when the acting principal is not an authenticated admin holding
	// toggle authority (reuses the mode-change authority seam). The engine state is unchanged (fail closed).
	ErrUnauthorizedEngineToggle = errors.New("policy: actor lacks engine-toggle authority")
	// ErrEngineToggleNotConfirmed is returned when a warned override is attempted WITHOUT the required
	// acknowledgement (a single ack when forcing the engine on in a read-only mode; a distinct red
	// double-confirmation when disabling it in an actuating mode). The override is refused and the engine state
	// is unchanged. This is NOT "blocking the posture" (REQ-1517): WITH the confirmation the override always
	// applies — the requirement is only that a risky change be explicit and loud, exactly as floor.go requires
	// for removing a floor entry.
	ErrEngineToggleNotConfirmed = errors.New("policy: engine toggle requires an acknowledged confirmation — refused, engine state unchanged")
)

// isAllowAllMatch reports whether a Match is an allow-all: its ONLY specified dimension is a wildcard "*"
// host-glob selector, which matches every host (and therefore every action). This is precisely the shape of
// the `bare` template's single rule; any narrower selector or any additional dimension (op-class, argv,
// territory, reversible) is NOT an allow-all.
func isAllowAllMatch(m Match) bool {
	if m.OpClass != "" || m.ArgvPattern != "" || m.Territory != "" || m.Reversible != nil {
		return false
	}
	return m.Selector != nil && m.Selector.Kind == credential.KindHostGlob && m.Selector.Pattern == "*"
}

// EngineDefaultForMode is the per-mode engine default (REQ-1500/1519): the engine is OFF in the read-only modes
// (Shadow / HITL) and ON in the actuating modes (Semi-auto / Full-auto). The admin toggle OVERRIDES this default;
// an override in the permissive direction (forcing on in read-only, disabling in actuating) is the warned case.
func EngineDefaultForMode(m Mode) bool { return m == ModeSemiAuto || m == ModeFullAuto }

// ToggleRecord is the NON-SECRET audit record emitted on EVERY admin engine-toggle override (REQ-1519). It
// records the resolved enabled state, the mode it overrode, the actor, the acknowledged warning text, whether
// the distinct double-confirmation accompanied a disable, and which warned condition fired. It carries no
// secret. A later leaf (T-015-12) persists it alongside the mode-transition records; here it is returned and
// appended to the governance ledger.
type ToggleRecord struct {
	Enabled       bool          // the resolved engine state after the override.
	Mode          Mode          // the active mode whose default was overridden.
	Actor         string        // the admin principal (non-secret label).
	WarningText   string        // the acknowledged warning text (non-secret); empty when no confirmation was required.
	DoubleConfirm bool          // whether a distinct red double-confirmation accompanied the change.
	ForcedOn      bool          // true when the override forced the engine ON in a read-only mode (warned).
	Disabled      bool          // true when the override DISABLED the engine in an actuating mode (double-warned).
	Warning       PolicyWarning // the surfaced warning for the console banner (zero-value Code when none).
	Reason        string        // human-readable explanation for the audit trail / packet-tracer.
	Timestamp     time.Time     // when the override was recorded (the only non-deterministic field).
}

// EngineToggle is the admin enable/disable control for the policy engine (REQ-1519). It holds an OPTIONAL
// override of the per-mode default and gates every change behind an authenticated, authority-checked admin
// (reusing the AuthorityChecker seam the mode controller uses) plus an audited governance-ledger append. It is
// concurrency-safe. Effective(mode) reports whether the engine is enabled for a mode (the override if set, else
// the mode default); the decision wrapper (AuditedEngine, audit.go) consults it, and WHEN it reports disabled
// the wrapper defers to the mechanical floors — the engine-off state is never a bypass of the never-auto floor.
type EngineToggle struct {
	mu       sync.RWMutex
	override *bool // nil ⇒ follow the per-mode default; else the admin's explicit override.
	authz    AuthorityChecker
	ledger   ledgerAppender
	now      func() time.Time
	logf     func(format string, args ...any)
}

// NewEngineToggle builds a toggle over the admin authority checker and the governance ledger. authz and ledger
// are required to perform an override (a nil authz refuses every override, fail closed; a nil ledger refuses an
// unaudited override). The override starts unset (Effective follows the per-mode default). The clock defaults to
// time.Now.
func NewEngineToggle(authz AuthorityChecker, ledger ledgerAppender) *EngineToggle {
	return &EngineToggle{authz: authz, ledger: ledger, now: time.Now}
}

// WithClock sets the audit clock (for deterministic tests) and returns the toggle.
func (t *EngineToggle) WithClock(now func() time.Time) *EngineToggle {
	t.mu.Lock()
	defer t.mu.Unlock()
	if now != nil {
		t.now = now
	}
	return t
}

// WithLogf sets the optional logger and returns the toggle.
func (t *EngineToggle) WithLogf(logf func(string, ...any)) *EngineToggle {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.logf = logf
	return t
}

// Effective reports whether the policy engine is enabled for the given active mode: the admin override if one
// has been set, else the per-mode default (REQ-1519). It is a concurrency-safe pure read. It is fail-closed for
// safety-relevant purposes: even when it reports enabled, the never-auto floor still clamps; even when it
// reports disabled, the decision wrapper routes to a human (`approve`), never to `auto`.
func (t *EngineToggle) Effective(mode Mode) bool {
	if t == nil {
		return EngineDefaultForMode(mode)
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.override != nil {
		return *t.override
	}
	return EngineDefaultForMode(mode)
}

// Override sets the admin's explicit enable/disable of the engine against the active mode's default and returns
// a NON-SECRET ToggleRecord (REQ-1519). It is the warn-don't-block gate for the engine toggle:
//
//   - It gates on an authenticated admin (AuthorityChecker); an unauthenticated/unauthorized actor is REFUSED
//     (ErrUnauthorizedEngineToggle) and the engine state is unchanged.
//   - Forcing the engine ON in a read-only mode (Shadow / HITL) requires a single acknowledgement and records a
//     WARNING (WarnEngineForcedOn). Disabling it in an actuating mode (Semi-auto / Full-auto) requires a distinct
//     red DOUBLE-CONFIRMATION and records a double-confirmation warning (WarnEngineDisabled). WITHOUT the required
//     acknowledgement the change is refused (ErrEngineToggleNotConfirmed) — not because the posture is blocked
//     (REQ-1517 forbids that), but because a risky change must be loud; WITH it the override always applies.
//   - Setting the engine to the mode's own default direction (enabling in an actuating mode, disabling in a
//     read-only mode) is not a permissive override and needs no confirmation.
//   - It appends ONE immutable governance-ledger record BEFORE the override takes effect (an unaudited toggle is
//     refused); a ledger-append failure leaves the engine state unchanged (fail closed).
//
// The constitutional never-auto floor (INV-09) is UNAFFECTED by any override — disabling the engine defers to
// the mechanical floors (the wrapper returns `approve`, never `auto`), and enabling it does not lift the floor.
func (t *EngineToggle) Override(ctx context.Context, actor string, enable bool, mode Mode, ack Warning) (ToggleRecord, error) {
	if t == nil {
		return ToggleRecord{}, ErrUnauthorizedEngineToggle
	}
	if t.authz == nil {
		return ToggleRecord{}, ErrUnauthorizedEngineToggle
	}
	if err := t.authz.HasModeChangeAuthority(ctx, actor); err != nil {
		return ToggleRecord{}, fmt.Errorf("%w: %v", ErrUnauthorizedEngineToggle, err)
	}

	def := EngineDefaultForMode(mode)
	forcedOn := enable && !def  // forcing the engine ON in a read-only mode (Shadow / HITL) — warned.
	disabled := !enable && def  // disabling the engine in an actuating mode (Semi-auto / Full-auto) — double-warned.

	rec := ToggleRecord{Enabled: enable, Mode: mode, Actor: actor, ForcedOn: forcedOn, Disabled: disabled}

	switch {
	case disabled:
		// Disabling an actuating mode's engine requires the distinct red double-confirmation (REQ-1519).
		if !ack.redConfirmed() {
			return ToggleRecord{}, ErrEngineToggleNotConfirmed
		}
		rec.DoubleConfirm = true
		rec.WarningText = ack.Text
		rec.Warning = PolicyWarning{
			Code:    WarnEngineDisabled,
			Message: fmt.Sprintf("admin %s DISABLED the policy engine in %s (double-confirmed) — actions now defer to the mechanical floors: verdict `approve` (route to a human), never `auto`; the constitutional never-auto floor (INV-09) still clamps beneath", actor, mode),
		}
		rec.Reason = rec.Warning.Message
	case forcedOn:
		// Forcing the engine on in a read-only mode requires a single acknowledgement (REQ-1519).
		if !ack.acknowledged() {
			return ToggleRecord{}, ErrEngineToggleNotConfirmed
		}
		rec.DoubleConfirm = ack.DoubleConfirm
		rec.WarningText = ack.Text
		rec.Warning = PolicyWarning{
			Code:    WarnEngineForcedOn,
			Message: fmt.Sprintf("admin %s forced the policy engine ON in %s (a read-only mode) — the engine now decides per action; the constitutional never-auto floor (INV-09) is unaffected", actor, mode),
		}
		rec.Reason = rec.Warning.Message
	default:
		// Setting the engine to the mode's own default direction — no permissive override, no confirmation needed.
		rec.WarningText = ack.Text
		rec.DoubleConfirm = ack.DoubleConfirm
		rec.Reason = fmt.Sprintf("admin %s set the policy engine %s in %s (the mode default direction — no confirmation required)", actor, enabledWord(enable), mode)
	}

	// Append the immutable governance record BEFORE the override takes effect (REQ-1519, INV-19). A nil ledger
	// or an append failure refuses the change — the engine state is not advanced onto an unrecorded toggle.
	if t.ledger == nil {
		return ToggleRecord{}, errors.New("policy: nil ledger — refusing an unaudited engine toggle")
	}
	rec.Timestamp = t.clock()
	govRec := audit.GovDecision{
		Decision: fmt.Sprintf("engine-toggle:%s", enabledWord(enable)),
		Reason:   fmt.Sprintf("actor=%s mode=%s forcedOn=%v disabled=%v doubleConfirm=%v: %s", actor, mode, forcedOn, disabled, rec.DoubleConfirm, rec.Reason),
		ActionID: fmt.Sprintf("engine-toggle:%s:%s:%s", mode, enabledWord(enable), actor),
		Withheld: !enable, // disabling the engine withholds autonomy.
	}
	if _, err := t.ledger.Append(govRec); err != nil {
		return ToggleRecord{}, fmt.Errorf("policy: engine toggle audit failed, engine state unchanged: %w", err)
	}

	t.mu.Lock()
	e := enable
	t.override = &e
	t.mu.Unlock()
	t.log("policy: engine toggle -> %s in %s by %s (forcedOn=%v disabled=%v)", enabledWord(enable), mode, actor, forcedOn, disabled)
	return rec, nil
}

// engineOffDecision is the decision the wrapper returns WHEN the admin toggle has DISABLED the engine: it defers
// to the mechanical floors. It reuses the engine's own fail-closed construction, so the verdict is `approve`
// (route to a human) and NEVER `auto`, and the never-auto floor's applicability to the action is recorded — the
// proof that engine-off is not a bypass. It records the loaded bundle version so the engine-off decision still
// names which rule set was in force (REQ-1522). It does NOT run the Rego composition (the engine is off) and
// does not touch core/safety.
func engineOffDecision(in EvalInput, bundleVer string) PolicyDecision {
	return failClosedDecision(in, bundleVer,
		"policy engine DISABLED by admin toggle — deferring to the mechanical floors: verdict `approve` (route to a human); the constitutional never-auto floor (INV-09) still clamps beneath, so a never-auto action cannot resolve to `auto`")
}

func enabledWord(enable bool) string {
	if enable {
		return "enabled"
	}
	return "disabled"
}

func (t *EngineToggle) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (t *EngineToggle) log(format string, args ...any) {
	if t.logf != nil {
		t.logf(format, args...)
	}
}
