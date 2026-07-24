package policy

// This file wires the REAL mode-transition RBAC onto the AuthorityChecker seam that mode.go declares
// (spec/015 REQ-1502). mode.go built the seam and the fail-closed state machine standalone (T-015-4);
// here is the production authority the worker binds in place of the fail-closed `nil` — the LAST gate
// before an owner-present mutation flip. It changes NOTHING about the state machine: a transition still
// requires (a) an authenticated, authority-checked operator AND (b) a green preflight for any escalation
// into an actuating mode; wiring this authority only makes an operator-INVOKED transition ADMISSIBLE, it
// never auto-transitions anything, and the default/absent/corrupt mode still resolves to Shadow (REQ-1519).
//
// The flip-authorized set is populated two ways (the operator owns the dial):
//   1. An explicit operator allowlist — TG_MODE_TRANSITION_OPERATORS, comma-separated operator ids. When
//      set, ONLY those operators may flip the mode (a tighter, auditable gate — even an admin-group member
//      not on the list is denied).
//   2. The LDAP admin group the console login already resolves. An operator who authenticated as an
//      admin-group member (proven by the AuthAdminSession route the transition surface registers, carried
//      to this checker as a trusted in-process context signal) is authorized ONLY WHILE no explicit
//      allowlist narrows the set. This is the "LDAP admin group OR allowlist" the flip-authorized set means.
//
// Fail-CLOSED by construction: an empty/unknown/unauthenticated actor is denied; an actor neither on the
// allowlist nor an admin-group member is denied; and with NEITHER an allowlist configured NOR an admin
// signal present, no one is authorized (nobody can flip until the operator provisions the allowlist or logs
// in through the admin group). A denied transition changes nothing and is audited (AuditedModeTransition).
//
// Provenance: [O] INV-01 (mandatory auth, default-deny), INV-19 (audited transitions) · [R] spec/015
// REQ-1502 (authenticated, authority-checked mode change) · reuses spec/006 (the AuthAdminSession admin
// tier resolves the LDAP admin group at login) and the mode.go AuthorityChecker seam.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/audit"
)

// ErrEmptyModeChangeActor is returned when the acting operator id is empty (unauthenticated) — fail closed.
var ErrEmptyModeChangeActor = errors.New("policy: empty operator identity — no mode-change authority")

// modeChangeAdminKey carries the "this request's operator authenticated as an admin-group member" signal
// from the authenticated transition surface into the AuthorityChecker. It is set ONLY by the trusted
// server-side path (the worker's mode-transition activity, from the AuthAdminSession principal that the
// console login resolved against the LDAP admin group) — a caller-supplied request can never forge a Go
// context value, so the signal cannot be spoofed across the trust boundary.
type modeChangeAdminKey struct{}

// WithModeChangeAdmin marks the context as carrying an authenticated admin-group operator (an
// AuthAdminSession principal — an LDAP tg-admins member, or the static break-glass admin). The
// mode-transition activity sets this AFTER the auth layer proved the caller is an admin; the
// ModeTransitionAuthority treats it as satisfying the "LDAP admin group" branch of the flip-authorized set.
func WithModeChangeAdmin(ctx context.Context) context.Context {
	return context.WithValue(ctx, modeChangeAdminKey{}, true)
}

// modeChangeAdmin reports whether the context carries the trusted admin-group signal (default false — fail
// closed: an absent signal is treated as a non-admin operator).
func modeChangeAdmin(ctx context.Context) bool {
	v, _ := ctx.Value(modeChangeAdminKey{}).(bool)
	return v
}

// ModeTransitionAuthority is the production AuthorityChecker (REQ-1502): it authorizes a mode transition
// from the AUTHENTICATED operator's identity/role. It holds ONLY the non-secret operator allowlist (a set
// of normalized ids); the admin-group branch is a per-request context signal. It is immutable after
// construction and safe for concurrent use (reads only).
type ModeTransitionAuthority struct {
	// allow is the explicit operator allowlist (TG_MODE_TRANSITION_OPERATORS), normalized. Empty ⇒ no
	// explicit allowlist is configured; authorization then falls back to the admin-group branch alone.
	allow map[string]struct{}
}

// compile-time proof the production authority satisfies the mode.go seam.
var _ AuthorityChecker = (*ModeTransitionAuthority)(nil)

// NewModeTransitionAuthority builds the authority over an explicit operator allowlist. Each id is
// normalized (an "operator:" prefix stripped, trimmed, lower-cased) so a console principal id
// ("operator:kyriakosp") and a bare config id ("kyriakosp") compare equal. An empty/whitespace id is
// dropped. A nil/empty list means no explicit allowlist — authorization then rests on the admin-group
// branch alone (see HasModeChangeAuthority).
func NewModeTransitionAuthority(operators []string) *ModeTransitionAuthority {
	allow := make(map[string]struct{}, len(operators))
	for _, o := range operators {
		if n := normalizeModeOperator(o); n != "" {
			allow[n] = struct{}{}
		}
	}
	return &ModeTransitionAuthority{allow: allow}
}

// ParseOperatorAllowlist splits a comma-separated operator list (the TG_MODE_TRANSITION_OPERATORS env
// value) into ids. Empty entries are dropped; normalization happens in NewModeTransitionAuthority.
func ParseOperatorAllowlist(csv string) []string {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// AllowlistSize reports how many explicit operators are configured (for an honest boot log). Zero means the
// authority admits only admin-group operators (and only when an admin signal is present).
func (a *ModeTransitionAuthority) AllowlistSize() int { return len(a.allow) }

// normalizeModeOperator strips an "operator:" principal prefix, trims, and lower-cases an operator id so
// the allowlist compares case-insensitively (FreeIPA uids are case-insensitive) and independently of
// whether the id arrived as a bare name or a console principal id.
func normalizeModeOperator(actor string) string {
	s := strings.TrimSpace(actor)
	s = strings.TrimPrefix(s, "operator:")
	return strings.ToLower(strings.TrimSpace(s))
}

// HasModeChangeAuthority admits an operator to change the mode IFF they are in the flip-authorized set,
// fail-closed (REQ-1502):
//   - an empty operator id is denied (ErrEmptyModeChangeActor) — an unauthenticated actor never authorizes;
//   - an operator on the explicit allowlist is admitted (the config-allowlist branch);
//   - otherwise, an operator who authenticated as an admin-group member (the trusted context signal) is
//     admitted ONLY WHILE no explicit allowlist is configured to narrow the set (the LDAP-admin-group
//     branch) — a NON-EMPTY allowlist is the tighter gate and denies an admin who is not on it;
//   - everyone else is denied.
//
// It never mutates state and never auto-authorizes: it only answers "may THIS operator flip?" — the actual
// transition (and its green-preflight escalation gate) is still performed by ModeController.Transition.
func (a *ModeTransitionAuthority) HasModeChangeAuthority(ctx context.Context, actor string) error {
	op := normalizeModeOperator(actor)
	if op == "" {
		return ErrEmptyModeChangeActor
	}
	if _, ok := a.allow[op]; ok {
		return nil // explicit allowlist — the config-allowlist branch.
	}
	if len(a.allow) == 0 && modeChangeAdmin(ctx) {
		return nil // no explicit allowlist ⇒ an authenticated admin-group operator authorizes (LDAP admin group).
	}
	return fmt.Errorf("policy: operator %q is not in the flip-authorized set (fail closed)", op)
}

// AuditedModeTransition performs a gated mode transition and audits BOTH outcomes to the controller's own
// hash-chained governance ledger (REQ-1502 + the task's "DENIED + audited" obligation). It composes over
// ModeController.Transition WITHOUT changing it:
//   - on SUCCESS, Transition itself appended the immutable mode-transition record (prior→new mode, actor,
//     reason) BEFORE the new mode took effect — this helper adds nothing, so a success is recorded exactly
//     once;
//   - on REFUSAL (an unauthorized actor, a red preflight, a stale from-mode, or an unknown target), the
//     controller changed nothing and appended nothing, so this helper appends ONE non-secret
//     `mode-transition-refused` record (actor, from→to, the refusal reason) so a denied flip attempt is
//     never silent. The refusal audit is best-effort: the transition was already refused (fail closed), so
//     a ledger hiccup cannot un-refuse it.
//
// It returns the active mode AFTER the attempt (unchanged on refusal) and the transition error verbatim.
func AuditedModeTransition(ctx context.Context, ctrl *ModeController, from, to Mode, actor, reason string) (Mode, error) {
	err := ctrl.Transition(ctx, from, to, actor, reason)
	if err != nil {
		if ctrl.ledger != nil {
			// Withheld: a refused transition withholds autonomy — it is the fail-closed outcome.
			_, _ = ctrl.ledger.Append(audit.GovDecision{
				Decision: fmt.Sprintf("mode-transition-refused:%s->%s", from, to),
				Reason:   fmt.Sprintf("actor=%s reason=%s refused=%v", actor, reason, err),
				ActionID: fmt.Sprintf("mode-transition-refused:%s->%s:%s", from, to, actor),
				Withheld: true,
			})
		}
		return ctrl.Current(), err
	}
	return ctrl.Current(), nil
}
