package litellm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	model "github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/config"
)

// fakeGateway answers by the model name in the request: models in failFor return 500; others return a
// completion carrying REAL usage numbers the module must record verbatim (no fabrication).
type fakeGateway struct {
	failFor map[string]bool
	seen    []string
}

func (f *fakeGateway) Do(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	var cr struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(raw, &cr)
	f.seen = append(f.seen, cr.Model)
	if f.failFor[cr.Model] {
		return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"rate limited"}}`)), Header: make(http.Header)}, nil
	}
	body := `{"choices":[{"message":{"role":"assistant","content":"the model said this"}}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func newModule(t *testing.T, fake *fakeGateway) *Module {
	t.Setenv("TG_TEST_LITELLM_KEY", "sk-x")
	resolver := model.Resolver{Components: map[string]string{"agent": "zai-primary"}, Default: "zai-primary"}
	return New("https://litellm.test", config.SecretRef("env:TG_TEST_LITELLM_KEY"), resolver, []string{"deepseek", "mistral", "ollama"}, WithHTTPClient(fake))
}

func TestServesOverOneEndpointAndRecordsRealUsage(t *testing.T) {
	fake := &fakeGateway{}
	m := newModule(t, fake)
	text, u, err := m.Complete(context.Background(), "agent", "agent-1", []model.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("completion must succeed: %v", err)
	}
	if text != "the model said this" {
		t.Errorf("model text (untrusted data) not returned: %q", text)
	}
	if u.Model != "zai-primary" || u.TotalTokens != 18 || u.PromptTokens != 11 || u.CompletionTokens != 7 {
		t.Errorf("usage must be READ from the response, not fabricated: %+v", u)
	}
	rec := m.Usage()
	if len(rec) != 1 || rec[0].TotalTokens != 18 {
		t.Errorf("real-token usage must be recorded per request: %+v", rec)
	}
}

func TestFallbackLadderAdvancesOnError(t *testing.T) {
	// the primary (zai) errors; the ladder must advance to the next rung and serve.
	fake := &fakeGateway{failFor: map[string]bool{"zai-primary": true}}
	m := newModule(t, fake)
	_, u, err := m.Complete(context.Background(), "agent", "agent-1", []model.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("the fallback ladder must recover: %v", err)
	}
	if u.Model != "deepseek" {
		t.Errorf("must advance to the next ladder rung, served %q", u.Model)
	}
	if len(fake.seen) != 2 || fake.seen[0] != "zai-primary" || fake.seen[1] != "deepseek" {
		t.Errorf("must try primary then advance: %v", fake.seen)
	}
}

func TestBreakerShortCircuitsDeadRung(t *testing.T) {
	// zai-primary is persistently down. After its breaker trips (3 consecutive failures) the primary must be
	// SKIPPED on the next request — the resilience improvement over the predecessor's retry-every-rung ladder.
	fake := &fakeGateway{failFor: map[string]bool{"zai-primary": true}}
	m := newModule(t, fake)
	ctx := context.Background()
	msg := []model.Message{{Role: "user", Content: "hi"}}
	countPrimary := func() int {
		n := 0
		for _, s := range fake.seen {
			if s == "zai-primary" {
				n++
			}
		}
		return n
	}
	// Three calls each try+fail the primary (recovering via deepseek), tripping the primary breaker at 3.
	for i := 0; i < 3; i++ {
		if _, u, err := m.Complete(ctx, "agent", "u", msg); err != nil || u.Model != "deepseek" {
			t.Fatalf("call %d must recover via deepseek: model=%q err=%v", i, u.Model, err)
		}
	}
	if got := countPrimary(); got != 3 {
		t.Fatalf("primary should be attempted exactly 3 times before tripping, got %d", got)
	}
	// Fourth call: the primary breaker is OPEN → the primary is short-circuited (no new attempt), and the
	// request still recovers via the next rung.
	if _, u, err := m.Complete(ctx, "agent", "u", msg); err != nil || u.Model != "deepseek" {
		t.Fatalf("post-trip call must recover via deepseek: model=%q err=%v", u.Model, err)
	}
	if got := countPrimary(); got != 3 {
		t.Fatalf("an open breaker must SKIP the dead rung (still 3 attempts), got %d", got)
	}
}

func TestAllRungsFailReturnsError(t *testing.T) {
	fake := &fakeGateway{failFor: map[string]bool{"zai-primary": true, "deepseek": true, "mistral": true, "ollama": true}}
	m := newModule(t, fake)
	if _, _, err := m.Complete(context.Background(), "agent", "a", nil); err == nil {
		t.Fatal("all rungs failing must return an error")
	}
	if len(m.Usage()) != 0 {
		t.Error("a fully-failed request must record no usage (no fabrication)")
	}
}
