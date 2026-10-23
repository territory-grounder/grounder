package acceptance

import (
	"context"
	"fmt"
	"time"

	"github.com/cucumber/godog"

	adaptingest "github.com/territory-grounder/grounder/adapters/ingest"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/ingest/librenms"
)

// The LibreNMS module (REQ-801) binds its own scenario here, appended to the shared harness via init() —
// no edit to acceptance_test.go. Each future connector follows this same self-contained pattern.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerLibrenmsSteps)
}

type librenmsWorld struct {
	reg       *modules.Registry
	published []coreingest.TriageRequested
	err       error
}

func registerLibrenmsSteps(sc *godog.ScenarioContext) {
	w := &librenmsWorld{}
	now := func() time.Time { return time.Date(2026, 7, 15, 12, 5, 0, 0, time.UTC) }

	sc.Step(`^the LibreNMS ingest module is registered and enabled with an NL config row and a GR config row$`, func() error {
		w.reg = modules.NewRegistry()
		mod := librenms.New([]librenms.Deployment{
			{Site: "NL", BaseURL: "https://librenms.nl", TokenRef: "env:LIBRENMS_NL_TOKEN"},
			{Site: "GR", BaseURL: "https://librenms.gr", TokenRef: "env:LIBRENMS_GR_TOKEN"},
		}, librenms.WithClock(now))
		return w.reg.Register(modules.Registration{
			Surface:    modules.SurfaceIngest,
			SourceType: librenms.SourceType,
			Capability: "ingest.librenms",
			Enabled:    true,
			Adapter:    mod,
		})
	})

	sc.Step(`^a device-down event arrives from the NL instance$`, func() error {
		reg, err := w.reg.Resolve(modules.SurfaceIngest, librenms.SourceType)
		if err != nil {
			return fmt.Errorf("the enabled module must resolve to an execution path: %w", err)
		}
		ing, ok := reg.Adapter.(adaptingest.Ingester)
		if !ok {
			return fmt.Errorf("the registered adapter must satisfy adapters/ingest.Ingester")
		}
		payload := []byte(`{"site":"NL","id":"42","rule":"Device Down","severity":"critical","host":"192.0.2.1","hostname":"sw-core-01","title":"Device Down: sw-core-01","timestamp":"2026-07-15 12:00:00","state":1}`)
		env, err := ing.Normalize(context.Background(), payload)
		if err != nil {
			w.err = err
			return nil
		}
		// in-code admission (dedup → flap → burst → correlate) BEFORE any triage.requested is published.
		batch := coreingest.NewPipeline().Process([]coreingest.IncidentEnvelope{env}, now())
		pub := &coreingest.RecordingPublisher{}
		if _, err := coreingest.PublishTriage(context.Background(), pub, batch, now()); err != nil {
			w.err = err
			return nil
		}
		w.published = pub.Events
		return nil
	})

	sc.Step(`^the event is normalized to the canonical triage shape and a triage.requested event is emitted after in-code admission$`, func() error {
		if w.err != nil {
			return fmt.Errorf("normalize + admission must succeed: %w", w.err)
		}
		if len(w.published) != 1 {
			return fmt.Errorf("exactly one triage.requested must be emitted, got %d", len(w.published))
		}
		ev := w.published[0]
		if ev.ExternalRef != "librenms-NL-42" {
			return fmt.Errorf("triage.requested external_ref = %q, want librenms-NL-42", ev.ExternalRef)
		}
		if ev.Envelope.Severity != coreingest.SeverityCritical || ev.Envelope.Host != "sw-core-01" {
			return fmt.Errorf("envelope is not the canonical device-down shape: %+v", ev.Envelope)
		}
		return nil
	})
}
