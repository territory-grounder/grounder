package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRenderExposesMutationAndBreakerGauges proves the exposition renders mutation_enabled 0 (the OFF gate)
// and the circuit_breaker_state gauge with the expected HELP/TYPE headers and values.
func TestRenderExposesMutationAndBreakerGauges(t *testing.T) {
	out := Render([]Sample{
		{Name: "mutation_enabled", Kind: Gauge, Help: "mutation gate on(1)/off(0)", Value: 0},
		{Name: "circuit_breaker_state", Kind: Gauge, Help: "breaker 0 closed/1 half/2 open", Value: 0},
		{Name: "deviation_count", Kind: Counter, Help: "trip-worthy deviations", Value: 0},
	})
	for _, want := range []string{
		"# HELP mutation_enabled mutation gate on(1)/off(0)",
		"# TYPE mutation_enabled gauge",
		"mutation_enabled 0",
		"# TYPE circuit_breaker_state gauge",
		"circuit_breaker_state 0",
		"# TYPE deviation_count counter",
		"deviation_count 0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("exposition missing %q; got:\n%s", want, out)
		}
	}
	// mutation_enabled must render exactly 0 here — the read-only posture. A stray "mutation_enabled 1"
	// would be the alert-worthy unexpected-mutation state and must never appear over an OFF gate.
	if strings.Contains(out, "mutation_enabled 1") {
		t.Fatalf("mutation_enabled must be 0 over an OFF gate; got:\n%s", out)
	}
}

// TestRenderGroupsLabelsAndIsDeterministic proves labelled samples of one name share a single header and
// that repeated renders are byte-identical (sorted groups + sorted labels).
func TestRenderGroupsLabelsAndIsDeterministic(t *testing.T) {
	samples := []Sample{
		{Name: "breaker_state", Kind: Gauge, Help: "h", Value: 2, Labels: map[string]string{"name": "mutation"}},
		{Name: "breaker_state", Kind: Gauge, Help: "h", Value: 0, Labels: map[string]string{"name": "model"}},
	}
	a, b := Render(samples), Render(samples)
	if a != b {
		t.Fatalf("Render must be deterministic:\n%q\n%q", a, b)
	}
	if strings.Count(a, "# TYPE breaker_state gauge") != 1 {
		t.Fatalf("all samples of one name must share ONE TYPE header; got:\n%s", a)
	}
	if !strings.Contains(a, `breaker_state{name="model"} 0`) || !strings.Contains(a, `breaker_state{name="mutation"} 2`) {
		t.Fatalf("labelled samples not rendered correctly:\n%s", a)
	}
}

// TestHandlerServesTextAndRejectsNonGet proves the /metrics handler serves the exposition on GET and 405s a POST.
func TestHandlerServesTextAndRejectsNonGet(t *testing.T) {
	h := Handler(func() []Sample {
		return []Sample{{Name: "mutation_enabled", Kind: Gauge, Help: "gate", Value: 0}}
	})

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain", ct)
	}
	if !strings.Contains(rec.Body.String(), "mutation_enabled 0") {
		t.Fatalf("body missing mutation_enabled 0; got:\n%s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /metrics = %d, want 405", rec.Code)
	}
}
