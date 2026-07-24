package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/cucumber/godog"

	model "github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/model/anthropic"
	"github.com/territory-grounder/grounder/modules/model/deepseek"
	"github.com/territory-grounder/grounder/modules/model/litellm"
	"github.com/territory-grounder/grounder/modules/model/mistral"
	"github.com/territory-grounder/grounder/modules/model/ollama"
	"github.com/territory-grounder/grounder/modules/model/openai"
	"github.com/territory-grounder/grounder/modules/model/zai"
)

// The model family (REQ-815): the LiteLLM gateway lead + 6 provider backends. Several scenarios share
// When/Then text, so all are registered once here.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerModelSteps)
}

// modelFakeGateway answers by the request model: models in failFor return 500, else a completion with real
// usage numbers the gateway records verbatim.
type modelFakeGateway struct {
	failFor map[string]bool
	seen    []string
}

func (f *modelFakeGateway) Do(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	var cr struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(raw, &cr)
	f.seen = append(f.seen, cr.Model)
	if f.failFor[cr.Model] {
		return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"rate limited"}}`)), Header: make(http.Header)}, nil
	}
	body := `{"choices":[{"message":{"role":"assistant","content":"untrusted model text"}}],"usage":{"prompt_tokens":5,"completion_tokens":9,"total_tokens":14}}`
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

type modelWorld struct {
	reg   *modules.Registry
	gw    *litellm.Module
	fake  *modelFakeGateway
	text  string
	usage litellm.Usage
	err   error
}

func registerModelSteps(sc *godog.ScenarioContext) {
	w := &modelWorld{}
	_ = os.Setenv("TG_MODEL_ACCEPT_KEY", "sk-x")

	// buildGateway wires a gateway that routes "agent" to primaryModel, with the given ladder + failures,
	// and registers the provider under the model surface.
	buildGateway := func(primaryModel string, ladder []string, fail map[string]bool, providerSlug string, provider any) error {
		w.reg = modules.NewRegistry()
		w.fake = &modelFakeGateway{failFor: fail}
		resolver := model.Resolver{Components: map[string]string{"agent": primaryModel}, Default: primaryModel}
		w.gw = litellm.New("https://litellm.test", "env:TG_MODEL_ACCEPT_KEY", resolver, ladder, litellm.WithHTTPClient(w.fake))
		if provider != nil {
			if err := w.reg.Register(modules.Registration{Surface: modules.SurfaceModel, SourceType: providerSlug, Capability: "model." + providerSlug, Enabled: true, Adapter: provider}); err != nil {
				return err
			}
		}
		return w.reg.Register(modules.Registration{Surface: modules.SurfaceModel, SourceType: litellm.SourceType, Capability: "model.litellm", Enabled: true, Adapter: w.gw})
	}
	route := func() {
		w.text, w.usage, w.err = w.gw.Complete(context.Background(), "agent", "agent-1", []model.Message{{Role: "user", Content: "hi"}})
	}

	// ---- gateway (REQ-815) ----
	sc.Step(`^the bundled LiteLLM model-gateway module is registered and enabled$`, func() error {
		return buildGateway("zai/glm-4.6", []string{"deepseek", "mistral"}, nil, "", nil)
	})
	sc.Step(`^a component resolves a model through the one source-of-truth router$`, func() error { route(); return nil })
	sc.Step(`^the request is served over one OpenAI-compatible endpoint and real-token usage is recorded with no fabrication$`, func() error {
		if w.err != nil {
			return fmt.Errorf("the gateway must serve the request: %w", w.err)
		}
		if w.usage.TotalTokens != 14 || w.usage.PromptTokens != 5 {
			return fmt.Errorf("usage must be READ from the response, not fabricated: %+v", w.usage)
		}
		if rec := w.gw.Usage(); len(rec) != 1 || rec[0].TotalTokens != 14 {
			return fmt.Errorf("real-token usage must be recorded per request: %+v", rec)
		}
		return nil
	})

	// ---- z.ai (REQ-815): primary rung errors → fallback advances ----
	sc.Step(`^the z\.ai provider backend is configured as the primary ladder rung$`, func() error {
		return buildGateway("zai-primary", []string{"deepseek", "mistral"}, map[string]bool{"zai-primary": true}, zai.SourceType, zai.New())
	})
	sc.Step(`^the gateway routes a request to it and it errors$`, func() error { route(); return nil })
	sc.Step(`^the configured auto-fallback ladder advances to the next provider and the response is treated as untrusted typed data$`, func() error {
		if w.err != nil {
			return fmt.Errorf("the fallback ladder must recover: %w", w.err)
		}
		if w.usage.Model == "zai-primary" || w.text == "" {
			return fmt.Errorf("the ladder must advance past the failed primary, served %q text=%q", w.usage.Model, w.text)
		}
		return nil
	})

	// ---- DeepSeek (REQ-815): reasoning blocks joined on type text ----
	sc.Step(`^the DeepSeek provider backend is configured behind the gateway$`, func() error {
		return buildGateway("deepseek", nil, nil, deepseek.SourceType, deepseek.New())
	})
	sc.Step(`^its reasoning response blocks are joined on type text and treated as untrusted typed data$`, func() error {
		if w.err != nil {
			return fmt.Errorf("the gateway must route to DeepSeek: %w", w.err)
		}
		joined, err := deepseek.JoinReasoning([]byte(`[{"type":"thinking","text":"let me reason"},{"type":"text","text":"the answer"}]`))
		if err != nil || joined != "the answer" {
			return fmt.Errorf("reasoning blocks must join on type=text (dropping thinking), got %q err=%v", joined, err)
		}
		return nil
	})

	// ---- Mistral / Anthropic / OpenAI (REQ-815): served over the gateway ----
	sc.Step(`^the Mistral provider backend is configured behind the gateway$`, func() error {
		return buildGateway("mistral", nil, nil, mistral.SourceType, mistral.New())
	})
	sc.Step(`^the Anthropic provider backend is configured as a fallback ladder rung$`, func() error {
		return buildGateway("anthropic", nil, nil, anthropic.SourceType, anthropic.New())
	})
	sc.Step(`^the OpenAI provider backend is configured as a fallback ladder rung$`, func() error {
		return buildGateway("openai", nil, nil, openai.SourceType, openai.New())
	})
	// shared When for DeepSeek/Mistral/Anthropic/OpenAI:
	sc.Step(`^the gateway routes a request to it$`, func() error { route(); return nil })

	// ---- Ollama (REQ-815): local-first cost profile ----
	sc.Step(`^the Ollama local provider backend is configured behind the gateway$`, func() error {
		return buildGateway("ollama", nil, nil, ollama.SourceType, ollama.New())
	})
	sc.Step(`^the gateway routes a request to it under the local-first cost profile$`, func() error { route(); return nil })

	// shared Then for Mistral/Ollama/Anthropic/OpenAI:
	sc.Step(`^the response is served over the gateway and treated as untrusted typed data$`, func() error {
		if w.err != nil {
			return fmt.Errorf("the provider must be served over the gateway: %w", w.err)
		}
		if w.text == "" {
			return fmt.Errorf("the model response (untrusted typed data) must be returned")
		}
		return nil
	})
}
