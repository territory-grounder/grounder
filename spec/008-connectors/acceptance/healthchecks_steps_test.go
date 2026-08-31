package acceptance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/cucumber/godog"

	observability "github.com/territory-grounder/grounder/adapters/observability"
	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/observability/healthchecks"
)

// Healthchecks.io dead-man (REQ-820): each heartbeat pings an EXTERNAL check so a missed heartbeat raises an
// alert independent of TG's internal alert path.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerHealthchecksSteps)
}

// healthchecksFakeDoer records every request so the oracle can assert the dead-man check URL was hit.
type healthchecksFakeDoer struct {
	mu   sync.Mutex
	hits []string
}

func (d *healthchecksFakeDoer) Do(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.hits = append(d.hits, req.URL.String())
	d.mu.Unlock()
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("OK")), Header: make(http.Header)}, nil
}

type healthchecksWorld struct {
	reg  *modules.Registry
	doer *healthchecksFakeDoer
	err  error
}

func registerHealthchecksSteps(sc *godog.ScenarioContext) {
	w := &healthchecksWorld{}

	sc.Step(`^the Healthchecks.io observability module is registered and enabled with a configured dead-man check$`, func() error {
		_ = os.Setenv("TG_HEALTHCHECKS_ACCEPT_CHECK", "c0ffee-dead-man")
		w.doer = &healthchecksFakeDoer{}
		w.reg = modules.NewRegistry()
		mod := healthchecks.New("https://hc-ping.test", "env:TG_HEALTHCHECKS_ACCEPT_CHECK", healthchecks.WithHTTPClient(w.doer))
		return w.reg.Register(modules.Registration{
			Surface: modules.SurfaceObservability, SourceType: healthchecks.SourceType, Capability: "observability.healthchecks", Enabled: true, Adapter: mod,
		})
	})

	sc.Step(`^a scheduled control-plane heartbeat fires$`, func() error {
		reg, err := w.reg.Resolve(modules.SurfaceObservability, healthchecks.SourceType)
		if err != nil {
			return fmt.Errorf("the enabled module must resolve: %w", err)
		}
		if _, ok := reg.Adapter.(observability.Exporter); !ok {
			return fmt.Errorf("the registered adapter must satisfy adapters/observability.Exporter")
		}
		mod, ok := reg.Adapter.(*healthchecks.Module)
		if !ok {
			return fmt.Errorf("expected the concrete Healthchecks module to ping the dead-man check")
		}
		// the scheduled heartbeat drives the real Ping on the real module through the registry.
		w.err = mod.Ping(context.Background())
		return nil
	})

	sc.Step(`^the dead-man check is pinged and a missed heartbeat raises an external alert independent of the internal alert path$`, func() error {
		if w.err != nil {
			return fmt.Errorf("the heartbeat ping must succeed: %w", w.err)
		}
		w.doer.mu.Lock()
		defer w.doer.mu.Unlock()
		if len(w.doer.hits) == 0 {
			return fmt.Errorf("the dead-man check URL must be hit on heartbeat, got 0 requests")
		}
		hit := w.doer.hits[len(w.doer.hits)-1]
		if !strings.Contains(hit, "/c0ffee-dead-man") {
			return fmt.Errorf("the configured dead-man check must be pinged, got %q", hit)
		}
		// The ping is an OUT-OF-BAND signal: because Healthchecks.io watches the check on its own
		// infrastructure, a subsequently missed heartbeat trips its alert independent of TG's internal path.
		return nil
	})
}
