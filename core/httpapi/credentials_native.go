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
	"github.com/territory-grounder/grounder/core/credential"
)

// The DB-backed NATIVE per-target credential mapping surface (spec/016 REQ-1610, TG-109):
//   - GET  /v1/credentials/native            (AuthTraceRead)   — the operator-authored rule rows
//   - POST /v1/credentials/native/rules      (AuthAdminSession) — add ONE rule {entry, rationale}
//   - DELETE /v1/credentials/native/rules/{id} (AuthAdminSession) — remove one rule {rationale}
//
// The writes execute in the WORKER (the governance ledger's single writer) through the distinctly-named
// nativerule.NativeRuleWriteWorkflow: the entry is VALIDATED via core/credential.ParseRules (exactly ONE
// rule — a malformed or multi-rule entry is refused, never stored where the sync source would then fail
// every sync on it), ledgered BEFORE the row commits, then persisted. The grounder NEVER writes the rule
// table itself. The rows land in resolution through the registered nativedb sync source — a console write
// needs NO worker restart.
//
// The write routes live on the DISTINCT /rules pattern rather than as verbs on /v1/credentials/native:
// this Router registers by PATTERN (a second Handle on one pattern REPLACES the first — see the
// /v1/config/{key} registration note), and the GET carries a DIFFERENT auth class (AuthTraceRead) than
// the writes (AuthAdminSession), so one pattern could not carry both tiers.

// ErrNoSuchNativeRule is the typed not-found refusal the worker backend maps a delete of a missing row
// onto, so the handler returns an honest 404. It lives here (not in a temporal package) so the handler
// maps it without depending on the worker packages, exactly as the ruleset conflict sentinel does.
var ErrNoSuchNativeRule = errors.New("no such native credential rule")

// ErrNativeRuleNotAdmin is the worker's fail-closed non-admin refusal, surfaced as 403. Reaching the
// handler already proves an admin session, so seeing this means the principal was not forwarded — an
// authorization refusal, not a retryable fault.
var ErrNativeRuleNotAdmin = errors.New("native rule write refused: not an admin-tier operator")

// ErrNativeRuleInvalid wraps a WORKER-side validation refusal (defense in depth — the surface already
// validates with the same parser) so the handler can return the validator's text as a 400.
var ErrNativeRuleInvalid = errors.New("native rule refused")

// maxNativeRuleBytes bounds the write bodies. One packed rule + rationale is tiny; 8 KiB is generous.
const maxNativeRuleBytes = 8 << 10

// NativeRule is one operator-authored rule row of the native mapping. Entry is the packed ParseRules rule
// and carries SecretRef REFERENCE strings only, never a secret value (INV-13) — which is exactly why the
// read surface sits at the elevated trace-read tier (see the route).
type NativeRule struct {
	ID        int64  `json:"id"`
	Entry     string `json:"entry"`
	Rationale string `json:"rationale"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

// NativeRulesPage is what GET /v1/credentials/native renders.
type NativeRulesPage struct {
	Rules []NativeRule `json:"rules"`
	Total int          `json:"total"`
}

// NativeRulesReader lists the operator-authored native rule rows. nil ⇒ the read fails closed to 503.
type NativeRulesReader interface {
	NativeRules(ctx context.Context) ([]NativeRule, error)
}

// NativeRuleWriteOutcome is the committed write for the console: the row id created/removed and the
// governance-ledger sequence the decision landed at.
type NativeRuleWriteOutcome struct {
	ID        int64 `json:"id"`
	LedgerSeq int64 `json:"ledger_seq"`
}

// NativeRuleWriter executes the validated, ledgered native-rule write via the worker. nil = the surface
// fails closed to 503. admin reflects the AuthAdminSession principal, carried to the worker (which
// re-checks it — the surface is never the only line).
type NativeRuleWriter interface {
	AddNativeRule(ctx context.Context, entry, rationale, operator string, admin bool) (NativeRuleWriteOutcome, error)
	DeleteNativeRule(ctx context.Context, id int64, rationale, operator string, admin bool) (NativeRuleWriteOutcome, error)
}

// credentialNativeRulesHandler serves GET /v1/credentials/native (AuthTraceRead — see the route's tier
// rationale).
func (d Deps) credentialNativeRulesHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if d.CredentialNativeRead == nil {
		// A deployment whose worker holds no DB pool has no rule table to serve — said plainly rather
		// than an empty list that reads as "no rules configured".
		http.Error(w, "native credential rules unavailable (no durable store is wired)", http.StatusServiceUnavailable)
		return
	}
	rules, err := d.CredentialNativeRead.NativeRules(r.Context())
	if err != nil {
		http.Error(w, "native credential rules unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(NativeRulesPage{Rules: rules, Total: len(rules)})
}

// nativeRuleWriteErr maps a worker refusal to an honest status; anything else is retryable.
func nativeRuleWriteErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNoSuchNativeRule):
		http.Error(w, "no such native rule", http.StatusNotFound)
	case errors.Is(err, ErrNativeRuleNotAdmin):
		http.Error(w, "refused: native rule writes require an admin-tier operator", http.StatusForbidden)
	case errors.Is(err, ErrNativeRuleInvalid):
		// Defense in depth: the worker re-validated (the authority) and refused — its text, verbatim.
		http.Error(w, "refused: "+err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, "native rule write failed — retry", http.StatusServiceUnavailable)
	}
}

// NativeRuleAddRequest is the operator's add order. Entry is ONE packed ParseRules rule; rationale is
// mandatory (every native rule states why it exists).
type NativeRuleAddRequest struct {
	Entry     string `json:"entry"`
	Rationale string `json:"rationale"`
}

// NativeRuleDeleteRequest carries the delete's mandatory rationale (taking a mapping back is a governed
// decision, not a cleanup — the config-clear precedent).
type NativeRuleDeleteRequest struct {
	Rationale string `json:"rationale"`
}

// nativeRuleAddHandler serves POST /v1/credentials/native/rules (AuthAdminSession).
func (d Deps) nativeRuleAddHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !adminMutationGuard(w, r, http.MethodPost, d.NativeRuleWrite != nil, "native rule write path unavailable", p) {
		return
	}
	var req NativeRuleAddRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxNativeRuleBytes)).Decode(&req); err != nil {
		http.Error(w, "malformed native rule write", http.StatusBadRequest)
		return
	}
	entry := strings.TrimSpace(req.Entry)
	if entry == "" {
		http.Error(w, "entry required (kind:pattern|user|port|scheme[|refs…])", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Rationale) == "" {
		http.Error(w, "rationale required — every native rule states why it exists", http.StatusBadRequest)
		return
	}
	// Fast surface validation (the worker re-validates as the authority): the entry must parse to EXACTLY
	// ONE rule. The parser's refusal surfaces verbatim, mirroring the ruleset write's 400.
	rules, err := credential.ParseRules(entry)
	if err != nil {
		http.Error(w, "refused: malformed native rule — "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(rules) != 1 {
		http.Error(w, "refused: the entry packs more than one rule — one row, one rule (split into separate adds)", http.StatusBadRequest)
		return
	}
	// operator + admin proof come from the AUTHENTICATED principal, never the body (INV-01).
	out, err := d.NativeRuleWrite.AddNativeRule(r.Context(), entry, strings.TrimSpace(req.Rationale), operatorOf(p), p.Admin)
	if err != nil {
		nativeRuleWriteErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// nativeRuleDeleteHandler serves DELETE /v1/credentials/native/rules/{id} (AuthAdminSession).
func (d Deps) nativeRuleDeleteHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !adminMutationGuard(w, r, http.MethodDelete, d.NativeRuleWrite != nil, "native rule write path unavailable", p) {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "a positive numeric rule id is required", http.StatusBadRequest)
		return
	}
	var req NativeRuleDeleteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxNativeRuleBytes)).Decode(&req); err != nil {
		http.Error(w, "malformed native rule delete", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Rationale) == "" {
		http.Error(w, "rationale required — taking a mapping back is a decision, not a cleanup", http.StatusBadRequest)
		return
	}
	out, err := d.NativeRuleWrite.DeleteNativeRule(r.Context(), id, strings.TrimSpace(req.Rationale), operatorOf(p), p.Admin)
	if err != nil {
		nativeRuleWriteErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// MemNativeRulesReader is the in-memory NativeRulesReader twin for the CI oracles.
type MemNativeRulesReader struct {
	Rules []NativeRule
	Err   error
}

// NativeRules returns the canned rows.
func (m *MemNativeRulesReader) NativeRules(context.Context) ([]NativeRule, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	return m.Rules, nil
}

// MemNativeRuleWriter is the in-memory NativeRuleWriter twin for the CI oracles (no Temporal/worker). It
// returns a canned outcome/error and records the last call so a test can assert the handler derived the
// operator + admin proof from the principal (not the body) and forwarded the validated fields.
type MemNativeRuleWriter struct {
	Outcome       NativeRuleWriteOutcome
	Err           error
	LastVerb      string
	LastEntry     string
	LastID        int64
	LastRationale string
	LastOperator  string
	LastAdmin     bool
	Calls         int
}

// AddNativeRule records the call and returns the canned result.
func (m *MemNativeRuleWriter) AddNativeRule(_ context.Context, entry, rationale, operator string, admin bool) (NativeRuleWriteOutcome, error) {
	m.Calls++
	m.LastVerb, m.LastEntry, m.LastRationale, m.LastOperator, m.LastAdmin = "add", entry, rationale, operator, admin
	if m.Err != nil {
		return NativeRuleWriteOutcome{}, m.Err
	}
	return m.Outcome, nil
}

// DeleteNativeRule records the call and returns the canned result.
func (m *MemNativeRuleWriter) DeleteNativeRule(_ context.Context, id int64, rationale, operator string, admin bool) (NativeRuleWriteOutcome, error) {
	m.Calls++
	m.LastVerb, m.LastID, m.LastRationale, m.LastOperator, m.LastAdmin = "delete", id, rationale, operator, admin
	if m.Err != nil {
		return NativeRuleWriteOutcome{}, m.Err
	}
	return m.Outcome, nil
}

// compile-time proof the in-memory twins satisfy the interfaces.
var (
	_ NativeRulesReader = (*MemNativeRulesReader)(nil)
	_ NativeRuleWriter  = (*MemNativeRuleWriter)(nil)
)
