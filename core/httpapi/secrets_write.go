package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
)

// The sealed-secret write surface (task #27 Phase D, spec/006 REQ-524): POST /v1/secrets/{name}
// {value, purpose, rationale}, admin-session-only. WRITE-ONLY by construction: the value is sealed
// (envelope-encrypted, core/seal) in the backend BEFORE anything durable or workflow-bound sees it,
// the response type has no value field, and the read surface (/v1/secrets) lists names + metadata
// only. The secret becomes consumable ONLY as the reference "store:<name>" inside the control plane.

// secretNameRe bounds a sealed secret's name: a stable, log-safe identifier (mirrors the DDL CHECK).
var secretNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// SecretPutRequest carries the material IN (this is the only direction it ever travels).
type SecretPutRequest struct {
	Value     string `json:"value"`
	Purpose   string `json:"purpose"`
	Rationale string `json:"rationale"`
}

// SecretPutOutcome is the value-LESS commit receipt: the reference and the ledger seq, never the value.
type SecretPutOutcome struct {
	Name      string `json:"name"`
	Ref       string `json:"ref"` // "store:<name>"
	LedgerSeq int64  `json:"ledger_seq"`
}

// SealedSecretWriter seals the value and executes the ledgered store via the worker. nil = 503.
type SealedSecretWriter interface {
	PutSecret(ctx context.Context, name, value, purpose, rationale, operator string) (SecretPutOutcome, error)
}

// secretPutHandler serves POST /v1/secrets/{name} (AuthAdminSession).
func (d Deps) secretPutHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !adminWriteGuard(w, r, d.SecretsWrite != nil, "sealed secret store unavailable", p) {
		return
	}
	name := chi.URLParam(r, "name")
	if !secretNameRe.MatchString(name) {
		http.Error(w, "secret name must match ^[a-z0-9][a-z0-9._-]{0,62}$", http.StatusBadRequest)
		return
	}
	var req SecretPutRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil {
		http.Error(w, "malformed secret write", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Rationale) == "" {
		http.Error(w, "rationale required — every secret write states why", http.StatusBadRequest)
		return
	}
	if req.Value == "" || len(req.Value) > 8192 {
		http.Error(w, "secret value required (1..8192 bytes)", http.StatusBadRequest)
		return
	}
	out, err := d.SecretsWrite.PutSecret(r.Context(), name, req.Value, strings.TrimSpace(req.Purpose),
		strings.TrimSpace(req.Rationale), operatorOf(p))
	// The material is not referenced again past this line — write-only.
	if err != nil {
		http.Error(w, "sealed secret write failed — retry", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(out)
}
