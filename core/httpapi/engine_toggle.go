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

// The policy-engine enable/disable surface (spec/015 REQ-1519, "the operator owns the paranoia dial"): POST
// /v1/policy/engine-toggle {enable, reason, double_confirm?}, admin-session-only (AuthAdminSession — a
// step-up-elevated operator; machine principals and plain sessions have NO route here). This is a
// warn-don't-block control: it never BLOCKS a permissive posture (REQ-1517), but a risky change is made LOUD.
//
// The write executes in the WORKER on the single live EngineToggle (the ledger's single writer) through the
// distinctly-named enginetoggle.EngineToggleWorkflow: the wired AuthorityChecker gates on the operator being
// toggle-authorized; forcing the engine ON in a read-only mode needs a single acknowledgement (the mandatory
// reason), and DISABLING it in an actuating mode needs a DISTINCT red double-confirmation. The immutable
// toggle record is appended by the worker before the effect; a denied attempt is audited too. The
// constitutional never-auto floor (INV-09) is unaffected: engine-off routes to a human (`approve`), never
// `auto`. Operator + admin proof are DERIVED from the authenticated principal, never the body (INV-01).

// EngineToggleRequest is the operator's toggle order. Reason is mandatory (the audited acknowledgement text).
// DoubleConfirm is the DISTINCT red confirmation required to DISABLE the engine in an actuating mode.
type EngineToggleRequest struct {
	Enable        bool   `json:"enable"`
	Reason        string `json:"reason"`
	DoubleConfirm bool   `json:"double_confirm,omitempty"`
}

// EngineToggleOutcome is the committed toggle for the console: the engine's effective state + any warning.
type EngineToggleOutcome struct {
	Enabled     bool   `json:"enabled"`
	Mode        string `json:"mode"`
	WarningCode string `json:"warning_code,omitempty"`
	WarningText string `json:"warning_text,omitempty"`
}

// EngineToggler executes the ledgered, gated engine toggle via the worker. nil = the surface fails closed to
// 503. adminAuthorized reflects the AuthAdminSession principal, carried to the worker's AuthorityChecker as
// the trusted admin-group signal.
type EngineToggler interface {
	ToggleEngine(ctx context.Context, enable bool, reason, operator string, adminAuthorized, doubleConfirm bool) (EngineToggleOutcome, error)
}

// engineToggleErr maps the typed policy refusals to honest statuses; anything else (incl. "not armed") is
// unavailable/retryable.
func engineToggleErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, policy.ErrUnauthorizedEngineToggle):
		http.Error(w, "refused: operator is not in the engine-toggle-authorized set", http.StatusForbidden)
	case errors.Is(err, policy.ErrEngineToggleNotConfirmed):
		http.Error(w, "refused: this change requires acknowledgement — disabling the engine in an actuating mode needs the red double-confirmation", http.StatusConflict)
	default:
		// Includes enginetoggle.ErrNoToggle (the toggle is not armed on this deployment) — fail closed.
		http.Error(w, "engine toggle unavailable — retry", http.StatusServiceUnavailable)
	}
}

// engineToggleHandler serves POST /v1/policy/engine-toggle (AuthAdminSession).
func (d Deps) engineToggleHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !adminWriteGuard(w, r, d.EngineToggle != nil, "engine toggle path unavailable", p) {
		return
	}
	var req EngineToggleRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		http.Error(w, "malformed engine toggle", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		http.Error(w, "rationale required — every engine toggle states why it exists", http.StatusBadRequest)
		return
	}
	// operator + admin proof come from the AUTHENTICATED principal, never the body (INV-01). Reaching an
	// AuthAdminSession route means p.Admin is true (the LDAP admin group / static break-glass admin).
	out, err := d.EngineToggle.ToggleEngine(r.Context(), req.Enable, reason, operatorOf(p), p.Admin, req.DoubleConfirm)
	if err != nil {
		engineToggleErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// MemEngineToggler is the in-memory EngineToggler twin for the CI oracles (no Temporal/worker). It records the
// last call so a test can assert the handler derived operator + admin from the principal (not the body).
type MemEngineToggler struct {
	Outcome           EngineToggleOutcome
	Err               error
	LastEnable        bool
	LastReason        string
	LastOperator      string
	LastAdmin         bool
	LastDoubleConfirm bool
	Calls             int
}

// ToggleEngine records the call and returns the canned result.
func (m *MemEngineToggler) ToggleEngine(_ context.Context, enable bool, reason, operator string, adminAuthorized, doubleConfirm bool) (EngineToggleOutcome, error) {
	m.Calls++
	m.LastEnable, m.LastReason, m.LastOperator, m.LastAdmin, m.LastDoubleConfirm = enable, reason, operator, adminAuthorized, doubleConfirm
	if m.Err != nil {
		return EngineToggleOutcome{}, m.Err
	}
	return m.Outcome, nil
}

// compile-time proof the in-memory twin satisfies the interface.
var _ EngineToggler = (*MemEngineToggler)(nil)

// EngineToggleHandlerForAcceptance exposes the unexported handler to an external acceptance oracle driving the
// REAL router/handler (it adds no behavior — the oracle exercises exactly what production serves).
func (d Deps) EngineToggleHandlerForAcceptance(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	d.engineToggleHandler(w, r, p)
}
