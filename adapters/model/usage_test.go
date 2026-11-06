package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// capUsage captures every ObserveUsage call. It implements BOTH observer halves, as the production
// GatewayObserver does.
type capUsage struct {
	capObs
	usages []Usage
	tiers  []string
}

func (c *capUsage) ObserveUsage(tier string, u Usage) {
	c.tiers = append(c.tiers, tier)
	c.usages = append(c.usages, u)
}

// gatewayFor spins a fake gateway answering every completion with body, and returns the client pointed at it.
func gatewayFor(t *testing.T, status int, body string) (*Gateway, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Setenv("TG_TEST_GW_KEY", "k")
	return &Gateway{BaseURL: srv.URL, APIKeyRef: config.SecretRef("env:TG_TEST_GW_KEY"), HTTP: srv.Client()}, srv.Close
}

// okBody renders a well-formed completion response, optionally carrying a usage block.
func okBody(usage string) string {
	b := `{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]`
	if usage != "" {
		b += `,"usage":` + usage
	}
	return b + `}`
}

// TestUsageBlockIsCapturedNotDiscarded is the TG-44 oracle. Before this change chatResponse had no `usage`
// member, so encoding/json dropped the block on every call and every downstream token number was a chars/4
// guess. The live gateway (dc1tg01 LiteLLM, verified 2026-08-04) returns exactly this shape:
//
//	{"completion_tokens": 4, "prompt_tokens": 162, "total_tokens": 166}
//
// KILLING MUTATION (EXECUTED 2026-08-04): change the field tag to `json:"-"`, reproducing the original
// defect exactly — the block arrives on the wire and is silently dropped on decode. Four tests go RED,
// this one with
//
//	usage was DISCARDED: Measured=false — the cost breaker is billing a chars/4 guess (TG-44)
//
// which names the real consequence: an unmeasured completion is billed from an estimate that measured
// 1.9x-13.8x LOW, so TG_COST_DAILY_BUDGET_USD stops being a ceiling. Tag restored, suite green.
func TestUsageBlockIsCapturedNotDiscarded(t *testing.T) {
	g, closeSrv := gatewayFor(t, 200, okBody(`{"prompt_tokens":162,"completion_tokens":4,"total_tokens":166}`))
	defer closeSrv()

	out, u, err := g.CompleteWithUsage(context.Background(), "u", "fast", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("completion must succeed: %v", err)
	}
	if out != "hello" {
		t.Fatalf("content=%q want hello — CompleteWithUsage must return the same text Complete does", out)
	}
	if !u.Measured {
		t.Fatalf("usage was DISCARDED: Measured=false — the cost breaker is billing a chars/4 guess (TG-44)")
	}
	if u.PromptTokens != 162 || u.CompletionTokens != 4 || u.TotalTokens != 166 {
		t.Fatalf("usage=%+v want prompt=162 completion=4 total=166 (the live gateway's reported figures)", u)
	}
}

// TestCompleteStillReturnsTextAndErrorUnchanged locks the no-behaviour-change contract: every existing
// caller uses Complete, which now delegates to CompleteWithUsage and discards the usage.
func TestCompleteStillReturnsTextAndErrorUnchanged(t *testing.T) {
	g, closeSrv := gatewayFor(t, 200, okBody(`{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}`))
	defer closeSrv()
	out, err := g.Complete(context.Background(), "u", "fast", []Message{{Role: "user", Content: "hi"}})
	if err != nil || out != "hello" {
		t.Fatalf("Complete()=(%q,%v) want (hello,nil)", out, err)
	}
}

// TestAbsentUsageIsNotMeasured: the honest half. A provider that reports nothing must yield Measured=false
// rather than a confident zero, so the caller falls back to the estimate AND says so.
func TestAbsentUsageIsNotMeasured(t *testing.T) {
	g, closeSrv := gatewayFor(t, 200, okBody(""))
	defer closeSrv()
	_, u, err := g.CompleteWithUsage(context.Background(), "u", "fast", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("completion must succeed: %v", err)
	}
	if u.Measured {
		t.Fatalf("a response with NO usage block reported Measured=true (%+v) — an absent measurement must never look like a measurement", u)
	}
}

// TestZeroUsageBlockIsNotMeasured. A present-but-empty block (some proxies emit one for a streamed
// response without stream_options) must NOT count as a measurement: billing it would charge every call
// $0 forever — quieter, and worse, than the estimate it replaced.
func TestZeroUsageBlockIsNotMeasured(t *testing.T) {
	g, closeSrv := gatewayFor(t, 200, okBody(`{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`))
	defer closeSrv()
	_, u, err := g.CompleteWithUsage(context.Background(), "u", "fast", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("completion must succeed: %v", err)
	}
	if u.Measured {
		t.Fatalf("an all-zero usage block reported Measured=true — every call would then be billed at $0")
	}
}

// TestTotalDerivedWhenOmitted: a provider that sends the two halves but not the redundant total is still
// MEASURED. Demoting it to a guess over a formatting difference would throw away a real number.
func TestTotalDerivedWhenOmitted(t *testing.T) {
	g, closeSrv := gatewayFor(t, 200, okBody(`{"prompt_tokens":100,"completion_tokens":7}`))
	defer closeSrv()
	_, u, err := g.CompleteWithUsage(context.Background(), "u", "fast", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("completion must succeed: %v", err)
	}
	if !u.Measured || u.TotalTokens != 107 {
		t.Fatalf("usage=%+v want Measured=true total=107 (derived from prompt+completion)", u)
	}
}

// TestUsageObservedOnlyForBillableCalls. The observer's missing-usage counter must mean "the provider
// billed us and told us nothing", not "the network was down". A 500 spent no tokens; counting it would
// make the honesty signal read high for a reason unrelated to token accounting.
func TestUsageObservedOnlyForBillableCalls(t *testing.T) {
	obs := &capUsage{}
	g, closeSrv := gatewayFor(t, 500, `{"error":{"message":"upstream exploded"}}`)
	defer closeSrv()
	g.Obs = obs
	if _, _, err := g.CompleteWithUsage(context.Background(), "u", "fast", []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("a 500 must return an error")
	}
	if len(obs.usages) != 0 {
		t.Fatalf("a failed (non-billable) call reported usage %+v — it must not count as a missing usage block", obs.usages)
	}
	// VACUITY FLOOR: the assertion above passes trivially if usage is NEVER observed. Prove the seam fires
	// on the path it is meant to fire on.
	obs2 := &capUsage{}
	g2, close2 := gatewayFor(t, 200, okBody(`{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}`))
	defer close2()
	g2.Obs = obs2
	if _, _, err := g2.CompleteWithUsage(context.Background(), "u", "fast", []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("completion must succeed: %v", err)
	}
	if len(obs2.usages) != 1 || !obs2.usages[0].Measured || obs2.usages[0].TotalTokens != 6 {
		t.Fatalf("billable call observed usage %+v — want exactly one measured record of 6 tokens (if this is "+
			"empty, the negative assertion above proves nothing)", obs2.usages)
	}
	if obs2.tiers[0] != "fast" {
		t.Fatalf("usage tier=%q want fast — usage must be attributable to the tier that spent it", obs2.tiers[0])
	}
}

// TestEmptyCompletionStillReportsUsage. A thinking-only model that spends its whole budget reasoning
// returns 200 with no content — and is billed for every one of those tokens. Excluding "empty" from usage
// observation would under-count exactly the calls that cost most and produced least.
func TestEmptyCompletionStillReportsUsage(t *testing.T) {
	obs := &capUsage{}
	g, closeSrv := gatewayFor(t, 200,
		`{"choices":[{"message":{"role":"assistant","content":"   "}}],"usage":{"prompt_tokens":900,"completion_tokens":1200,"total_tokens":2100}}`)
	defer closeSrv()
	g.Obs = obs
	if _, u, err := g.CompleteWithUsage(context.Background(), "u", "primary", []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("an empty 200 is not an error: %v", err)
	} else if u.TotalTokens != 2100 {
		t.Fatalf("empty-content completion reported %d tokens, want 2100 — a thinking-only call is the most expensive kind", u.TotalTokens)
	}
	if len(obs.usages) != 1 {
		t.Fatalf("empty completion observed %d usage records, want 1", len(obs.usages))
	}
}

// TestObserverWithoutUsageHalfStillWorks: the optional interface must not break a CallObserver that
// predates it (the no-behaviour-change contract for existing implementations).
func TestObserverWithoutUsageHalfStillWorks(t *testing.T) {
	plain := &capObs{}
	g, closeSrv := gatewayFor(t, 200, okBody(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
	defer closeSrv()
	g.Obs = plain
	if _, err := g.Complete(context.Background(), "u", "fast", []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("completion must succeed with a usage-less observer: %v", err)
	}
	if plain.n != 1 {
		t.Fatalf("ObserveCall fired %d times, want 1 — the existing observer contract must be untouched", plain.n)
	}
}
