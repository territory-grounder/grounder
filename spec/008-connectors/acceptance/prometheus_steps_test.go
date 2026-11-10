package acceptance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cucumber/godog"

	observability "github.com/territory-grounder/grounder/adapters/observability"
	"github.com/territory-grounder/grounder/modules"
	prometheus "github.com/territory-grounder/grounder/modules/observability/prometheus"
)

// The Prometheus observability module (REQ-816) binds its own scenario here — another registrar appended
// alongside the rest, driving the REAL module through the registry with no edit to the shared harness.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerPrometheusSteps)
}

type prometheusWorld struct {
	reg      *modules.Registry
	gathered []observability.Sample
	err      error
}

func registerPrometheusSteps(sc *godog.ScenarioContext) {
	w := &prometheusWorld{}
	now := func() time.Time { return time.Date(2026, 7, 15, 12, 5, 0, 0, time.UTC) }

	sc.Step(`^the Prometheus observability module is registered and enabled$`, func() error {
		w.reg = modules.NewRegistry()
		mod := prometheus.New(prometheus.WithClock(now))
		return w.reg.Register(modules.Registration{
			Surface:    modules.SurfaceObservability,
			SourceType: prometheus.SourceType,
			Capability: "observability.prometheus",
			Enabled:    true,
			Adapter:    mod,
		})
	})

	sc.Step(`^control-plane and per-connector metrics are scraped$`, func() error {
		reg, err := w.reg.Resolve(modules.SurfaceObservability, prometheus.SourceType)
		if err != nil {
			return fmt.Errorf("the enabled module must resolve: %w", err)
		}
		exp, ok := reg.Adapter.(observability.Exporter)
		if !ok {
			return fmt.Errorf("the registered adapter must satisfy adapters/observability.Exporter")
		}
		// A control-plane series and a per-connector series, exported with NO explicit Stamped so Export
		// must stamp them itself (INV-15).
		if err := exp.Export(context.Background(), []observability.Sample{
			{Name: "tg_control_plane_up", Value: 1, Labels: map[string]string{"component": "grounder"}},
			{Name: "tg_connector_up", Value: 1, Labels: map[string]string{"connector": "netbox"}},
		}); err != nil {
			w.err = err
			return nil
		}
		// A scrape reads the held series (and their staleness gauges) back via Gather.
		gatherer, ok := reg.Adapter.(*prometheus.Module)
		if !ok {
			return fmt.Errorf("expected the concrete Prometheus module for Gather()")
		}
		w.gathered = gatherer.Gather()
		return nil
	})

	sc.Step(`^each series carries a freshness timestamp and an absent\(\)-guarded staleness metric so a dead writer pages$`, func() error {
		if w.err != nil {
			return fmt.Errorf("the scrape must succeed: %w", w.err)
		}
		var sawStale bool
		for _, s := range w.gathered {
			if strings.HasSuffix(s.Name, "_stale") {
				sawStale = true
				continue
			}
			if s.Stamped.IsZero() {
				return fmt.Errorf("every non-stale series must carry a freshness timestamp, %q had none", s.Name)
			}
		}
		if !sawStale {
			return fmt.Errorf("a <name>_stale staleness series must exist so a dead writer pages")
		}
		return nil
	})
}
