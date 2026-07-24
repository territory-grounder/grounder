package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/territory-grounder/grounder/core/auth"
)

// The governance read surface (spec/006 REQ-511): the safety posture as the control plane itself
// holds it — the mutation gate's live state, the band distribution the classifier actually recorded
// (the audit spine, grouped), and the hash chain's head. Every field is read from the authoritative
// component; nothing is computed or asserted here (INV-15).

// ChainHead is the governance ledger's current tip.
type ChainHead struct {
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
}

// GovernanceState is the read-only safety posture the console renders. mutation_enabled + effect_capability
// reflect the WORKER's published live posture (the authoritative mutation gate lives in the worker process,
// not the read-only grounder). posture_stale flags that the worker's published row is stale or absent — the
// surface then reports the freshest reading it holds but marks it unknown rather than a confident OFF;
// posture_source names where the value came from (worker / worker-stale / grounder-gate). preflight_green
// stays the local gate's own preflight bit.
type GovernanceState struct {
	MutationEnabled  bool           `json:"mutation_enabled"`
	PreflightGreen   bool           `json:"preflight_green"`
	Bands            map[string]int `json:"bands"` // band -> session count, from the audit spine
	Chain            ChainHead      `json:"chain"`
	EffectCapability string         `json:"effect_capability"`
	PostureStale     bool           `json:"posture_stale"`
	PostureSource    string         `json:"posture_source"`
}

// GovernanceReader assembles the live posture for the authenticated principal.
type GovernanceReader interface {
	Governance(ctx context.Context, p auth.Principal) (GovernanceState, error)
}

// governanceHandler serves GET /v1/governance. Nil reader = 503 fail-closed.
func (d Deps) governanceHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Governance == nil {
		http.Error(w, "governance unavailable", http.StatusServiceUnavailable)
		return
	}
	st, err := d.Governance.Governance(r.Context(), p)
	if err != nil {
		http.Error(w, "governance unavailable", http.StatusServiceUnavailable)
		return
	}
	if st.Bands == nil {
		st.Bands = map[string]int{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}
