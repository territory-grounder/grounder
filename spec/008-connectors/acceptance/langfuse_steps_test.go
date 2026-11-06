package acceptance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/cucumber/godog"

	observability "github.com/territory-grounder/grounder/adapters/observability"
	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/observability/langfuse"
)

// Langfuse observability (REQ-819): a per-session LLM/agent trace keyed by the session id is recorded to
// the configured endpoint, so the session's trajectory is reconstructable from the outside (INV-14).
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerLangfuseSteps)
}

// langfuseFakeDoer stands in for Langfuse: it records the POST path and body of the last request so the
// oracle can assert the recorded trace is keyed by the session id, and returns a canned 200.
type langfuseFakeDoer struct {
	path string
	body string
}

func (d *langfuseFakeDoer) Do(req *http.Request) (*http.Response, error) {
	d.path = req.URL.Path
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		d.body = string(b)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
}

type langfuseWorld struct {
	reg  *modules.Registry
	doer *langfuseFakeDoer
	err  error
}

func registerLangfuseSteps(sc *godog.ScenarioContext) {
	w := &langfuseWorld{}

	sc.Step(`^the Langfuse observability module is registered and enabled$`, func() error {
		_ = os.Setenv("TG_LANGFUSE_ACCEPT_TOKEN", "tok")
		_ = os.Setenv("TG_LANGFUSE_ACCEPT_PUBLIC", "pub")
		w.doer = &langfuseFakeDoer{}
		w.reg = modules.NewRegistry()
		mod := langfuse.New("https://langfuse.test", "env:TG_LANGFUSE_ACCEPT_PUBLIC", "env:TG_LANGFUSE_ACCEPT_TOKEN", langfuse.WithHTTPClient(w.doer))
		return w.reg.Register(modules.Registration{
			Surface: modules.SurfaceObservability, SourceType: langfuse.SourceType, Capability: "observability.langfuse", Enabled: true, Adapter: mod,
		})
	})

	sc.Step(`^a session invokes the agent loop$`, func() error {
		reg, err := w.reg.Resolve(modules.SurfaceObservability, langfuse.SourceType)
		if err != nil {
			return fmt.Errorf("the enabled module must resolve: %w", err)
		}
		if _, ok := reg.Adapter.(observability.Exporter); !ok {
			return fmt.Errorf("the registered adapter must satisfy adapters/observability.Exporter")
		}
		mod, ok := reg.Adapter.(*langfuse.Module)
		if !ok {
			return fmt.Errorf("expected the concrete Langfuse module to record the session trace")
		}
		w.err = mod.Record(context.Background(), "sess-7", []string{"llm.call", "agent.step", "tool.invoke"})
		return nil
	})

	sc.Step(`^a per-session LLM and agent trace keyed by the session id is recorded to the configured endpoint$`, func() error {
		if w.err != nil {
			return fmt.Errorf("recording the per-session trace must succeed: %w", w.err)
		}
		// The corrected module emits the trace as trace-create/observation-create events on the batch
		// ingestion route (not the non-write /traces/{id} resource); the session id keys the trace in the
		// event body, not the URL path.
		if !strings.HasPrefix(w.doer.path, "/api/public/ingestion") {
			return fmt.Errorf("the trace must be recorded to the Langfuse ingestion endpoint, got path %q", w.doer.path)
		}
		if !strings.Contains(w.doer.body, `"sessionId":"sess-7"`) {
			return fmt.Errorf("the recorded trace body must be keyed by the session id, got %q", w.doer.body)
		}
		return nil
	})
}
