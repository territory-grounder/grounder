package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/cpconfig"
)

// The control-plane config write surface (task #27 Phase C, spec/006 REQ-523): POST /v1/config/{key}
// {value, rationale}, admin-session-only (AuthAdminSession — a step-up-elevated operator; machine
// principals and plain sessions have no route here). The write executes in the WORKER (the governance
// ledger's single writer) through the distinctly-named configwrite.ConfigWriteWorkflow: ledger append
// BEFORE row commit. The LAW clamp is enforced three times — here (fast 422), in the worker activity
// (the authority), and at resolve time (an illegal row is ignored) — the clamp is the law.

// ConfigWriteRequest is the operator's override order. Rationale is mandatory.
type ConfigWriteRequest struct {
	Value     string `json:"value"`
	Rationale string `json:"rationale"`
}

// ConfigWriteOutcome is the committed override for the console: the key resolves source=console from
// the next read on, and the governance ledger holds the decision at LedgerSeq.
type ConfigWriteOutcome struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Source    string `json:"source"`
	LedgerSeq int64  `json:"ledger_seq"`
}

// ConfigWriter executes the ledgered write via the worker. nil = the surface fails closed to 503.
type ConfigWriter interface {
	WriteConfig(ctx context.Context, key, value, rationale, operator string) (ConfigWriteOutcome, error)
	// ClearConfig removes an override (TG-261). Same worker lane, same ledger-first discipline; the only
	// way an operator can take a saved setting back.
	ClearConfig(ctx context.Context, key, rationale, operator string) (ConfigWriteOutcome, error)
}

// adminWriteLimits is a small fixed-window per-operator rate limit shared by the admin write surfaces
// (defense in depth on top of the elevation TTL) — same mechanism as the vote limiter.
var adminWriteLimits voteLimiter

// adminWriteGuard is the shared admission for admin write handlers: POST-only, wired backend,
// same-origin, per-operator rate limit. The AuthAdminSession router class already authenticated the
// caller and proved the admin elevation.
func adminWriteGuard(w http.ResponseWriter, r *http.Request, wired bool, unavailable string, p auth.Principal) bool {
	return adminMutationGuard(w, r, http.MethodPost, wired, unavailable, p)
}

// adminMutationGuard is adminWriteGuard with the admitted method as a parameter, for the admin lanes
// whose verb is not POST (the config clear and the native-rule delete are DELETEs). Until TG-109 the
// POST check was hardcoded, which left DELETE /v1/config/{key} (TG-261) answering 405 through its own
// guard — an un-set lane that shipped unreachable; configClearHandler now passes its real method and the
// config-write suite pins the DELETE path.
func adminMutationGuard(w http.ResponseWriter, r *http.Request, method string, wired bool, unavailable string, p auth.Principal) bool {
	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !wired {
		http.Error(w, unavailable, http.StatusServiceUnavailable)
		return false
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin write rejected", http.StatusForbidden)
		return false
	}
	if !adminWriteLimits.allow(operatorOf(p), time.Now()) {
		http.Error(w, "write rate limit exceeded", http.StatusTooManyRequests)
		return false
	}
	return true
}

// configWriteErr maps the typed cpconfig refusals to honest statuses; anything else is retryable.
func configWriteErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, cpconfig.ErrUnknownKey):
		http.Error(w, "unknown configuration key", http.StatusNotFound)
	case errors.Is(err, cpconfig.ErrLawPinned):
		// 422: the request is well-formed; the write is UNPROCESSABLE by law. The clamp is the law.
		http.Error(w, "refused: key is pinned by law and can never be overridden", http.StatusUnprocessableEntity)
	case errors.Is(err, cpconfig.ErrNotWritable):
		http.Error(w, "refused: key is boot-only, not console-writable", http.StatusUnprocessableEntity)
	case errors.Is(err, cpconfig.ErrValueBounds):
		http.Error(w, "value out of bounds (1..2048 chars, no control characters)", http.StatusBadRequest)
	default:
		http.Error(w, "config write failed — retry", http.StatusServiceUnavailable)
	}
}

// configWriteHandler serves POST /v1/config/{key} (AuthAdminSession).
func (d Deps) configWriteHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !adminWriteGuard(w, r, d.ConfigWrite != nil, "config write path unavailable", p) {
		return
	}
	key := chi.URLParam(r, "key")
	if key == "" {
		http.Error(w, "configuration key required", http.StatusBadRequest)
		return
	}
	var req ConfigWriteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		http.Error(w, "malformed config write", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Rationale) == "" {
		http.Error(w, "rationale required — every override states why it exists", http.StatusBadRequest)
		return
	}
	// Fast surface validation (the worker re-validates as the authority).
	if _, err := cpconfig.ValidateWrite(key, req.Value); err != nil {
		configWriteErr(w, err)
		return
	}
	out, err := d.ConfigWrite.WriteConfig(r.Context(), key, req.Value, strings.TrimSpace(req.Rationale), operatorOf(p))
	if err != nil {
		configWriteErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// configKeyHandler dispatches the two verbs of /v1/config/{key}: POST sets an override, DELETE takes it
// back (TG-261). One handler because this Router matches on pattern alone — see the registration.
func (d Deps) configKeyHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	switch r.Method {
	case http.MethodDelete:
		d.configClearHandler(w, r, p)
	case http.MethodPost:
		d.configWriteHandler(w, r, p)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// configClearHandler serves DELETE /v1/config/{key} (AuthAdminSession) — the un-set (TG-261).
//
// Before this, the route was POST-only and ValidateWrite refuses an empty value, so an override could be
// corrected but never REMOVED. After TG-260 made the worker READ these rows, that gap had teeth: a value
// that stops the worker booting could only be undone by hand surgery on Postgres, because the only writer
// permitted to touch the table runs inside the worker that will not boot.
//
// A rationale is required exactly as it is for a write: taking a capability's configuration back is a
// governed decision, not a cleanup.
func (d Deps) configClearHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	// The DELETE-shaped guard: the shared POST-only adminWriteGuard 405'd every clear this handler was
	// dispatched (the configKeyHandler switch already proved the method is DELETE), so the un-set lane
	// shipped unreachable — fixed with adminMutationGuard (TG-109; see the guard's note).
	if !adminMutationGuard(w, r, http.MethodDelete, d.ConfigWrite != nil, "config write path unavailable", p) {
		return
	}
	key := chi.URLParam(r, "key")
	if key == "" {
		http.Error(w, "configuration key required", http.StatusBadRequest)
		return
	}
	var req ConfigWriteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		http.Error(w, "malformed config clear", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Rationale) == "" {
		http.Error(w, "rationale required — taking an override back is a decision, not a cleanup", http.StatusBadRequest)
		return
	}
	// Surface validation mirrors the write path: the worker re-validates as the authority.
	if _, err := cpconfig.ValidateClear(key); err != nil {
		configWriteErr(w, err)
		return
	}
	out, err := d.ConfigWrite.ClearConfig(r.Context(), key, strings.TrimSpace(req.Rationale), operatorOf(p))
	if err != nil {
		configWriteErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
