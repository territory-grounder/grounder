package acceptance

import (
	"context"
	"fmt"

	"github.com/cucumber/godog"

	observability "github.com/territory-grounder/grounder/adapters/observability"
	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/observability/grafana"
)

// Grafana observability provisioning (REQ-817): the control-plane dashboards are defined in version
// control and reconciled; a live dashboard whose hash diverges is a hand-edited panel and is rejected as
// drift. This drives the REAL grafana module through the capability-scoped registry.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerGrafanaSteps)
}

type grafanaWorld struct {
	reg         *modules.Registry
	mod         *grafana.Module
	driftClean  bool // DetectDrift on the untouched, version-controlled dashboard
	driftEdited bool // DetectDrift on a hand-edited dashboard
}

func registerGrafanaSteps(sc *godog.ScenarioContext) {
	w := &grafanaWorld{}

	sc.Step(`^the Grafana observability module is registered and enabled$`, func() error {
		w.reg = modules.NewRegistry()
		mod := grafana.New()
		return w.reg.Register(modules.Registration{
			Surface: modules.SurfaceObservability, SourceType: grafana.SourceType,
			Capability: "observability.grafana", Enabled: true, Adapter: mod,
		})
	})

	sc.Step(`^the control-plane dashboards are provisioned from version-controlled definitions$`, func() error {
		reg, err := w.reg.Resolve(modules.SurfaceObservability, grafana.SourceType)
		if err != nil {
			return fmt.Errorf("the enabled module must resolve: %w", err)
		}
		// prove the registered adapter satisfies the observability surface; Export is a no-op for a
		// provisioning backend and must return nil.
		exp, ok := reg.Adapter.(observability.Exporter)
		if !ok {
			return fmt.Errorf("the registered adapter must satisfy adapters/observability.Exporter")
		}
		if err := exp.Export(context.Background(), nil); err != nil {
			return fmt.Errorf("grafana Export is a provisioning no-op and must return nil: %w", err)
		}
		mod, ok := reg.Adapter.(*grafana.Module)
		if !ok {
			return fmt.Errorf("expected the concrete Grafana module for provisioning")
		}
		w.mod = mod
		// provision the control-plane dashboard from its version-controlled definition.
		mod.Provision([]grafana.Dashboard{{UID: "cp", Hash: "v1"}})
		return nil
	})

	sc.Step(`^hand-edited panels are rejected as drift$`, func() error {
		if w.mod == nil {
			return fmt.Errorf("dashboards must be provisioned before drift detection")
		}
		w.driftClean = w.mod.DetectDrift(grafana.Dashboard{UID: "cp", Hash: "v1"})
		w.driftEdited = w.mod.DetectDrift(grafana.Dashboard{UID: "cp", Hash: "EDITED"})
		if w.driftClean {
			return fmt.Errorf("an untouched, version-controlled dashboard must NOT be flagged as drift")
		}
		if !w.driftEdited {
			return fmt.Errorf("a hand-edited panel (divergent hash) must be rejected as drift")
		}
		return nil
	})
}
