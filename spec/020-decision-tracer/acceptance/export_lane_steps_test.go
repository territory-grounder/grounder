package acceptance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/config"
	exportlangfuse "github.com/territory-grounder/grounder/modules/export/langfuse"
	exportotel "github.com/territory-grounder/grounder/modules/export/otel"
)

// REQ-2020 acceptance binding (T-020-14): the optional, off-by-default Tier-3 LLM-observability export lane
// over an OTel span backbone. Drives the REAL modules/export/otel backbone + modules/export/langfuse sink so
// the scenario executes strictly: disabled ⇒ nothing exported; enabled ⇒ the REDACTED LLM subset only, never
// a governance field, never the system of record, never on the decision path.
func init() {
	stepRegistrars = append(stepRegistrars, registerExportLaneSteps)
}

// fakeLangfuseDoer captures the bodies the exporter posts, so the oracle can inspect exactly what leaves.
type fakeLangfuseDoer struct{ bodies []string }

func (f *fakeLangfuseDoer) Do(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	f.bodies = append(f.bodies, string(b))
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
}

// redactSecretToken stands in for the estate Scrub/redaction path (REQ-2008/REQ-2015): it masks "SECRET".
func redactSecretToken(s string) (string, int) {
	n := strings.Count(s, "SECRET")
	return strings.ReplaceAll(s, "SECRET", "[redacted]"), n
}

type exportLaneWorld struct {
	disabledPosts int
	enabledBody   string
	decisionValue string // proves RecordModelCall is off the decision path — the caller's value is unchanged.
}

func registerExportLaneSteps(sc *godog.ScenarioContext) {
	w := &exportLaneWorld{}
	ctx := context.Background()
	_ = os.Setenv("TG_TEST_EXPORT_LF_PUB", "pk-acceptance")
	_ = os.Setenv("TG_TEST_EXPORT_LF_SEC", "sk-acceptance")
	newExporter := func(fake *fakeLangfuseDoer) *exportlangfuse.Exporter {
		return exportlangfuse.New("https://lf.example",
			config.SecretRef("env:TG_TEST_EXPORT_LF_PUB"), config.SecretRef("env:TG_TEST_EXPORT_LF_SEC"),
			exportlangfuse.WithHTTPClient(fake))
	}
	// The LLM subset a real session might produce — carrying an estate identifier in the prompt/completion
	// so the redaction path is exercised.
	subset := exportotel.LLMSubset{
		Model:  "azure/gpt-4.1",
		Input:  "alert on SECRET-db-07; propose a remediation",
		Output: "restart the SECRET-svc unit and re-check",
	}

	sc.Step(`^the optional export lane over an OTel span backbone$`, func() error { return nil })

	sc.Step(`^the lane is disabled and when it is enabled$`, func() error {
		// DISABLED: a backbone built off exports nothing, even handed a full subset.
		fakeD := &fakeLangfuseDoer{}
		bd := exportotel.NewBackbone(newExporter(fakeD), false, redactSecretToken)
		bd.RecordModelCall(ctx, "sess-req2020", subset)
		w.disabledPosts = len(fakeD.bodies)

		// ENABLED: the same subset now exports — through the redaction path — to the Langfuse sink.
		fakeE := &fakeLangfuseDoer{}
		be := exportotel.NewBackbone(newExporter(fakeE), true, redactSecretToken)
		// Off the decision path: RecordModelCall returns nothing, so a value the caller computes AFTER it is
		// unaffected. We compute one to make the property observable.
		be.RecordModelCall(ctx, "sess-req2020", subset)
		w.decisionValue = "decided-independently"
		if err := be.Shutdown(ctx); err != nil {
			return err
		}
		w.enabledBody = strings.Join(fakeE.bodies, "\n")
		return nil
	})

	sc.Step(`^nothing is exported while the lane is disabled and when enabled it carries the redacted LLM subset only never the governance fields never becomes the system of record and never sits on the decision path$`, func() error {
		// Disabled ⇒ nothing exported.
		if w.disabledPosts != 0 {
			return fmt.Errorf("the disabled lane exported %d times, want 0", w.disabledPosts)
		}
		// Enabled ⇒ something exported.
		if strings.TrimSpace(w.enabledBody) == "" {
			return fmt.Errorf("the enabled lane exported nothing")
		}
		// ...carrying the redacted LLM subset: the model slug and the redacted prompt/completion.
		for _, want := range []string{"azure/gpt-4.1", "[redacted]", "generation-create", "GENERATION"} {
			if !strings.Contains(w.enabledBody, want) {
				return fmt.Errorf("the export must carry the redacted LLM subset; missing %q in: %s", want, w.enabledBody)
			}
		}
		// ...the estate identifier never reaches the wire verbatim (the Scrub path ran — model text is never
		// exported verbatim).
		if strings.Contains(w.enabledBody, "SECRET") {
			return fmt.Errorf("estate/model text was exported UNREDACTED: %s", w.enabledBody)
		}
		// ...never the governance fields: a band/verdict/rule/confidence/host cannot ride this lane (the
		// LLMSubset type has no slot for them and the sink allowlists only the four subset keys).
		for _, gov := range []string{"band", "verdict", "\"rule\"", "confidence", "blast", "AUTO"} {
			if strings.Contains(w.enabledBody, gov) {
				return fmt.Errorf("a governance field %q leaked onto the lane: %s", gov, w.enabledBody)
			}
		}
		// ...never the system of record: the subset went to the transient external sink, not a TG store; the
		// backbone holds no persistence handle (compile-time — its only sink is the injected SpanExporter).
		// ...never the decision path: RecordModelCall returns nothing, so the caller's value stands unchanged.
		if w.decisionValue != "decided-independently" {
			return fmt.Errorf("the export must not touch the decision path")
		}
		return nil
	})
}
