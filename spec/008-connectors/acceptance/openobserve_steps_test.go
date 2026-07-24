package acceptance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cucumber/godog"

	observability "github.com/territory-grounder/grounder/adapters/observability"
	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/observability/openobserve"
)

// OpenObserve observability (REQ-818): OTLP export with tracing default-on, so a completed session's
// metrics/logs and its span trajectory both reach the endpoint and the trajectory is reconstructable.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerOpenobserveSteps)
}

// openobservePost is one POST the fake endpoint recorded.
type openobservePost struct {
	method string
	path   string
	body   string
}

type openobserveFakeDoer struct{ posts []openobservePost }

func (f *openobserveFakeDoer) Do(req *http.Request) (*http.Response, error) {
	var body string
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}
	f.posts = append(f.posts, openobservePost{method: req.Method, path: req.URL.Path, body: body})
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
}

type openobserveWorld struct {
	reg  *modules.Registry
	fake *openobserveFakeDoer
	mod  *openobserve.Module
	err  error
}

func registerOpenobserveSteps(sc *godog.ScenarioContext) {
	w := &openobserveWorld{}

	sc.Step(`^the OpenObserve observability module is registered and enabled with tracing default-on$`, func() error {
		_ = os.Setenv("TG_OPENOBSERVE_ACCEPT_TOKEN", "tok")
		w.reg = modules.NewRegistry()
		w.fake = &openobserveFakeDoer{}
		w.mod = openobserve.New("https://openobserve.test", "env:TG_OPENOBSERVE_ACCEPT_TOKEN", openobserve.WithHTTPClient(w.fake))
		if !w.mod.Tracing() {
			return fmt.Errorf("tracing must be default-on")
		}
		return w.reg.Register(modules.Registration{
			Surface: modules.SurfaceObservability, SourceType: openobserve.SourceType, Capability: "observability.openobserve", Enabled: true, Adapter: w.mod,
		})
	})

	sc.Step(`^a session runs to completion$`, func() error {
		reg, err := w.reg.Resolve(modules.SurfaceObservability, openobserve.SourceType)
		if err != nil {
			return fmt.Errorf("the enabled module must resolve: %w", err)
		}
		exp, ok := reg.Adapter.(observability.Exporter)
		if !ok {
			return fmt.Errorf("the registered adapter must satisfy adapters/observability.Exporter")
		}
		// the session's metrics/logs ship as OTLP-ish samples, each carrying its freshness stamp (INV-15).
		samples := []observability.Sample{
			{Name: "session_duration_seconds", Value: 12, Stamped: time.Now(), Labels: map[string]string{"session": "sess-1"}},
		}
		if err := exp.Export(context.Background(), samples); err != nil {
			w.err = err
			return nil
		}
		// the trajectory ships as the session's ordered spans so it is reconstructable (INV-14).
		oo, ok := reg.Adapter.(*openobserve.Module)
		if !ok {
			return fmt.Errorf("expected the concrete OpenObserve module for span export")
		}
		if err := oo.ExportSpans(context.Background(), "sess-1", []string{"triage", "decide", "notify"}); err != nil {
			w.err = err
		}
		return nil
	})

	sc.Step(`^its OTLP traces and logs are exported to the configured endpoint and its trajectory is reconstructable$`, func() error {
		if w.err != nil {
			return fmt.Errorf("export must succeed: %w", w.err)
		}
		var sawLogs, sawTrace bool
		for _, p := range w.fake.posts {
			if p.method != http.MethodPost {
				return fmt.Errorf("every export must be a POST, got %s", p.method)
			}
			if strings.Contains(p.path, "/v1/logs") {
				sawLogs = true
			}
			// the trace POST must carry the session id so the completed trajectory is reconstructable.
			if strings.Contains(p.path, "/v1/traces") && strings.Contains(p.body, "sess-1") {
				sawTrace = true
			}
		}
		if !sawLogs {
			return fmt.Errorf("the session logs must be exported to the endpoint")
		}
		if !sawTrace {
			return fmt.Errorf("the session trace carrying the session id must reach the endpoint so the trajectory is reconstructable")
		}
		return nil
	})
}
