package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/policy"
)

// The autonomy-mode transition surface (spec/015 REQ-1502): POST /v1/mode {to, expected_from?, reason},
// admin-session-only (AuthAdminSession — a step-up-elevated operator; machine principals and plain
// sessions have NO route here). This is the LAST gate before an owner-present mutation flip. The write
// executes in the WORKER on the single live, chokepoint-bound ModeController through the distinctly-named
// modetransition.ModeTransitionWorkflow: the wired AuthorityChecker gates on the operator being
// flip-authorized AND, for any escalation into an actuating mode, the green boot preflight; the immutable
// mode-transition record is appended by the worker (the ledger's single writer) before the new mode takes
// effect, and a DENIED attempt is audited too.
//
// This surface ENABLES an operator-invoked transition; it never auto-transitions anything. The operator
// identity + admin-tier proof are DERIVED from the authenticated principal (never the body): reaching this
// handler already proves the caller is an admin-session operator (the LDAP admin group the console login
// resolved, or the static break-glass admin), which is the "LDAP admin group" branch of the flip-authorized
// set. Mutation stays OFF until an operator actually posts a flip here.

// ModeTransitionRequest is the operator's transition order. Reason is mandatory (every governed change
// states why). ExpectedFrom is an OPTIONAL compare-and-swap guard against a stale console view.
type ModeTransitionRequest struct {
	To           string `json:"to"`                      // canonical target mode name
	ExpectedFrom string `json:"expected_from,omitempty"` // optional expected current mode
	Reason       string `json:"reason"`
}

// ModeTransitionOutcome is the committed transition for the console: the active mode AFTER the flip.
type ModeTransitionOutcome struct {
	Mode string `json:"mode"` // the active mode after the transition (unchanged on a refusal error)
	From string `json:"from"`
	To   string `json:"to"`
}

// ModeTransitioner executes the ledgered, gated mode transition via the worker. nil = the surface fails
// closed to 503. adminAuthorized reflects the AuthAdminSession principal (the LDAP admin group / static
// admin), carried to the worker's AuthorityChecker as the trusted admin-group signal.
type ModeTransitioner interface {
	TransitionMode(ctx context.Context, to, expectedFrom, reason, operator string, adminAuthorized bool) (ModeTransitionOutcome, error)
}

// modeTransitionErr maps the typed policy refusals to honest statuses; anything else is retryable.
func modeTransitionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, policy.ErrUnauthorizedModeChange), errors.Is(err, policy.ErrEmptyModeChangeActor):
		// Authenticated (the route proved it) but not flip-authorized — a genuine authorization refusal.
		http.Error(w, "refused: operator is not in the flip-authorized set", http.StatusForbidden)
	case errors.Is(err, policy.ErrPreflightNotGreen):
		// Well-formed, but the system is not in a flippable state (the boot preflight is not green).
		http.Error(w, "refused: cannot escalate — the boot preflight is not green", http.StatusConflict)
	case errors.Is(err, policy.ErrStaleMode):
		http.Error(w, "refused: expected_from no longer matches the active mode — re-read and retry", http.StatusConflict)
	case errors.Is(err, policy.ErrUnknownMode):
		http.Error(w, "unknown target mode (want Shadow | HITL | Semi-auto | Full-auto)", http.StatusBadRequest)
	default:
		http.Error(w, "mode transition failed — retry", http.StatusServiceUnavailable)
	}
}

// modeTransitionHandler serves POST /v1/mode (AuthAdminSession).
func (d Deps) modeTransitionHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !adminWriteGuard(w, r, d.ModeTransition != nil, "mode transition path unavailable", p) {
		return
	}
	var req ModeTransitionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		http.Error(w, "malformed mode transition", http.StatusBadRequest)
		return
	}
	to := strings.TrimSpace(req.To)
	if to == "" {
		http.Error(w, "target mode required", http.StatusBadRequest)
		return
	}
	// Fast surface validation of the mode names (the worker re-validates as the authority).
	if _, err := policy.ParseMode(to); err != nil {
		http.Error(w, "unknown target mode (want Shadow | HITL | Semi-auto | Full-auto)", http.StatusBadRequest)
		return
	}
	expectedFrom := strings.TrimSpace(req.ExpectedFrom)
	if expectedFrom != "" {
		if _, err := policy.ParseMode(expectedFrom); err != nil {
			http.Error(w, "unknown expected_from mode", http.StatusBadRequest)
			return
		}
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		http.Error(w, "rationale required — every mode change states why it exists", http.StatusBadRequest)
		return
	}
	// operator + admin proof come from the AUTHENTICATED principal, never the body (INV-01). Reaching an
	// AuthAdminSession route means p.Admin is true (the LDAP admin group / static admin).
	out, err := d.ModeTransition.TransitionMode(r.Context(), to, expectedFrom, reason, operatorOf(p), p.Admin)
	if err != nil {
		modeTransitionErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// MemModeTransitioner is the in-memory ModeTransitioner twin for the CI oracles (no Temporal/worker). It
// returns a canned outcome/error and records the last call so a test can assert the handler derived the
// operator + admin proof from the principal (not the body) and forwarded the validated fields.
type MemModeTransitioner struct {
	Outcome          ModeTransitionOutcome
	Err              error
	LastTo           string
	LastExpectedFrom string
	LastReason       string
	LastOperator     string
	LastAdmin        bool
	Calls            int
}

// TransitionMode records the call and returns the canned result.
func (m *MemModeTransitioner) TransitionMode(_ context.Context, to, expectedFrom, reason, operator string, adminAuthorized bool) (ModeTransitionOutcome, error) {
	m.Calls++
	m.LastTo, m.LastExpectedFrom, m.LastReason, m.LastOperator, m.LastAdmin = to, expectedFrom, reason, operator, adminAuthorized
	if m.Err != nil {
		return ModeTransitionOutcome{}, m.Err
	}
	return m.Outcome, nil
}

// compile-time proof the in-memory twin satisfies the interface.
var _ ModeTransitioner = (*MemModeTransitioner)(nil)

// ModeTransitionHandlerForAcceptance exposes the unexported handler to an external acceptance oracle
// driving the REAL router/handler (it adds no behavior — the oracle exercises exactly what production
// serves).
func (d Deps) ModeTransitionHandlerForAcceptance(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	d.modeTransitionHandler(w, r, p)
}
