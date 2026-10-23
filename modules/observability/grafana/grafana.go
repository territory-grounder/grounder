// Package grafana is the loadable Grafana observability module (spec/008 REQ-817).
//
// Grafana is a PROVISIONING surface, not a sample sink: the control-plane dashboards are defined in
// version control and Grafana is reconciled toward those definitions. The version-controlled definition
// is the fact; a live dashboard whose hash diverges is a hand-edited panel and is rejected as drift. The
// module still satisfies adapters/observability.Exporter so it registers under the observability surface,
// but Export is a no-op (there are no samples to ship — freshness stamping, INV-15, applies to
// sample-exporting backends, and provisioning carries none).
//
// Provenance: [O] REQ-817, spec/008.
package grafana

import (
	"context"
	"sync"

	observability "github.com/territory-grounder/grounder/adapters/observability"
)

// SourceType is the vendor slug this module serves.
const SourceType = "grafana"

// Dashboard is a control-plane dashboard identified by its stable UID and a content Hash. The Hash of the
// version-controlled definition is the fact; a live dashboard whose Hash differs has been hand-edited.
type Dashboard struct {
	UID  string
	Hash string
}

// Module is the Grafana provisioning adapter. Construct with New. It holds the provisioned (version-
// controlled) hash per dashboard UID as the drift baseline.
type Module struct {
	mu    sync.RWMutex
	board map[string]string // UID -> provisioned (version-controlled) Hash
}

// New builds an empty Grafana module. Provision it with the version-controlled dashboard definitions
// before drift detection.
func New() *Module {
	return &Module{board: map[string]string{}}
}

// SourceType implements adapters/observability.Exporter.
func (m *Module) SourceType() string { return SourceType }

// compile-time proof the module satisfies the stable observability interface.
var _ observability.Exporter = (*Module)(nil)

// Export is a no-op: Grafana provisions dashboards, it does not sink freshness-stamped samples. It exists
// so the module satisfies the observability Exporter surface and registers alongside sample backends.
func (m *Module) Export(_ context.Context, _ []observability.Sample) error { return nil }

// Provision records the version-controlled dashboard definitions as the drift baseline — this is the
// source of truth every live dashboard is reconciled against. Re-provisioning a UID overwrites its
// baseline with the newly committed hash.
func (m *Module) Provision(defs []Dashboard) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range defs {
		m.board[d.UID] = d.Hash
	}
}

// DetectDrift reports whether a live dashboard has drifted from its provisioned definition: true when the
// live Hash differs from the version-controlled Hash for the same UID (a hand-edited panel), false
// otherwise (including when the UID was never provisioned, so there is no committed definition to diverge
// from).
func (m *Module) DetectDrift(live Dashboard) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	want, ok := m.board[live.UID]
	if !ok {
		return false
	}
	return live.Hash != want
}
