package model

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// capObs captures the last (and count of) ObserveCall invocations.
type capObs struct {
	tier, caller, outcome, detail string
	status                        int
	seconds                       float64
	n                             int
}

func (c *capObs) ObserveCall(tier, caller, outcome string, statusCode int, seconds float64, detail string) {
	c.tier, c.caller, c.outcome, c.status, c.seconds, c.detail, c.n = tier, caller, outcome, statusCode, seconds, detail, c.n+1
}

func TestClassifyStatus(t *testing.T) {
	for st, want := range map[int]string{
		429: "rate_limit", 400: "bad_request", 401: "auth", 403: "auth",
		500: "provider_error", 503: "provider_error", 200: "other", 418: "other",
	} {
		if got := classifyStatus(st); got != want {
			t.Errorf("classifyStatus(%d)=%q want %q", st, got, want)
		}
	}
}

func TestClassifyTransport(t *testing.T) {
	if got := classifyTransport(context.DeadlineExceeded); got != "timeout" {
		t.Errorf("deadline → %q want timeout", got)
	}
	if got := classifyTransport(errors.New("Client.Timeout exceeded while awaiting headers")); got != "timeout" {
		t.Errorf("client timeout → %q want timeout", got)
	}
	if got := classifyTransport(errors.New("dial tcp: connection refused")); got != "transport" {
		t.Errorf("refused → %q want transport", got)
	}
}

func testGateway(t *testing.T, url string, obs CallObserver) *Gateway {
	t.Helper()
	os.Setenv("TG_TEST_LLM_KEY", "test-key")
	g := NewGateway(url, config.SecretRef("env:TG_TEST_LLM_KEY"))
	g.Obs = obs
	// These are the pre-429-retry classification/observability tests: they assert one observe per call and a
	// single-shot outcome. Opt out of the 429 backoff-retry (its behavior is covered by ratelimit_test.go) so
	// a rate_limit case stays single-attempt and fast. Negative ⇒ disabled (see the Gateway field doc).
	g.RateLimitRetries = -1
	return g
}

func serve(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
}

// The gateway classifies each outcome, reports it through Obs exactly once, and returns a typed *ModelError
// (with status + class) on failure — so a caller can distinguish transient from permanent and the metrics
// layer sees a bounded outcome. This is the observability contract the silent-error gap needed.
func TestCompleteClassifiesAndObserves(t *testing.T) {
	okBody := `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
	emptyBody := `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"length"}]}`

	cases := []struct {
		name        string
		status      int
		body        string
		wantOut     string
		wantErr     bool
		wantOutcome string
		wantStatus  int
		wantClass   string
	}{
		{"ok", 200, okBody, "hi", false, "ok", 200, ""},
		{"empty-200-content", 200, emptyBody, "", false, "empty", 200, ""},
		{"rate-limit", 429, `{"error":{"message":"slow down","code":"429"}}`, "", true, "rate_limit", 429, "rate_limit"},
		{"auth", 401, `{"error":{"message":"invalid api key"}}`, "", true, "auth", 401, "auth"},
		{"bad-request", 400, `{"error":{"message":"only temperature 1 allowed"}}`, "", true, "bad_request", 400, "bad_request"},
		{"provider-error", 503, `{"error":{"message":"upstream down"}}`, "", true, "provider_error", 503, "provider_error"},
		{"no-choices", 200, `{"choices":[]}`, "", true, "empty", 200, "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(tc.status, tc.body)
			defer srv.Close()
			obs := &capObs{}
			out, err := testGateway(t, srv.URL, obs).Complete(context.Background(), "u", "primary", []Message{{Role: "user", Content: "x"}})

			if out != tc.wantOut {
				t.Fatalf("out=%q want %q", out, tc.wantOut)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if obs.n != 1 {
				t.Fatalf("Obs called %d times, want exactly 1", obs.n)
			}
			if obs.outcome != tc.wantOutcome || obs.status != tc.wantStatus || obs.tier != "primary" {
				t.Fatalf("obs = tier=%q outcome=%q status=%d, want primary/%s/%d", obs.tier, obs.outcome, obs.status, tc.wantOutcome, tc.wantStatus)
			}
			if tc.wantClass != "" {
				var me *ModelError
				if !errors.As(err, &me) {
					t.Fatalf("err is not *ModelError: %v", err)
				}
				if me.Class != tc.wantClass || me.Status != tc.wantStatus {
					t.Fatalf("ModelError = class=%q status=%d, want %s/%d", me.Class, me.Status, tc.wantClass, tc.wantStatus)
				}
			}
		})
	}
}

// The exact failure mode found live: a slow reasoning model whose call exceeds the client timeout surfaces
// as a typed timeout ModelError and outcome=timeout — no longer a silent context-deadline swallowed by a
// batch caller.
func TestCompleteTimeoutIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		io.WriteString(w, `{"choices":[{"message":{"content":"late"}}]}`)
	}))
	defer srv.Close()
	obs := &capObs{}
	g := testGateway(t, srv.URL, obs)
	g.HTTP = &http.Client{Timeout: 40 * time.Millisecond}

	_, err := g.Complete(context.Background(), "u", "fast", nil)
	if err == nil {
		t.Fatal("want a timeout error")
	}
	var me *ModelError
	if !errors.As(err, &me) || me.Class != "timeout" {
		t.Fatalf("want *ModelError class=timeout, got %v", err)
	}
	if obs.outcome != "timeout" || obs.tier != "fast" {
		t.Fatalf("obs outcome=%q tier=%q, want timeout/fast", obs.outcome, obs.tier)
	}
}

// Embeddings share the SAME observability seam as completions, under the fixed "embed" tier — so a slow or
// failing embedding backend is visible in tg_model_* too, and returns a typed *ModelError like Complete.
func TestEmbeddingsClassifiesAndObserves(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		wantErr     bool
		wantOutcome string
		wantClass   string
	}{
		{"ok", 200, `{"data":[{"index":0,"embedding":[0.1,0.2]}]}`, false, "ok", ""},
		{"provider-error", 503, `{"error":{"message":"backend down"}}`, true, "provider_error", "provider_error"},
		{"rate-limit", 429, `{"error":{"message":"slow down"}}`, true, "rate_limit", "rate_limit"},
		{"empty-vector", 200, `{"data":[{"index":0,"embedding":[]}]}`, true, "empty", "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(tc.status, tc.body)
			defer srv.Close()
			obs := &capObs{}
			_, err := testGateway(t, srv.URL, obs).Embeddings(context.Background(), "embed-model", []string{"hello"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if obs.n != 1 || obs.tier != "embed" || obs.outcome != tc.wantOutcome {
				t.Fatalf("obs = n=%d tier=%q outcome=%q, want 1/embed/%s", obs.n, obs.tier, obs.outcome, tc.wantOutcome)
			}
			if tc.wantClass != "" {
				var me *ModelError
				if !errors.As(err, &me) || me.Class != tc.wantClass {
					t.Fatalf("want *ModelError class=%s, got %v", tc.wantClass, err)
				}
			}
		})
	}
	// empty input → no gateway call, so nothing is observed (not a phantom metric).
	obs := &capObs{}
	if _, err := testGateway(t, "http://unused.invalid", obs).Embeddings(context.Background(), "m", nil); err != nil {
		t.Fatalf("empty texts should be a nil no-op, got %v", err)
	}
	if obs.n != 0 {
		t.Fatalf("empty-texts must make no gateway call and no observation, got n=%d", obs.n)
	}
}

// A nil Obs leaves Complete behaving exactly as before (no panic, result unchanged).
func TestCompleteNilObserverSafe(t *testing.T) {
	srv := serve(200, `{"choices":[{"message":{"content":"ok"}}]}`)
	defer srv.Close()
	g := testGateway(t, srv.URL, nil)
	out, err := g.Complete(context.Background(), "u", "primary", nil)
	if err != nil || out != "ok" {
		t.Fatalf("nil-Obs Complete = (%q,%v), want (ok,nil)", out, err)
	}
}
