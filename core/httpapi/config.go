package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/cpconfig"
)

// The control-plane configuration read surface (task #27 Phase A, spec/006 REQ-520): the resolved
// configuration with each knob's SOURCE (law / env / console / default). LAW keys are pinned and shown
// read-only — they can never be overridden. It is a pure read: no write path here (Phase B adds the admin
// lane), and it emits NO secret VALUE — secrets keep their value-less /v1/secrets surface (INV-13). A nil
// resolver fails closed to 503, never a fabricated config (INV-15).

// ConfigResolver resolves the layered control-plane configuration. *cpconfig.Resolver satisfies it; a fake
// backs the tests. nil = the surface fails closed to 503.
type ConfigResolver interface {
	Resolve(ctx context.Context) ([]cpconfig.Value, error)
}

// ConfigView is one resolved knob as the console renders it.
type ConfigView struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	Value           string `json:"value"`
	Source          string `json:"source"` // law | env | console | default
	Law             bool   `json:"law"`
	ConsoleWritable bool   `json:"console_writable"`
}

// ConfigPage is the read-only control-plane config view, registry order.
type ConfigPage struct {
	Config []ConfigView `json:"config"`
}

// configHandler serves GET /v1/config. Nil resolver = 503 fail-closed.
func (d Deps) configHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Config == nil {
		http.Error(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	vals, err := d.Config.Resolve(r.Context())
	if err != nil {
		http.Error(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	views := make([]ConfigView, 0, len(vals))
	for _, v := range vals {
		views = append(views, ConfigView{
			Name:            v.Name,
			Description:     v.Description,
			Value:           v.Value,
			Source:          string(v.Source),
			Law:             v.Law,
			ConsoleWritable: v.ConsoleWritable,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ConfigPage{Config: views})
}
