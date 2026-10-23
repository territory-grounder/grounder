package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
)

// The DB-backed OBJECT GROUP surface (spec/016, TG-481) — named sets of host-glob patterns the policy and
// credential engines union into a target's group membership:
//   - GET    /v1/estate/groups              (AuthTraceRead)    — the operator-authored object groups
//   - POST   /v1/estate/groups/entries      (AuthAdminSession) — add ONE group {name, patterns[], precedence?, rationale}
//   - DELETE /v1/estate/groups/entries/{id} (AuthAdminSession) — remove one group {rationale}
//
// The writes execute in the WORKER (the governance ledger's single writer) through the distinctly-named
// objectgroup.ObjectGroupWriteWorkflow: the payload is VALIDATED (non-empty name, at least one host-glob
// pattern, a recognized precedence), ledgered BEFORE the row commits, then persisted. The grounder NEVER
// writes estate_object_group itself. The read sits at the trace-read tier because group membership reveals
// actuation-policy structure, though it carries no secret value. The writes ride DISTINCT /entries patterns
// (not verbs on /v1/estate/groups): this Router keys by PATTERN and the GET carries a different auth class, so
// one pattern cannot serve both tiers — the native-rule discipline.

// ErrNoSuchObjectGroup is the typed not-found refusal the worker backend maps a delete of a missing row onto.
var ErrNoSuchObjectGroup = errors.New("no such object group")

// ErrObjectGroupNotAdmin is the worker's fail-closed non-admin refusal, surfaced as 403.
var ErrObjectGroupNotAdmin = errors.New("object-group write refused: not an admin-tier operator")

// ErrObjectGroupInvalid wraps a WORKER-side validation refusal (defense in depth) so the handler returns the
// validator's text as a 400.
var ErrObjectGroupInvalid = errors.New("object group refused")

// maxObjectGroupBytes bounds the write bodies. A name + a handful of patterns + rationale is small.
const maxObjectGroupBytes = 8 << 10

// ObjectGroup is one operator-authored object group. Name is referenced by KindGroup selectors; Patterns are
// host-globs. No secret value is carried (INV-13 does not apply — these are host names + globs).
type ObjectGroup struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Patterns   []string `json:"patterns"`
	Precedence string   `json:"precedence"`
	CreatedBy  string   `json:"created_by"`
	CreatedAt  string   `json:"created_at"`
}

// ObjectGroupsPage is what GET /v1/estate/groups renders.
type ObjectGroupsPage struct {
	Groups []ObjectGroup `json:"groups"`
	Total  int           `json:"total"`
}

// ObjectGroupsReader lists the operator-authored object groups. nil ⇒ the read fails closed to 503.
type ObjectGroupsReader interface {
	ObjectGroups(ctx context.Context) ([]ObjectGroup, error)
}

// ObjectGroupWriteOutcome is the committed write for the console: the row id created/removed + the
// governance-ledger sequence the decision landed at.
type ObjectGroupWriteOutcome struct {
	ID        int64 `json:"id"`
	LedgerSeq int64 `json:"ledger_seq"`
}

// ObjectGroupWriter executes the validated, ledgered object-group write via the worker. nil = the surface
// fails closed to 503. admin reflects the AuthAdminSession principal, carried to the worker (which re-checks).
type ObjectGroupWriter interface {
	AddObjectGroup(ctx context.Context, name string, patterns []string, precedence, rationale, operator string, admin bool) (ObjectGroupWriteOutcome, error)
	DeleteObjectGroup(ctx context.Context, id int64, rationale, operator string, admin bool) (ObjectGroupWriteOutcome, error)
}

// objectGroupsHandler serves GET /v1/estate/groups (AuthTraceRead).
func (d Deps) objectGroupsHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if d.ObjectGroupRead == nil {
		http.Error(w, "object groups unavailable (no durable store is wired)", http.StatusServiceUnavailable)
		return
	}
	groups, err := d.ObjectGroupRead.ObjectGroups(r.Context())
	if err != nil {
		http.Error(w, "object groups unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ObjectGroupsPage{Groups: groups, Total: len(groups)})
}

// objectGroupWriteErr maps a worker refusal to an honest status; anything else is retryable.
func objectGroupWriteErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNoSuchObjectGroup):
		http.Error(w, "no such object group", http.StatusNotFound)
	case errors.Is(err, ErrObjectGroupNotAdmin):
		http.Error(w, "refused: object-group writes require an admin-tier operator", http.StatusForbidden)
	case errors.Is(err, ErrObjectGroupInvalid):
		http.Error(w, "refused: "+err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, "object-group write failed — retry", http.StatusServiceUnavailable)
	}
}

// ObjectGroupAddRequest is the operator's add order. Patterns is at least one host-glob; precedence is
// optional (defaults to 'union'); rationale is mandatory.
type ObjectGroupAddRequest struct {
	Name       string   `json:"name"`
	Patterns   []string `json:"patterns"`
	Precedence string   `json:"precedence"`
	Rationale  string   `json:"rationale"`
}

// ObjectGroupDeleteRequest carries the delete's mandatory rationale (taking a group back is a governed decision).
type ObjectGroupDeleteRequest struct {
	Rationale string `json:"rationale"`
}

// objectGroupAddHandler serves POST /v1/estate/groups (AuthAdminSession).
func (d Deps) objectGroupAddHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !adminMutationGuard(w, r, http.MethodPost, d.ObjectGroupWrite != nil, "object-group write path unavailable", p) {
		return
	}
	var req ObjectGroupAddRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxObjectGroupBytes)).Decode(&req); err != nil {
		http.Error(w, "malformed object-group write", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		http.Error(w, "name required (the group name a KindGroup selector references)", http.StatusBadRequest)
		return
	}
	// keep only non-empty, trimmed patterns; at least one is required.
	patterns := make([]string, 0, len(req.Patterns))
	for _, pat := range req.Patterns {
		if pat = strings.TrimSpace(pat); pat != "" {
			patterns = append(patterns, pat)
		}
	}
	if len(patterns) == 0 {
		http.Error(w, "at least one non-empty host-glob pattern required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Rationale) == "" {
		http.Error(w, "rationale required — every object group states why it exists", http.StatusBadRequest)
		return
	}
	out, err := d.ObjectGroupWrite.AddObjectGroup(r.Context(), strings.TrimSpace(req.Name), patterns,
		strings.TrimSpace(req.Precedence), strings.TrimSpace(req.Rationale), operatorOf(p), p.Admin)
	if err != nil {
		objectGroupWriteErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// objectGroupDeleteHandler serves DELETE /v1/estate/groups/{id} (AuthAdminSession).
func (d Deps) objectGroupDeleteHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !adminMutationGuard(w, r, http.MethodDelete, d.ObjectGroupWrite != nil, "object-group write path unavailable", p) {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "a positive numeric group id is required", http.StatusBadRequest)
		return
	}
	var req ObjectGroupDeleteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxObjectGroupBytes)).Decode(&req); err != nil {
		http.Error(w, "malformed object-group delete", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Rationale) == "" {
		http.Error(w, "rationale required — taking a group back is a decision, not a cleanup", http.StatusBadRequest)
		return
	}
	out, err := d.ObjectGroupWrite.DeleteObjectGroup(r.Context(), id, strings.TrimSpace(req.Rationale), operatorOf(p), p.Admin)
	if err != nil {
		objectGroupWriteErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// MemObjectGroupsReader is the in-memory ObjectGroupsReader twin for the CI oracles.
type MemObjectGroupsReader struct {
	Groups []ObjectGroup
	Err    error
}

// ObjectGroups returns the canned rows.
func (m *MemObjectGroupsReader) ObjectGroups(context.Context) ([]ObjectGroup, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	return m.Groups, nil
}

// MemObjectGroupWriter is the in-memory ObjectGroupWriter twin for the CI oracles (no Temporal/worker),
// recording the last call so a test can assert the handler derived operator/admin from the principal.
type MemObjectGroupWriter struct {
	Outcome       ObjectGroupWriteOutcome
	Err           error
	LastVerb      string
	LastName      string
	LastPatterns  []string
	LastID        int64
	LastRationale string
	LastOperator  string
	LastAdmin     bool
	Calls         int
}

// AddObjectGroup records the call and returns the canned result.
func (m *MemObjectGroupWriter) AddObjectGroup(_ context.Context, name string, patterns []string, precedence, rationale, operator string, admin bool) (ObjectGroupWriteOutcome, error) {
	m.Calls++
	m.LastVerb, m.LastName, m.LastPatterns, m.LastRationale, m.LastOperator, m.LastAdmin = "add", name, patterns, rationale, operator, admin
	if m.Err != nil {
		return ObjectGroupWriteOutcome{}, m.Err
	}
	return m.Outcome, nil
}

// DeleteObjectGroup records the call and returns the canned result.
func (m *MemObjectGroupWriter) DeleteObjectGroup(_ context.Context, id int64, rationale, operator string, admin bool) (ObjectGroupWriteOutcome, error) {
	m.Calls++
	m.LastVerb, m.LastID, m.LastRationale, m.LastOperator, m.LastAdmin = "delete", id, rationale, operator, admin
	if m.Err != nil {
		return ObjectGroupWriteOutcome{}, m.Err
	}
	return m.Outcome, nil
}

// compile-time proof the in-memory twins satisfy the interfaces.
var (
	_ ObjectGroupsReader = (*MemObjectGroupsReader)(nil)
	_ ObjectGroupWriter  = (*MemObjectGroupWriter)(nil)
)
