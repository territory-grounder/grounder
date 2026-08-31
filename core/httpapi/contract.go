package httpapi

import (
	"net/http"
	"strconv"

	"github.com/territory-grounder/grounder/core/auth"
)

// The contract read surface (spec/006 REQ-515): the generated OpenAPI document served verbatim, so an
// operator can browse the COMPLETE authenticated endpoint map of the control plane. The document is
// the repo's generated artifact (go:embed via docs/contracts) and cannot drift from the served routes
// — the gencontracts -check CI gate fails on any divergence — making this a map that provably matches
// the territory (INV-15).

// contractHandler serves GET /v1/contract. An empty document = 503 fail-closed.
func (d Deps) contractHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(d.Contract) == 0 {
		http.Error(w, "contract unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Length", strconv.Itoa(len(d.Contract)))
	_, _ = w.Write(d.Contract)
}
