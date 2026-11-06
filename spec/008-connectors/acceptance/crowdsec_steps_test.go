package acceptance

import (
	"context"
	"fmt"
	"time"

	"github.com/cucumber/godog"

	adaptingest "github.com/territory-grounder/grounder/adapters/ingest"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/ingest/crowdsec"
)

// The CrowdSec module (REQ-803) binds its own scenario — the third ingest registrar, still no edit to
// the shared acceptance_test.go.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerCrowdsecSteps)
}

type crowdsecWorld struct {
	reg       *modules.Registry
	published int
	err       error
}

func registerCrowdsecSteps(sc *godog.ScenarioContext) {
	w := &crowdsecWorld{}
	now := func() time.Time { return time.Date(2026, 7, 15, 12, 5, 0, 0, time.UTC) }

	sc.Step(`^the CrowdSec ingest module is registered and enabled with a configured LAPI endpoint$`, func() error {
		w.reg = modules.NewRegistry()
		mod := crowdsec.New(crowdsec.WithClock(now))
		return w.reg.Register(modules.Registration{
			Surface:    modules.SurfaceIngest,
			SourceType: crowdsec.SourceType,
			Capability: "ingest.crowdsec",
			Enabled:    true,
			Adapter:    mod,
		})
	})

	sc.Step(`^a scenario decision event arrives$`, func() error {
		reg, err := w.reg.Resolve(modules.SurfaceIngest, crowdsec.SourceType)
		if err != nil {
			return fmt.Errorf("the enabled module must resolve: %w", err)
		}
		ing, ok := reg.Adapter.(adaptingest.Ingester)
		if !ok {
			return fmt.Errorf("the registered adapter must satisfy adapters/ingest.Ingester")
		}
		event := []byte(`{"scenario":"crowdsecurity/ssh-bf","message":"Ip 1.2.3.4 performed ssh-bf","start_at":"2026-07-15T12:00:00Z","source":{"scope":"Ip","value":"1.2.3.4","ip":"1.2.3.4"},"decisions":[{"type":"ban","value":"1.2.3.4","scenario":"crowdsecurity/ssh-bf","origin":"crowdsec"}]}`)
		env, err := ing.Normalize(context.Background(), event)
		if err != nil {
			w.err = err
			return nil
		}
		batch := coreingest.NewPipeline().Process([]coreingest.IncidentEnvelope{env}, now())
		pub := &coreingest.RecordingPublisher{}
		n, err := coreingest.PublishTriage(context.Background(), pub, batch, now())
		if err != nil {
			w.err = err
			return nil
		}
		w.published = n
		return nil
	})

	sc.Step(`^it is normalized to the canonical triage shape and routed through the shared dedup and flap admission path$`, func() error {
		if w.err != nil {
			return fmt.Errorf("normalize + admission must succeed: %w", w.err)
		}
		if w.published != 1 {
			return fmt.Errorf("the decision event must route through admission and publish one triage.requested, got %d", w.published)
		}
		return nil
	})
}
