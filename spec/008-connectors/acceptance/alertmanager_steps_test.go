package acceptance

import (
	"context"
	"fmt"
	"time"

	"github.com/cucumber/godog"

	adaptingest "github.com/territory-grounder/grounder/adapters/ingest"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/modules"
	alertmanager "github.com/territory-grounder/grounder/modules/ingest/prometheus-alertmanager"
)

// The Alertmanager module (REQ-802) binds its own scenario here — a second registrar appended alongside
// LibreNMS's, proving the decoupled harness scales with no edit to the shared acceptance_test.go.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerAlertmanagerSteps)
}

type alertmanagerWorld struct {
	reg       *modules.Registry
	published int
	err       error
}

func registerAlertmanagerSteps(sc *godog.ScenarioContext) {
	w := &alertmanagerWorld{}
	now := func() time.Time { return time.Date(2026, 7, 15, 12, 5, 0, 0, time.UTC) }

	sc.Step(`^the Prometheus and Alertmanager ingest module is registered and enabled$`, func() error {
		w.reg = modules.NewRegistry()
		mod := alertmanager.New(alertmanager.WithClock(now))
		return w.reg.Register(modules.Registration{
			Surface:    modules.SurfaceIngest,
			SourceType: alertmanager.SourceType,
			Capability: "ingest.prometheus-alertmanager",
			Enabled:    true,
			Adapter:    mod,
		})
	})

	sc.Step(`^a firing alert is followed by its resolved alert for the same alertname and target$`, func() error {
		reg, err := w.reg.Resolve(modules.SurfaceIngest, alertmanager.SourceType)
		if err != nil {
			return fmt.Errorf("the enabled module must resolve: %w", err)
		}
		ing, ok := reg.Adapter.(adaptingest.Ingester)
		if !ok {
			return fmt.Errorf("the registered adapter must satisfy adapters/ingest.Ingester")
		}
		firing := []byte(`{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"MeshBFDSessionDown","instance":"nl-frr01:9100","severity":"warning"},"annotations":{"summary":"BFD session down"},"startsAt":"2026-07-15T12:00:00Z"}]}`)
		resolved := []byte(`{"status":"resolved","alerts":[{"status":"resolved","labels":{"alertname":"MeshBFDSessionDown","instance":"nl-frr01:9100","severity":"warning"},"annotations":{"summary":"BFD session recovered"},"startsAt":"2026-07-15T12:00:00Z"}]}`)
		f, err := ing.Normalize(context.Background(), firing)
		if err != nil {
			w.err = err
			return nil
		}
		r, err := ing.Normalize(context.Background(), resolved)
		if err != nil {
			w.err = err
			return nil
		}
		batch := coreingest.NewPipeline().Process([]coreingest.IncidentEnvelope{f, r}, now())
		pub := &coreingest.RecordingPublisher{}
		n, err := coreingest.PublishTriage(context.Background(), pub, batch, now())
		if err != nil {
			w.err = err
			return nil
		}
		w.published = n
		return nil
	})

	sc.Step(`^the transition is correlated to one incident and validated against the explicit grammar$`, func() error {
		if w.err != nil {
			return fmt.Errorf("both alerts must validate against the grammar: %w", w.err)
		}
		if w.published != 1 {
			return fmt.Errorf("a firing→resolved transition must correlate to one incident, published %d", w.published)
		}
		return nil
	})
}
