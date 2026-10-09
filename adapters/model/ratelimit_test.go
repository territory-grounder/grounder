package model

// Oracles for the 429 backoff-retry (TG-534). A 429 is cooperative backpressure, not a model fault: the
// eval change gate runs many sessions at once and briefly exceeds the provider's per-minute cap, and before
// this change a single 429 failed a whole arm. These tests pin the contract that makes the gate survive it:
//
//   - a transient 429 is retried with backoff and the call SUCCEEDS, and the ridden-out 429 does NOT accrue
//     toward a breaker trip (else we'd re-arm the retry-storm the breaker exists to prevent);
//   - a 429 that outlasts the bounded retries IS surfaced and DOES accrue (a genuine sustained cap);
//   - a context deadline shorter than the backoff returns promptly instead of sleeping it out;
//   - Retry-After is parsed onto the error and takes precedence over the exponential schedule.
//
// Like breaker_test.go, the load-bearing assertion is usually the SERVER HIT COUNT — it proves how many
// attempts actually happened, which a returned outcome alone cannot.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/config"
)

// recObs records EVERY observed outcome in order (capObs keeps only the last), so a retry burst is visible.
type recObs struct {
	mu       sync.Mutex
	outcomes []string
}

func (r *recObs) ObserveCall(_, _, outcome string, _ int, _ float64, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes = append(r.outcomes, outcome)
}

func (r *recObs) count(o string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, x := range r.outcomes {
		if x == o {
			n++
		}
	}
	return n
}

func (r *recObs) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.outcomes) == 0 {
		return ""
	}
	return r.outcomes[len(r.outcomes)-1]
}

const okCompletionBody = `{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`

// rateLimitThenOK serves 429 for the first failFirst requests (with the given Retry-After header, if any),
// then a 200 completion. It returns the server and a live hit counter.
func rateLimitThenOK(failFirst int, retryAfterHdr string) (*httptest.Server, *atomic.Int64) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n <= int64(failFirst) {
			if retryAfterHdr != "" {
				w.Header().Set("Retry-After", retryAfterHdr)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"slow down","code":"429"}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, okCompletionBody)
	}))
	return srv, &hits
}

func fixedClock() time.Time { return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) }

// retryGateway builds the real production pairing (NewGateway + a real breaker over a MemStore) with a tiny
// backoff base so retries do not actually stall the test, and a threshold-1 breaker so one accrual trips it —
// the discriminator between "rode out (no accrual)" and "exhausted (accrual)".
func retryGateway(t *testing.T, url string, obs CallObserver) *Gateway {
	t.Helper()
	os.Setenv("TG_TEST_LLM_KEY", "test-key")
	g := NewGateway(url, config.SecretRef("env:TG_TEST_LLM_KEY"))
	g.Obs = obs
	g.RateLimitBackoffBase = time.Millisecond
	g.Breakers = NewBreakers(breaker.NewMemStore(),
		breaker.WithThreshold(1),
		breaker.WithCooldown(time.Minute),
		breaker.WithHalfOpenSuccesses(1),
		breaker.WithClock(fixedClock))
	return g
}

// A transient 429 (two of them, under the default retry budget) is ridden out: the call succeeds, and the
// ridden-out 429s do NOT accrue — a threshold-1 breaker stays closed, so the very next call still reaches the
// server. If the intermediate 429s had recorded (the pre-TG-534 behavior), that next call would short-circuit.
func TestRateLimitRetryRidesOutTransient429(t *testing.T) {
	srv, hits := rateLimitThenOK(2, "")
	defer srv.Close()
	obs := &recObs{}
	g := retryGateway(t, srv.URL, obs)

	out, err := g.Complete(context.Background(), "u", "primary", msgs())
	if err != nil || out != "hello" {
		t.Fatalf("transient 429 should be ridden out to success; out=%q err=%v", out, err)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("want 3 hits (2×429 + 1×200), got %d", got)
	}
	if got := obs.count("rate_limit_retry"); got != 2 {
		t.Fatalf("want 2 rate_limit_retry observations, got %d (%v)", got, obs.outcomes)
	}
	if obs.last() != "ok" {
		t.Fatalf("final outcome = %q, want ok", obs.last())
	}
	// The proof of non-accrual: a second call still reaches the server (breaker not tripped by the transient 429s).
	out2, err2 := g.Complete(context.Background(), "u", "primary", msgs())
	if err2 != nil || out2 != "hello" {
		t.Fatalf("ridden-out 429 must NOT accrue toward a trip; second call refused: out=%q err=%v", out2, err2)
	}
	if got := hits.Load(); got != 4 {
		t.Fatalf("second call must reach the server; hits=%d want 4", got)
	}
}

// A 429 that outlasts the bounded retries IS surfaced as a rate_limit ModelError and DOES accrue: the
// threshold-1 breaker trips, so the next call short-circuits without a round trip.
func TestRateLimitRetryExhaustsAndAccrues(t *testing.T) {
	srv, hits := rateLimitThenOK(1000, "") // always 429
	defer srv.Close()
	obs := &recObs{}
	g := retryGateway(t, srv.URL, obs)
	g.RateLimitRetries = 2 // 1 initial + 2 retries = 3 attempts

	out, err := g.Complete(context.Background(), "u", "primary", msgs())
	if out != "" {
		t.Fatalf("exhausted call returned text %q, want empty", out)
	}
	var me *ModelError
	if !errors.As(err, &me) || me.Class != "rate_limit" || me.Status != 429 {
		t.Fatalf("want a rate_limit *ModelError (status 429), got %#v", err)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("want 3 attempts (1 + 2 retries), got %d hits", got)
	}
	if got := obs.count("rate_limit_retry"); got != 2 {
		t.Fatalf("want 2 rate_limit_retry observations, got %d (%v)", got, obs.outcomes)
	}
	if obs.last() != "rate_limit" {
		t.Fatalf("final outcome = %q, want rate_limit", obs.last())
	}
	// The exhausted 429 accrued: the breaker is open, so the next call never reaches the server.
	_, err2 := g.Complete(context.Background(), "u", "primary", msgs())
	if !errors.Is(err2, breaker.ErrOpen) {
		t.Fatalf("exhausted 429 must accrue and trip the breaker; want breaker.ErrOpen, got %v", err2)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("short-circuited call must not reach the server; hits=%d want 3", got)
	}
}

// A context deadline shorter than the backoff returns PROMPTLY (surfacing the last 429) instead of sleeping
// the full backoff — otherwise a cancelled caller would be parked for the whole retry schedule.
func TestRateLimitRetryStopsOnContextCancel(t *testing.T) {
	srv, hits := rateLimitThenOK(1000, "") // always 429
	defer srv.Close()
	obs := &recObs{}
	os.Setenv("TG_TEST_LLM_KEY", "test-key")
	g := NewGateway(srv.URL, config.SecretRef("env:TG_TEST_LLM_KEY"))
	g.Obs = obs
	g.RateLimitBackoffBase = 30 * time.Second // a backoff we must NOT actually wait out
	g.RateLimitRetries = 5

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := g.Complete(ctx, "u", "primary", msgs())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancel during backoff must return promptly; took %v", elapsed)
	}
	var me *ModelError
	if !errors.As(err, &me) || me.Class != "rate_limit" {
		t.Fatalf("want the last 429 surfaced as a rate_limit *ModelError, got %#v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("only the first attempt should reach the server before cancel; hits=%d want 1", got)
	}
}

// Retry-After from the provider is parsed onto the surfaced ModelError (delta-seconds form). Retry disabled
// here so the assertion is immediate — the backoff's PRECEDENCE of Retry-After is unit-tested separately.
func TestRetryAfterParsedOntoError(t *testing.T) {
	srv, _ := rateLimitThenOK(1000, "7")
	defer srv.Close()
	obs := &capObs{}
	g := testGateway(t, srv.URL, obs) // testGateway disables retry (RateLimitRetries = -1)

	_, err := g.Complete(context.Background(), "u", "primary", msgs())
	var me *ModelError
	if !errors.As(err, &me) {
		t.Fatalf("want a *ModelError, got %#v", err)
	}
	if me.RetryAfter != 7*time.Second {
		t.Fatalf("Retry-After not parsed onto the error: got %v want 7s", me.RetryAfter)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := map[string]time.Duration{
		"":                              0,
		"   ":                           0,
		"0":                             0,
		"-5":                            0,
		"abc":                           0,
		"Wed, 21 Oct 2026 07:28:00 GMT": 0, // HTTP-date form intentionally not honored (clock-skew risk)
		"3":                             3 * time.Second,
		" 12 ":                          12 * time.Second,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

// rateLimitBackoff: Retry-After wins over the exponential schedule and is capped; without it, the delay
// doubles per attempt and caps at rateLimitBackoffMax; a high attempt never overflows to a tiny/negative wait.
// Jitter adds up to 25%, so every assertion is a range.
func TestRateLimitBackoff(t *testing.T) {
	g := &Gateway{RateLimitBackoffBase: 500 * time.Millisecond}

	withinJitter := func(name string, got, floor time.Duration) {
		if got < floor || got > floor+floor/4 {
			t.Fatalf("%s: got %v, want [%v, %v]", name, got, floor, floor+floor/4)
		}
	}

	// Retry-After (2s) takes precedence over what the exponential base (500ms) would produce.
	withinJitter("retry-after precedence", g.rateLimitBackoff(0, 2*time.Second), 2*time.Second)
	// An absurd Retry-After is capped at retryAfterCap.
	withinJitter("retry-after cap", g.rateLimitBackoff(0, time.Hour), retryAfterCap)
	// Exponential: base, 2×base, 4×base for attempts 0,1,2 (all under the max).
	withinJitter("exp attempt 0", g.rateLimitBackoff(0, 0), 500*time.Millisecond)
	withinJitter("exp attempt 1", g.rateLimitBackoff(1, 0), time.Second)
	withinJitter("exp attempt 2", g.rateLimitBackoff(2, 0), 2*time.Second)
	// A high attempt caps at rateLimitBackoffMax, never overflowing the shift to a non-positive wait.
	withinJitter("exp capped", g.rateLimitBackoff(40, 0), rateLimitBackoffMax)
}
