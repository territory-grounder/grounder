package model

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureBody serves a canned OK completion and records the last request body it received.
func captureBody(t *testing.T, sink *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*sink = b
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
}

// TG-48: the model client set NO per-completion output cap, so a runaway high-severity investigation could
// spend an unbounded number of output tokens per call and the cost breaker only trips AFTER the daily budget
// is blown. Gateway.MaxTokens now sends `max_tokens` on every chat request, INERT by default (0 ⇒ omitted, so
// the model/gateway default stands — today's behaviour). Killing mutation: drop `MaxTokens: g.MaxTokens` in
// do() ⇒ the armed request carries no cap ⇒ the armed assertion goes RED.
func TestMaxTokensCapArmedAndInert(t *testing.T) {
	// ARMED: the ceiling rides on the request, on the wire.
	var body []byte
	srv := captureBody(t, &body)
	defer srv.Close()
	g := testGateway(t, srv.URL, nil)
	g.MaxTokens = 4096
	if _, err := g.Complete(context.Background(), "u", "primary", []Message{{Role: "user", Content: "x"}}); err != nil {
		t.Fatalf("armed complete: %v", err)
	}
	var got chatRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if got.MaxTokens != 4096 {
		t.Fatalf("an armed Gateway.MaxTokens must send max_tokens=4096, got %d — a runaway completion is still unbounded", got.MaxTokens)
	}
	if !bytes.Contains(body, []byte(`"max_tokens":4096`)) {
		t.Fatalf("the request WIRE carries no max_tokens cap: %s", body)
	}

	// INERT: MaxTokens 0 ⇒ omitempty drops it ⇒ no accidental cap on the default deployment.
	var body2 []byte
	srv2 := captureBody(t, &body2)
	defer srv2.Close()
	g2 := testGateway(t, srv2.URL, nil) // MaxTokens left 0
	if _, err := g2.Complete(context.Background(), "u", "primary", []Message{{Role: "user", Content: "x"}}); err != nil {
		t.Fatalf("inert complete: %v", err)
	}
	if bytes.Contains(body2, []byte("max_tokens")) {
		t.Fatalf("an unset MaxTokens must OMIT max_tokens (0 = model default), but the wire carries it: %s", body2)
	}
}
