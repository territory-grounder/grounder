// Package modules is the capability-scoped registry for Territory Grounder's loadable connector fleet.
//
// Provenance: [F] the module system (docs/PRODUCT.md §6) · [O] INV-17 (a disabled or unregistered module
// has NO execution path), INV-18 (exactly one registered implementation per surface+source type), spec/008.
//
// The load-bearing property is INV-17: a capability exists ONLY if its module is registered AND enabled.
// This is the product-grade closure of the predecessor's "dead path still executable" failure class — a
// module that is not registered, or is registered but disabled, cannot be resolved to a usable adapter, so
// there is no path by which it can act. INV-18 makes per-site variance configuration behind ONE
// implementation (the two LibreNMS deployments are two config rows behind one module, not two modules).
package modules

import (
	"errors"
	"sort"
	"sync"
)

// The seven module surfaces. A registration's Surface must be one of these.
const (
	SurfaceIngest        = "ingest"
	SurfaceTracker       = "tracker"
	SurfaceNotifier      = "notifier"
	SurfaceCMDB          = "cmdb"
	SurfaceActuation     = "actuation"
	SurfaceModel         = "model"
	SurfaceObservability = "observability"
)

// Registration is a module's registry entry: its surface + source type + capability scope + enablement,
// plus the concrete surface adapter. A capability exists only if its module is registered AND enabled; the
// concrete adapter is retrieved by a surface-typed assertion at the call site.
type Registration struct {
	Surface    string // one of the Surface* constants
	SourceType string // the source/vendor slug within the surface (e.g. "librenms", "youtrack")
	Capability string // the declared capability scope slug (e.g. "ingest.librenms")
	Enabled    bool   // a disabled module ships built but has NO execution path until enabled (INV-17)
	Adapter    any    // the concrete surface adapter (ingest.Ingester, tracker.Tracker, …)
}

var (
	// ErrNoExecutionPath is the load-bearing INV-17 property: an unregistered OR disabled module has no
	// execution path. Resolve returns it instead of a usable adapter.
	ErrNoExecutionPath = errors.New("modules: no execution path — module unregistered or disabled (INV-17)")
	// ErrDuplicateSource enforces INV-18: exactly one registered implementation per (surface, source type).
	ErrDuplicateSource = errors.New("modules: a module is already registered for this surface and source type (INV-18)")
	// ErrUnidentified rejects a registration missing its surface, source type, or adapter.
	ErrUnidentified = errors.New("modules: registration missing surface, source type, or adapter")
)

// Registry is the capability-scoped module registry. INV-17: a capability exists only if its module is
// registered and enabled — an unregistered or disabled module has no execution path. INV-18: each
// (surface, source type) admits exactly one registered implementation; per-site variance is configuration
// behind that one implementation, never a second registration. The zero Registry is not usable; call
// NewRegistry. [O] spec/008.
type Registry struct {
	mu  sync.RWMutex
	reg map[string]Registration
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{reg: map[string]Registration{}} }

func regKey(surface, sourceType string) string { return surface + "/" + sourceType }

// Register adds a module. A registration missing its surface, source type, or adapter is rejected
// (ErrUnidentified); a second registration for the same (surface, source type) is rejected
// (ErrDuplicateSource, INV-18). A module MAY be registered disabled — reference modules ship
// built-but-disabled — because enablement gates the execution path, not existence.
func (r *Registry) Register(reg Registration) error {
	if reg.Surface == "" || reg.SourceType == "" || reg.Adapter == nil {
		return ErrUnidentified
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := regKey(reg.Surface, reg.SourceType)
	if _, exists := r.reg[k]; exists {
		return ErrDuplicateSource
	}
	r.reg[k] = reg
	return nil
}

// SetEnabled flips the enablement of an already-registered module — the configuration act that gives a
// built-but-disabled module (a reference connector) its execution path, or withdraws it. Enablement never
// creates a second registration (INV-18); it toggles the one that already exists. A module that was never
// registered cannot be enabled (ErrNoExecutionPath).
func (r *Registry) SetEnabled(surface, sourceType string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := regKey(surface, sourceType)
	reg, ok := r.reg[k]
	if !ok {
		return ErrNoExecutionPath
	}
	reg.Enabled = enabled
	r.reg[k] = reg
	return nil
}

// Resolve returns the registered, ENABLED module for a (surface, source type). An unregistered or disabled
// module yields ErrNoExecutionPath — no registration (or no enablement) ⇒ no execution path (INV-17).
func (r *Registry) Resolve(surface, sourceType string) (Registration, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reg, ok := r.reg[regKey(surface, sourceType)]
	if !ok || !reg.Enabled || reg.Adapter == nil {
		return Registration{}, ErrNoExecutionPath
	}
	return reg, nil
}

// Manifest returns the sorted "surface/source" keys of every registered module (enabled or not) — the
// input a boot-time reconciler compares against the signed manifest to refuse a divergent live set.
func (r *Registry) Manifest() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := make([]string, 0, len(r.reg))
	for k := range r.reg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Capability is a status-aware view of one registered module for operator visibility: its surface, source
// type, capability slug, and whether it currently has an execution path (enabled). It carries no adapter —
// only the declaration facts, safe to expose over the read-only API.
type Capability struct {
	Surface    string `json:"surface"`
	SourceType string `json:"source_type"`
	Capability string `json:"capability"`
	Enabled    bool   `json:"enabled"`
}

// Capabilities returns the declared fleet with enablement status, sorted by surface then source. Unlike
// Manifest (bare keys, for boot reconciliation), this is the operator-facing view — it makes the DISABLED
// members (e.g. the Phase-0/1 actuation family, which has no execution path, INV-17) visibly distinct from
// the live ones, so the console/ops can never mistake a declared-but-inert capability for an available one.
func (r *Registry) Capabilities() []Capability {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Capability, 0, len(r.reg))
	for _, reg := range r.reg {
		out = append(out, Capability{Surface: reg.Surface, SourceType: reg.SourceType, Capability: reg.Capability, Enabled: reg.Enabled})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Surface != out[j].Surface {
			return out[i].Surface < out[j].Surface
		}
		return out[i].SourceType < out[j].SourceType
	})
	return out
}
