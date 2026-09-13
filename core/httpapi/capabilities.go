package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/modules"
)

// CapabilitiesReader returns the declared connector fleet with enablement status. It is the read side of the
// module registry's manifest — a pure declaration view (no adapters), safe over the read-only API. The
// composition root backs it with the live registry.
type CapabilitiesReader interface {
	Capabilities() []modules.Capability
}

// CapabilitiesPage is the read-only fleet view the console/ops render: which connectors are declared and
// which currently have an execution path (INV-17). A declared-but-disabled member (the Phase-0/1 actuation
// family) is visibly `enabled: false`, so an operator can never mistake it for an available capability.
type CapabilitiesPage struct {
	Capabilities []modules.Capability `json:"capabilities"`
}

// capabilitiesHandler serves the declared fleet. Runs only after core/auth authenticates the caller. A nil
// reader (no registry wired) fails closed to 503 rather than nil-dereferencing.
func (d Deps) capabilitiesHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if d.Capabilities == nil {
		http.Error(w, "capabilities unavailable", http.StatusServiceUnavailable)
		return
	}
	caps := d.Capabilities.Capabilities()
	if caps == nil {
		caps = []modules.Capability{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(CapabilitiesPage{Capabilities: caps})
}
