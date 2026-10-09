package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
)

// The secret-reference read surface (spec/006 REQ-512, extended by REQ-524): the REFERENCES the
// control plane is configured with (env:/file:) and whether each currently resolves, plus the sealed
// secrets by NAME (store:<name>, task #27 Phase D) — NEVER a value. The response types have no value
// field at all, so a secret value cannot be serialized here by construction (guardrail:
// secrets-as-references; sealed secrets are write-only).

// SecretRefStatus is one configured reference and its resolution state.
type SecretRefStatus struct {
	Ref      string `json:"ref"`     // the reference itself (safe to display: "env:NAME" / "file:/path")
	Purpose  string `json:"purpose"` // what the control plane uses it for
	Resolved bool   `json:"resolved"`
}

// SealedSecretInfo is one sealed secret's value-LESS listing row (REQ-524): the name, the reference
// the config plane consumes it by, and its metadata. No value field exists on this type.
type SealedSecretInfo struct {
	Name      string    `json:"name"`
	Ref       string    `json:"ref"` // "store:<name>"
	Purpose   string    `json:"purpose,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SecretsReader lists the configured references for the authenticated principal.
type SecretsReader interface {
	SecretRefs(ctx context.Context, p auth.Principal) ([]SecretRefStatus, error)
}

// SealedSecretsReader lists the sealed store's value-less inventory. nil = the section is omitted
// (a deployment with no sealed store simply has none).
type SealedSecretsReader interface {
	SealedSecrets(ctx context.Context) ([]SealedSecretInfo, error)
}

// SecretsPage is the read-only reference list the console renders.
type SecretsPage struct {
	Refs   []SecretRefStatus  `json:"refs"`
	Sealed []SealedSecretInfo `json:"sealed"`
}

// secretsHandler serves GET /v1/secrets. Nil reader = 503 fail-closed.
func (d Deps) secretsHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.SecretsRead == nil {
		http.Error(w, "secrets unavailable", http.StatusServiceUnavailable)
		return
	}
	refs, err := d.SecretsRead.SecretRefs(r.Context(), p)
	if err != nil {
		http.Error(w, "secrets unavailable", http.StatusServiceUnavailable)
		return
	}
	if refs == nil {
		refs = []SecretRefStatus{}
	}
	sealed := []SealedSecretInfo{}
	if d.SealedRead != nil {
		// Best-effort by design: the reference list stays served even when the sealed store errors —
		// an empty sealed section is honest (nothing listable), never fabricated rows.
		if rows, err := d.SealedRead.SealedSecrets(r.Context()); err == nil && rows != nil {
			sealed = rows
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(SecretsPage{Refs: refs, Sealed: sealed})
}
