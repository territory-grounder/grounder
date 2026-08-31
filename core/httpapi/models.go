package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/territory-grounder/grounder/core/auth"
)

// The models read surface (spec/006 REQ-514): what the model gateway itself reports about the fleet —
// a PASSTHROUGH of the LiteLLM control responses (model inventory / spend), fetched server-side with
// the gateway key (which never leaves the control plane) and relayed verbatim. Nothing is summarized
// or invented here: the console renders exactly what the gateway said, or an unavailable state (INV-15).

// ModelsReader fetches the gateway's own report for the authenticated principal. The returned bytes
// are the gateway's verbatim JSON body.
type ModelsReader interface {
	ModelsUsage(ctx context.Context, p auth.Principal) ([]byte, error)
}

// modelsHandler serves GET /v1/models. Nil reader or a gateway failure = 503 fail-closed.
func (d Deps) modelsHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Models == nil {
		http.Error(w, "models unavailable", http.StatusServiceUnavailable)
		return
	}
	body, err := d.Models.ModelsUsage(r.Context(), p)
	if err != nil || len(body) == 0 {
		http.Error(w, "models unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}
