package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
)

// The credential engine's "Sync now" (TG-109, spec/016 REQ-1615's on-demand half). POST-only, admin-session
// (it drives a real read-only pull against a third-party system — same tier and guard as the module TEST
// button), same-origin, rate-limited. The sync itself runs in the WORKER via temporal/credentialsync — this
// process holds no engine handle — and the outcome carries only the non-secret SyncRun facts (INV-13).

// CredentialSyncOutcome is what the operator is shown. No field can hold a secret.
type CredentialSyncOutcome struct {
	SourceID string `json:"source_id"`
	OK       bool   `json:"ok"`
	Summary  string `json:"summary"`
	Detail   string `json:"detail,omitempty"`
	Added    int    `json:"added"`
	Changed  int    `json:"changed"`
	Removed  int    `json:"removed"`
	// Entries is the ABSOLUTE count the source now holds — the anti-quiet-zero fact.
	Entries   int   `json:"entries"`
	Starved   bool  `json:"starved"`
	ElapsedMS int64 `json:"elapsed_ms"`
}

// CredentialSyncer starts the worker-side sync lane for ONE registered source. nil ⇒ the route 503s.
type CredentialSyncer interface {
	SyncSource(ctx context.Context, sourceID, operator string) (CredentialSyncOutcome, error)
}

// credentialSyncHandler is POST /v1/credentials/sources/{source_id}/sync.
func (d Deps) credentialSyncHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !adminWriteGuard(w, r, d.CredentialSync != nil, "credential sync lane not deployed", p) {
		return
	}
	sourceID := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "source_id")))
	if !moduleIdentOK(sourceID) {
		// Bounded + charset-checked like the module identity params: the id keys an engine lookup and a
		// log line, and neither should accept arbitrary bytes.
		http.Error(w, "source_id required", http.StatusBadRequest)
		return
	}
	out, err := d.CredentialSync.SyncSource(r.Context(), sourceID, operatorOf(p))
	if err != nil {
		// The lane could not be RUN (worker unreachable / workflow refused) — distinct from a sync that
		// ran and failed, which arrives as a non-OK outcome. 502 mirrors the module TEST button's honesty:
		// "could not run" is not a verdict on the source.
		http.Error(w, "the sync could not be run", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// MemCredentialSyncer is the in-memory CredentialSyncer twin for the CI oracles (no Temporal). It records
// the last request and returns a canned outcome.
type MemCredentialSyncer struct {
	Outcome      CredentialSyncOutcome
	Err          error
	LastSourceID string
	LastOperator string
}

// SyncSource implements CredentialSyncer.
func (m *MemCredentialSyncer) SyncSource(_ context.Context, sourceID, operator string) (CredentialSyncOutcome, error) {
	m.LastSourceID, m.LastOperator = sourceID, operator
	if m.Err != nil {
		return CredentialSyncOutcome{}, m.Err
	}
	out := m.Outcome
	if out.SourceID == "" {
		out.SourceID = sourceID
	}
	return out, nil
}
