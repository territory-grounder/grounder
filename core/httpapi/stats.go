package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/territory-grounder/grounder/core/auth"
)

// statsHandler serves read-only platform stats. It runs ONLY after core/auth has verified the caller
// (the PrincipalHandler signature guarantees a Principal), so an unauthenticated request is rejected
// by the middleware before this handler — and therefore before any request body — is ever touched.
func (d Deps) statsHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	s, err := d.Stats.Stats(r.Context(), p)
	if err != nil {
		http.Error(w, "stats unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s)
}
