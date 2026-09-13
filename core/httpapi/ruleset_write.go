package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/policy"
)

// The active-ruleset write surface (spec/015 REQ-1503, TG-104): POST /v1/policy/ruleset
// {document, expected_version?, rationale?}, admin-session-only (AuthAdminSession — a step-up-elevated
// operator; machine principals and plain sessions have NO route here). This is the "sealed, ledgered admin
// write" the Policy console's disabled "Edit rules…" placeholder names. The write executes in the WORKER
// (the governance ledger's single writer) through the distinctly-named rulesetwrite.RulesetWriteWorkflow:
// the document is VALIDATED via core/policy.ParseRuleSet (a malformed ruleset is refused, never persisted —
// a bad ruleset governs actuation), ledgered BEFORE the row commits, then persisted (active singleton +
// immutable version archive) in one transaction. The grounder NEVER writes the ruleset itself.
//
// The operator identity + admin-tier proof are DERIVED from the authenticated principal (never the body):
// reaching this handler already proves the caller is an admin-session operator. The read projection lives on
// GET /v1/policy/rules (AuthReadOnly); this is the write counterpart on a DISTINCT path — this Router
// registers by pattern, so reusing /v1/policy/rules would silently unroute the read (see router.go).

// ErrRulesetVersionConflict is the typed optimistic-concurrency refusal the worker backend maps a stale
// compare-and-swap onto, so the handler returns an honest 409 (mirrors the policy.ErrStaleMode → 409
// mapping on the mode surface). It lives here (not in a temporal package) so the handler maps it without
// depending on the worker packages, exactly as the mode handler maps policy errors.
var ErrRulesetVersionConflict = errors.New("ruleset version conflict — expected_version no longer matches the active ruleset")

// maxRulesetBytes bounds the submitted document. A rules-as-data policy is small (op-classes, host globs,
// verdicts, params) but can carry many rules; 1 MiB is generous headroom while still a hard admin-write cap.
const maxRulesetBytes = 1 << 20

// RulesetWriteRequest is the operator's replacement order. document is the new rules-as-data policy, accepted
// either as a JSON OBJECT (used verbatim) or as a JSON-encoded STRING (unquoted to its inner JSON), so a
// console or a scripted caller can submit either shape. expected_version is an OPTIONAL compare-and-swap
// guard (the bundle_version the operator last read); rationale is OPTIONAL and folded into the audit reason.
type RulesetWriteRequest struct {
	Document        json.RawMessage `json:"document"`
	ExpectedVersion string          `json:"expected_version,omitempty"`
	Rationale       string          `json:"rationale,omitempty"`
}

// RulesetWriteOutcome is the committed replacement for the console: the bundle_version now active (the same
// value a subsequent write passes as expected_version), the rule count, who wrote it, and the governance
// ledger sequence the decision landed at.
type RulesetWriteOutcome struct {
	Version   string `json:"version"`
	RuleCount int    `json:"rule_count"`
	UpdatedBy string `json:"updated_by"`
	LedgerSeq int64  `json:"ledger_seq"`
}

// RulesetWriter executes the validated, ledgered ruleset replacement via the worker. nil = the surface fails
// closed to 503. adminAuthorized reflects the AuthAdminSession principal, carried to the worker (which
// re-checks it — the surface is never the only line).
type RulesetWriter interface {
	WriteRuleset(ctx context.Context, document []byte, expectedVersion, rationale, operator string, adminAuthorized bool) (RulesetWriteOutcome, error)
}

// rulesetWriteErr maps a worker refusal to an honest status; anything else is retryable. A malformed
// ruleset that somehow reached the worker (the surface already validates) is a 400; a stale compare-and-swap
// is a 409.
func rulesetWriteErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, policy.ErrMalformedRule):
		http.Error(w, "refused: malformed ruleset — "+err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrRulesetVersionConflict):
		http.Error(w, "refused: expected_version no longer matches the active ruleset — re-read and retry", http.StatusConflict)
	default:
		http.Error(w, "ruleset write failed — retry", http.StatusServiceUnavailable)
	}
}

// rulesetWriteHandler serves POST /v1/policy/ruleset (AuthAdminSession).
func (d Deps) rulesetWriteHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !adminWriteGuard(w, r, d.RulesetWrite != nil, "ruleset write path unavailable", p) {
		return
	}
	var req RulesetWriteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRulesetBytes)).Decode(&req); err != nil {
		http.Error(w, "malformed ruleset write", http.StatusBadRequest)
		return
	}
	document, err := normalizeRulesetDocument(req.Document)
	if err != nil {
		http.Error(w, "ruleset document required (a JSON object or a JSON-encoded string)", http.StatusBadRequest)
		return
	}
	// Fast surface validation (the worker re-validates as the authority). A malformed ruleset is REJECTED
	// here — 400, no worker call, no persist — because a bad ruleset governs actuation. This is the
	// load-bearing safety check on the surface; the worker's ParseRuleSet is the same check as the authority.
	if _, err := policy.ParseRuleSet(document); err != nil {
		http.Error(w, "refused: malformed ruleset — "+err.Error(), http.StatusBadRequest)
		return
	}
	// operator + admin proof come from the AUTHENTICATED principal, never the body (INV-01). Reaching an
	// AuthAdminSession route means p.Admin is true.
	out, err := d.RulesetWrite.WriteRuleset(r.Context(), document,
		strings.TrimSpace(req.ExpectedVersion), strings.TrimSpace(req.Rationale), operatorOf(p), p.Admin)
	if err != nil {
		rulesetWriteErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// normalizeRulesetDocument accepts the ruleset as a JSON OBJECT (used verbatim) or as a JSON-encoded STRING
// (unquoted to its inner JSON), returning the raw rules-as-data JSON bytes ParseRuleSet consumes. An empty
// or JSON-null document is refused.
func normalizeRulesetDocument(raw json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, errors.New("empty ruleset document")
	}
	if trimmed[0] == '"' { // a JSON-encoded string carrying the ruleset JSON — unquote to the inner bytes.
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, err
		}
		if strings.TrimSpace(s) == "" {
			return nil, errors.New("empty ruleset document")
		}
		return []byte(s), nil
	}
	return trimmed, nil
}

// MemRulesetWriter is the in-memory RulesetWriter twin for the CI oracles (no Temporal/worker). It returns a
// canned outcome/error and records the last call so a test can assert the handler derived the operator +
// admin proof from the principal (not the body) and forwarded the validated fields.
type MemRulesetWriter struct {
	Outcome             RulesetWriteOutcome
	Err                 error
	LastDocument        []byte
	LastExpectedVersion string
	LastRationale       string
	LastOperator        string
	LastAdmin           bool
	Calls               int
}

// WriteRuleset records the call and returns the canned result.
func (m *MemRulesetWriter) WriteRuleset(_ context.Context, document []byte, expectedVersion, rationale, operator string, adminAuthorized bool) (RulesetWriteOutcome, error) {
	m.Calls++
	m.LastDocument = append([]byte(nil), document...)
	m.LastExpectedVersion, m.LastRationale, m.LastOperator, m.LastAdmin = expectedVersion, rationale, operator, adminAuthorized
	if m.Err != nil {
		return RulesetWriteOutcome{}, m.Err
	}
	return m.Outcome, nil
}

// compile-time proof the in-memory twin satisfies the interface.
var _ RulesetWriter = (*MemRulesetWriter)(nil)
