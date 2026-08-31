// Package resolve turns the registry's generic Resolve (which returns an untyped `Adapter any`) into typed,
// fail-closed accessors — one per surface. It is the primitive every registry-backed consumer uses: instead
// of each call site doing its own `reg.Resolve(...)` plus an unchecked `.(ingest.Ingester)` assertion (a
// panic waiting to happen), a caller gets the concrete surface adapter or a descriptive error.
//
// Both failure modes fail closed: an unregistered or disabled source propagates the registry's
// ErrNoExecutionPath (INV-17 — no capability, no path), and a registration whose adapter does not implement
// its surface's interface (a wiring bug) returns a typed error rather than panicking on the assertion.
//
// Provenance: [O] INV-17 (no execution path for an unregistered/disabled module), spec/008 (registry-backed
// surface resolution — the typed seam hot paths resolve their connectors through).
package resolve

import (
	"fmt"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	cmdb "github.com/territory-grounder/grounder/adapters/cmdb"
	ingest "github.com/territory-grounder/grounder/adapters/ingest"
	model "github.com/territory-grounder/grounder/adapters/model"
	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	observability "github.com/territory-grounder/grounder/adapters/observability"
	tracker "github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/modules"
)

// typed resolves a (surface, sourceType) to its concrete adapter T. It fails closed on both a no-execution-
// path (unregistered/disabled) and an adapter that does not implement T (a registration wiring bug).
func typed[T any](r *modules.Registry, surface, sourceType string) (T, error) {
	var zero T
	reg, err := r.Resolve(surface, sourceType)
	if err != nil {
		return zero, err
	}
	a, ok := reg.Adapter.(T)
	if !ok {
		return zero, fmt.Errorf("modules/resolve: %s/%s adapter %T does not implement the %s surface", surface, sourceType, reg.Adapter, surface)
	}
	return a, nil
}

// Ingester resolves the enabled ingest source for sourceType.
func Ingester(r *modules.Registry, sourceType string) (ingest.Ingester, error) {
	return typed[ingest.Ingester](r, modules.SurfaceIngest, sourceType)
}

// Tracker resolves the enabled issue tracker for sourceType.
func Tracker(r *modules.Registry, sourceType string) (tracker.Tracker, error) {
	return typed[tracker.Tracker](r, modules.SurfaceTracker, sourceType)
}

// Notifier resolves the enabled notifier for sourceType.
func Notifier(r *modules.Registry, sourceType string) (notifier.Notifier, error) {
	return typed[notifier.Notifier](r, modules.SurfaceNotifier, sourceType)
}

// CMDB resolves the enabled CMDB reader for sourceType.
func CMDB(r *modules.Registry, sourceType string) (cmdb.CMDB, error) {
	return typed[cmdb.CMDB](r, modules.SurfaceCMDB, sourceType)
}

// Actuator resolves the enabled actuator for sourceType. In Phase 0/1 the actuation family is registered
// DISABLED, so this returns ErrNoExecutionPath for every actuator — the surface is inert by construction.
func Actuator(r *modules.Registry, sourceType string) (actuation.Actuator, error) {
	return typed[actuation.Actuator](r, modules.SurfaceActuation, sourceType)
}

// Provider resolves the enabled model-provider descriptor for sourceType.
func Provider(r *modules.Registry, sourceType string) (model.Provider, error) {
	return typed[model.Provider](r, modules.SurfaceModel, sourceType)
}

// Exporter resolves the enabled observability exporter for sourceType.
func Exporter(r *modules.Registry, sourceType string) (observability.Exporter, error) {
	return typed[observability.Exporter](r, modules.SurfaceObservability, sourceType)
}
