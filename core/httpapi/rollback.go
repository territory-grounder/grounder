package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
)

// The operator-facing MANUAL ROLLBACK surface (TG-462): POST /v1/actions/{action_id}/rollback, admin-session-only
// (AuthAdminSession — a step-up-elevated operator; machine principals and plain sessions have NO route here).
// It triggers the INVERSE of a previously-EXECUTED forward action over the governed actuation chain: the WORKER
// seals a fresh content-hashed inverse ActionManifest, binds the forward execution record as evidence, classifies
// it POLL_PAUSE (a human must approve), and only then hands it to the interceptor with InvertsActionID set. It is
// INERT under Shadow/HITL — the chain refuses at the mode chokepoint before any effect. The endpoint does a
// SYNCHRONOUS pre-check so an operator learns fast that a target is unknown (404), was never executed (404), is
// not cleanly reversible (400), or has already been rolled back (409); on success it starts the workflow and
// returns the sealed inverse action_id + pending-approval status. The operator identity is DERIVED from the
// authenticated principal (never the body) — reaching this handler already proves an admin-session operator.

// RollbackRequested is the surface's response: the sealed inverse's identity + where the approval now lives.
type RollbackRequested struct {
	ForwardActionID string `json:"forward_action_id"`
	InverseActionID string `json:"inverse_action_id"` // the inverse's OWN content-addressed id (distinct from the forward)
	WorkflowID      string `json:"workflow_id"`
	Band            string `json:"band"`   // always POLL_PAUSE — a manual rollback is human-approved by construction
	Status          string `json:"status"` // "pending-approval": the inverse is sealed and awaiting a human vote
}

// Rollbacker starts the governed manual-rollback workflow for a forward action. nil = the surface fails closed
// to 503. It performs the synchronous pre-check and returns the typed refusals below, or a started workflow.
type Rollbacker interface {
	StartRollback(ctx context.Context, forwardActionID, operator string) (RollbackRequested, error)
}

// The typed refusals the backend returns; the handler maps each to an honest status. Anything else is retryable.
var (
	// ErrRollbackUnknownAction — no sealed manifest OR no execution record for the id (unknown / never-executed).
	ErrRollbackUnknownAction = errors.New("rollback: unknown or never-executed action")
	// ErrRollbackIrreversible — the forward op-class is not cleanly reversible (medium / irreversible /
	// vendor-critical / unregistered), so no safe inverse exists. Reversible-only, fail closed.
	ErrRollbackIrreversible = errors.New("rollback: action is not cleanly reversible")
	// ErrRollbackAlreadyInverted — the action was already rolled back, or is itself a rollback (no double-undo).
	ErrRollbackAlreadyInverted = errors.New("rollback: action already has an inverse (no double-undo)")
)

// rollbackErr maps the typed backend refusals to honest statuses.
func rollbackErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrRollbackUnknownAction):
		http.Error(w, "unknown or never-executed action — only a previously-executed forward action can be rolled back", http.StatusNotFound)
	case errors.Is(err, ErrRollbackIrreversible):
		http.Error(w, "refused: action is not cleanly reversible — a manual rollback is permitted only for reversible op-classes", http.StatusBadRequest)
	case errors.Is(err, ErrRollbackAlreadyInverted):
		http.Error(w, "refused: this action has already been rolled back (or is itself a rollback) — no double-undo", http.StatusConflict)
	default:
		http.Error(w, "rollback failed — retry", http.StatusServiceUnavailable)
	}
}

// rollbackHandler serves POST /v1/actions/{action_id}/rollback (AuthAdminSession).
func (d Deps) rollbackHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !adminWriteGuard(w, r, d.Rollback != nil, "rollback path unavailable", p) {
		return
	}
	forwardID := strings.TrimSpace(chi.URLParam(r, "action_id"))
	if forwardID == "" {
		http.Error(w, "action_id required", http.StatusBadRequest)
		return
	}
	// operator comes from the AUTHENTICATED principal, never the body (INV-01). Reaching an AuthAdminSession
	// route means p.Admin is true (the LDAP admin group / static admin).
	out, err := d.Rollback.StartRollback(r.Context(), forwardID, operatorOf(p))
	if err != nil {
		rollbackErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted) // 202: the inverse is sealed and the approval poll is pending
	_ = json.NewEncoder(w).Encode(out)
}

// MemRollbacker is the in-memory Rollbacker twin for the CI oracles (no Temporal/worker). It returns a canned
// result/error and records the last call so a test can assert the handler derived the operator from the principal
// (not the body) and forwarded the path action_id.
type MemRollbacker struct {
	Out           RollbackRequested
	Err           error
	LastForwardID string
	LastOperator  string
	Calls         int
}

// StartRollback records the call and returns the canned result.
func (m *MemRollbacker) StartRollback(_ context.Context, forwardActionID, operator string) (RollbackRequested, error) {
	m.Calls++
	m.LastForwardID, m.LastOperator = forwardActionID, operator
	if m.Err != nil {
		return RollbackRequested{}, m.Err
	}
	return m.Out, nil
}

// compile-time proof the in-memory twin satisfies the interface.
var _ Rollbacker = (*MemRollbacker)(nil)

// RollbackHandlerForAcceptance exposes the unexported handler to an external acceptance oracle driving the REAL
// router/handler (it adds no behavior — the oracle exercises exactly what production serves).
func (d Deps) RollbackHandlerForAcceptance(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	d.rollbackHandler(w, r, p)
}
