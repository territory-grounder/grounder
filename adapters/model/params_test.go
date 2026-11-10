package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// TG-670 (F18): the judge's temperature/seed were declared in rubric.json and never sent, and the provider's
// reported model was never read. CompleteWithParams sends exactly the pinned parameters and returns the reported
// model; Complete is byte-identical to before (no sampling keys at all).
func TestCompleteWithParamsSendsThePinnedSamplingAndReturnsTheReportedModel(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		bodies = append(bodies, m)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","model":"claude-opus-4-1-20250805","choices":[{"message":{"role":"assistant","content":"{\"ok\":1}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`))
	}))
	defer srv.Close()
	t.Setenv("TG_TEST_GW_KEY", "k")
	g := &Gateway{BaseURL: srv.URL, APIKeyRef: config.SecretRef("env:TG_TEST_GW_KEY"), HTTP: srv.Client()}

	c, err := g.CompleteWithParams(context.Background(), "session-judge", "judge", []Message{{Role: "user", Content: "p"}}, ParamsOf(0, 0))
	if err != nil {
		t.Fatalf("CompleteWithParams: %v", err)
	}
	if c.Text != `{"ok":1}` || c.Model != "claude-opus-4-1-20250805" || !c.Usage.Measured || c.Usage.TotalTokens != 12 {
		t.Fatalf("completion = %+v, want the text, the REPORTED model and the measured usage", c)
	}
	if _, err := g.Complete(context.Background(), "runner", "fast", []Message{{Role: "user", Content: "p"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected two requests, got %d", len(bodies))
	}
	// The parameterised call carries temperature 0 and seed 0 EXPLICITLY (a zero is a value, not an omission).
	if temp, ok := bodies[0]["temperature"]; !ok || temp != float64(0) {
		t.Errorf("temperature not sent as 0: %v", bodies[0])
	}
	if seed, ok := bodies[0]["seed"]; !ok || seed != float64(0) {
		t.Errorf("seed not sent as 0: %v", bodies[0])
	}
	// The plain call carries NEITHER: every pre-existing caller's request is unchanged.
	for _, k := range []string{"temperature", "seed"} {
		if _, ok := bodies[1][k]; ok {
			t.Errorf("Complete sent %q — the agent's requests must not change", k)
		}
	}
	if p := ParamsOf(0.2, 7); p.Sent() != "temperature=0.2 seed=7" {
		t.Errorf("Sent() = %q", p.Sent())
	}
	if (Params{}).Sent() != "" {
		t.Error("no parameters must render as an empty sent string, never as a fabricated one")
	}
}

// A provider that omits the top-level model leaves Completion.Model empty — unrecorded, never guessed from the alias.
func TestAnOmittedReportedModelStaysUnrecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer srv.Close()
	t.Setenv("TG_TEST_GW_KEY", "k")
	g := &Gateway{BaseURL: srv.URL, APIKeyRef: config.SecretRef("env:TG_TEST_GW_KEY"), HTTP: srv.Client()}
	c, err := g.CompleteWithParams(context.Background(), "u", "judge", []Message{{Role: "user", Content: "p"}}, ParamsOf(0, 0))
	if err != nil || c.Text != "hi" || c.Model != "" {
		t.Fatalf("completion = %+v (%v), want text hi and an EMPTY model", c, err)
	}
}
