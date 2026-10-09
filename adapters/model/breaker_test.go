package model

// Oracles for the PRODUCTION model-path breaker (TG-221 / PORT-FIDELITY-AUDIT finding #24).
//
// These drive the REAL call path end to end: a real *Gateway (constructed by NewGateway, the same
// constructor cmd/worker uses), a real *breaker.Breaker over the real breaker.MemStore, a real
// *http.Client and a real httptest server. Nothing on the path under test is reimplemented — the only
// injected things are core/breaker's own documented test seams (the in-memory Store twin and WithClock),
// which is what makes trip / half-open / reset deterministic instead of wall-clock flaky.
//
// The load-bearing assertion in almost every case is the SERVER HIT COUNT: a breaker that "returns an
// error" but still performs the round trip has not bounded anything. Counting hits is what proves the call
// was actually short-circuited.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/config"
)

// countingServer serves a scripted status/body and counts how many requests actually reached it.
type countingServer struct {
	*httptest.Server
	hits   atomic.Int64
	status atomic.Int64
	body   atomic.Value // string
}

func newCountingServer(status int, body string) *countingServer {
	cs := &countingServer{}
	cs.status.Store(int64(status))
	cs.body.Store(body)
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(cs.status.Load()))
		_, _ = w.Write([]byte(cs.body.Load().(string)))
	}))
	return cs
}

func (cs *countingServer) serve(status int, body string) {
	cs.status.Store(int64(status))
	cs.body.Store(body)
}

const okCompletion = `{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`
const okEmbedding = `{"data":[{"index":0,"embedding":[0.1,0.2]}]}`

// guardedGateway builds the REAL production pairing: NewGateway + NewBreakers over a MemStore with an
// injected clock. Returns the gateway, the clock knob, and the observer.
func guardedGateway(t *testing.T, url string, threshold int, cooldown time.Duration) (*Gateway, *time.Time, *capObs) {
	t.Helper()
	os.Setenv("TG_TEST_LLM_KEY", "test-key")
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	clock := &now
	obs := &capObs{}
	g := NewGateway(url, config.SecretRef("env:TG_TEST_LLM_KEY"))
	g.Obs = obs
	g.Breakers = NewBreakers(breaker.NewMemStore(),
		breaker.WithThreshold(threshold),
		breaker.WithCooldown(cooldown),
		breaker.WithHalfOpenSuccesses(1),
		breaker.WithClock(func() time.Time { return *clock }))
	return g, clock, obs
}

func msgs() []Message { return []Message{{Role: "user", Content: "hi"}} }

// The whole point of the finding: consecutive upstream failures TRIP the circuit and the next call never
// reaches the gateway. It fails LOUD — a typed breaker_open error wrapping breaker.ErrOpen — and NEVER
// returns an empty string with a nil error, which is the exact shape that would become an empty scorecard.
func TestCompleteTripsAndShortCircuits(t *testing.T) {
	srv := newCountingServer(500, `{"error":{"message":"upstream down"}}`)
	defer srv.Close()
	g, _, obs := guardedGateway(t, srv.URL, 3, time.Minute)

	for i := 0; i < 3; i++ {
		if _, err := g.Complete(context.Background(), "u", "primary", msgs()); err == nil {
			t.Fatalf("call %d: want an upstream error", i)
		}
	}
	if got := srv.hits.Load(); got != 3 {
		t.Fatalf("three admitted failures should hit the server 3 times, got %d", got)
	}

	out, err := g.Complete(context.Background(), "u", "primary", msgs())
	if err == nil {
		t.Fatal("a tripped circuit must return an ERROR, never a silent empty result")
	}
	if out != "" {
		t.Fatalf("short-circuited call returned text %q", out)
	}
	if !errors.Is(err, breaker.ErrOpen) {
		t.Fatalf("want errors.Is(err, breaker.ErrOpen); got %v", err)
	}
	var me *ModelError
	if !errors.As(err, &me) || me.Class != ClassBreakerOpen {
		t.Fatalf("want a typed *ModelError of class %q, got %#v", ClassBreakerOpen, err)
	}
	if got := srv.hits.Load(); got != 3 {
		t.Fatalf("the short-circuited call must NOT reach the gateway; hits=%d want 3", got)
	}
	// Observable: the refusal is reported through the same seam as every other call, under its own bounded
	// outcome label, so `tg_model_calls_total{outcome="breaker_open"}` moves.
	if obs.outcome != ClassBreakerOpen || obs.status != 0 {
		t.Fatalf("short-circuit observation = (%q, %d), want (%q, 0)", obs.outcome, obs.status, ClassBreakerOpen)
	}
}

// After the cooldown, Allow admits exactly ONE half-open probe; a success closes the circuit and traffic
// resumes. This is the real three-state recovery, driven through Gateway.Complete — not a breaker unit test.
func TestCompleteHalfOpenProbeRecovers(t *testing.T) {
	srv := newCountingServer(500, `{"error":{"message":"down"}}`)
	defer srv.Close()
	g, clock, _ := guardedGateway(t, srv.URL, 2, time.Minute)

	for i := 0; i < 2; i++ {
		_, _ = g.Complete(context.Background(), "u", "primary", msgs())
	}
	if _, err := g.Complete(context.Background(), "u", "primary", msgs()); !errors.Is(err, breaker.ErrOpen) {
		t.Fatalf("want an open circuit after 2 failures, got %v", err)
	}
	hitsWhileOpen := srv.hits.Load()

	// Still inside the cooldown: still short-circuited, still no round trip.
	*clock = clock.Add(30 * time.Second)
	if _, err := g.Complete(context.Background(), "u", "primary", msgs()); !errors.Is(err, breaker.ErrOpen) {
		t.Fatalf("inside the cooldown the circuit must stay open, got %v", err)
	}
	if srv.hits.Load() != hitsWhileOpen {
		t.Fatal("a call inside the cooldown reached the gateway — the circuit is not bounding anything")
	}

	// Past the cooldown the upstream has recovered: the probe is admitted and closes the circuit.
	*clock = clock.Add(90 * time.Second)
	srv.serve(200, okCompletion)
	out, err := g.Complete(context.Background(), "u", "primary", msgs())
	if err != nil || out != "hello" {
		t.Fatalf("half-open probe = (%q, %v), want the real completion", out, err)
	}
	if srv.hits.Load() != hitsWhileOpen+1 {
		t.Fatalf("the probe should have made exactly ONE round trip; hits=%d want %d", srv.hits.Load(), hitsWhileOpen+1)
	}
	rec, _ := g.Breakers.For("primary").Snapshot(context.Background())
	if rec.State != breaker.StateClosed {
		t.Fatalf("a successful probe must CLOSE the circuit; state=%s", rec.State)
	}
	// And traffic genuinely flows again.
	if _, err := g.Complete(context.Background(), "u", "primary", msgs()); err != nil {
		t.Fatalf("post-recovery call failed: %v", err)
	}
}

// A failure DURING the half-open probe re-opens immediately — one canary, not a slow bleed back into a
// broken upstream.
func TestCompleteHalfOpenFailureReopens(t *testing.T) {
	srv := newCountingServer(500, `{"error":{"message":"down"}}`)
	defer srv.Close()
	g, clock, _ := guardedGateway(t, srv.URL, 2, time.Minute)

	for i := 0; i < 2; i++ {
		_, _ = g.Complete(context.Background(), "u", "primary", msgs())
	}
	*clock = clock.Add(2 * time.Minute)
	// The probe is admitted (it reaches the still-broken server) and fails.
	before := srv.hits.Load()
	if _, err := g.Complete(context.Background(), "u", "primary", msgs()); errors.Is(err, breaker.ErrOpen) {
		t.Fatal("the post-cooldown probe must be ADMITTED, not short-circuited")
	}
	if srv.hits.Load() != before+1 {
		t.Fatal("the probe did not reach the gateway")
	}
	rec, _ := g.Breakers.For("primary").Snapshot(context.Background())
	if rec.State != breaker.StateOpen {
		t.Fatalf("a failed probe must RE-OPEN the circuit; state=%s", rec.State)
	}
	if _, err := g.Complete(context.Background(), "u", "primary", msgs()); !errors.Is(err, breaker.ErrOpen) {
		t.Fatalf("the re-opened circuit must short-circuit the next call, got %v", err)
	}
}

// Reset is the administrative re-arm: it clears a trip without waiting for the cooldown, and the very next
// call on the real path is admitted.
func TestCompleteResetRearmsTheRealPath(t *testing.T) {
	srv := newCountingServer(500, `{"error":{"message":"down"}}`)
	defer srv.Close()
	g, _, _ := guardedGateway(t, srv.URL, 2, time.Hour)

	for i := 0; i < 2; i++ {
		_, _ = g.Complete(context.Background(), "u", "primary", msgs())
	}
	if _, err := g.Complete(context.Background(), "u", "primary", msgs()); !errors.Is(err, breaker.ErrOpen) {
		t.Fatal("want an open circuit")
	}
	if err := g.Breakers.For("primary").Reset(context.Background()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	srv.serve(200, okCompletion)
	before := srv.hits.Load()
	out, err := g.Complete(context.Background(), "u", "primary", msgs())
	if err != nil || out != "hello" {
		t.Fatalf("post-reset call = (%q, %v), want the real completion", out, err)
	}
	if srv.hits.Load() != before+1 {
		t.Fatal("the post-reset call did not reach the gateway")
	}
}

// A 400 is a defect in the request THIS process sent, not an upstream outage. Tripping on it would let one
// malformed prompt short-circuit every other component's model calls — a self-inflicted outage.
func TestBadRequestNeverTripsTheCircuit(t *testing.T) {
	srv := newCountingServer(400, `{"error":{"message":"context length exceeded"}}`)
	defer srv.Close()
	g, _, _ := guardedGateway(t, srv.URL, 2, time.Minute)

	for i := 0; i < 5; i++ {
		if _, err := g.Complete(context.Background(), "u", "primary", msgs()); errors.Is(err, breaker.ErrOpen) {
			t.Fatalf("call %d was short-circuited — a 400 must not trip the circuit", i)
		}
	}
	if srv.hits.Load() != 5 {
		t.Fatalf("every 400 must still reach the gateway; hits=%d want 5", srv.hits.Load())
	}
	rec, _ := g.Breakers.For("primary").Snapshot(context.Background())
	if rec.State != breaker.StateClosed {
		t.Fatalf("state after five 400s = %s, want closed", rec.State)
	}
}

// One breaker per model TIER: a dead judge tier must not short-circuit a healthy agent tier.
func TestBreakersAreIsolatedPerTier(t *testing.T) {
	srv := newCountingServer(500, `{"error":{"message":"down"}}`)
	defer srv.Close()
	g, _, _ := guardedGateway(t, srv.URL, 2, time.Minute)

	for i := 0; i < 2; i++ {
		_, _ = g.Complete(context.Background(), "u", "judge-tier", msgs())
	}
	if _, err := g.Complete(context.Background(), "u", "judge-tier", msgs()); !errors.Is(err, breaker.ErrOpen) {
		t.Fatal("the judge tier should be open")
	}
	srv.serve(200, okCompletion)
	if _, err := g.Complete(context.Background(), "u", "primary", msgs()); err != nil {
		t.Fatalf("the healthy agent tier must be unaffected by the judge tier's trip: %v", err)
	}
}

// The RAG embedding backend gets the same named, persisted breaker (the predecessor's rag_embed_ollama) —
// the lane finding #24 called out as bounded per-call only.
func TestEmbeddingsAreGuardedToo(t *testing.T) {
	srv := newCountingServer(503, `{"error":{"message":"embedding backend down"}}`)
	defer srv.Close()
	g, _, obs := guardedGateway(t, srv.URL, 2, time.Minute)

	for i := 0; i < 2; i++ {
		if _, err := g.Embeddings(context.Background(), "nomic-embed", []string{"a"}); err == nil {
			t.Fatal("want an upstream error")
		}
	}
	before := srv.hits.Load()
	vecs, err := g.Embeddings(context.Background(), "nomic-embed", []string{"a"})
	if !errors.Is(err, breaker.ErrOpen) {
		t.Fatalf("want a short-circuited embedding call, got %v", err)
	}
	if vecs != nil {
		t.Fatal("a short-circuited embedding call must return NO vectors, never fabricated ones")
	}
	if srv.hits.Load() != before {
		t.Fatal("the short-circuited embedding call reached the backend")
	}
	if obs.tier != "embed" || obs.outcome != ClassBreakerOpen {
		t.Fatalf("embed refusal observed as (%q,%q), want (embed,%s)", obs.tier, obs.outcome, ClassBreakerOpen)
	}
	// Recovery works on this path too.
	srv.serve(200, okEmbedding)
	if err := g.Breakers.For("nomic-embed").Reset(context.Background()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := g.Embeddings(context.Background(), "nomic-embed", []string{"a"}); err != nil {
		t.Fatalf("post-reset embedding failed: %v", err)
	}
}

// A 200 with blank content (a thinking-only model spending its budget on reasoning) is NOT an upstream
// failure — callers tolerate it — so it must never accrue toward a trip.
func TestEmptyCompletionDoesNotTrip(t *testing.T) {
	srv := newCountingServer(200, `{"choices":[{"message":{"role":"assistant","content":"   "}}]}`)
	defer srv.Close()
	g, _, _ := guardedGateway(t, srv.URL, 2, time.Minute)
	for i := 0; i < 4; i++ {
		if _, err := g.Complete(context.Background(), "u", "primary", msgs()); err != nil {
			t.Fatalf("an empty 200 is not an error: %v", err)
		}
	}
	rec, _ := g.Breakers.For("primary").Snapshot(context.Background())
	if rec.State != breaker.StateClosed {
		t.Fatalf("state=%s want closed", rec.State)
	}
}

// errStore is a breaker.Store whose reads always fail — the "we lost breaker persistence" degraded mode.
type errStore struct{ err error }

func (e errStore) Load(context.Context, string) (breaker.Record, bool, error) {
	return breaker.Record{}, false, e.err
}
func (e errStore) Save(context.Context, breaker.Record) error     { return e.err }
func (e errStore) List(context.Context) ([]breaker.Record, error) { return nil, e.err }

// DEGRADED MODE, documented and asserted: a breaker whose own store is unreadable FAILS OPEN — the call
// proceeds, because losing breaker persistence must never block a healthy gateway — and the condition is
// REPORTED through Degraded, never silently swallowed.
func TestBreakerStoreOutageFailsOpenAndIsReported(t *testing.T) {
	srv := newCountingServer(200, okCompletion)
	defer srv.Close()
	os.Setenv("TG_TEST_LLM_KEY", "test-key")
	g := NewGateway(srv.URL, config.SecretRef("env:TG_TEST_LLM_KEY"))
	g.Breakers = NewBreakers(errStore{err: errors.New("db gone")})
	var reported []string
	g.Breakers.Degraded = func(name string, err error) { reported = append(reported, name+": "+err.Error()) }

	out, err := g.Complete(context.Background(), "u", "primary", msgs())
	if err != nil || out != "hello" {
		t.Fatalf("a degraded breaker store must FAIL OPEN; got (%q, %v)", out, err)
	}
	if srv.hits.Load() != 1 {
		t.Fatal("the call did not reach the gateway")
	}
	if len(reported) == 0 {
		t.Fatal("a breaker-store outage must be REPORTED, never silently swallowed")
	}
}

// An unwired registry is an honest no-op: identical behaviour to the pre-TG-221 gateway.
func TestNilBreakersIsTransparent(t *testing.T) {
	srv := newCountingServer(500, `{"error":{"message":"down"}}`)
	defer srv.Close()
	os.Setenv("TG_TEST_LLM_KEY", "test-key")
	g := NewGateway(srv.URL, config.SecretRef("env:TG_TEST_LLM_KEY"))
	for i := 0; i < 5; i++ {
		if _, err := g.Complete(context.Background(), "u", "primary", msgs()); errors.Is(err, breaker.ErrOpen) {
			t.Fatal("an unwired gateway must never short-circuit")
		}
	}
	if srv.hits.Load() != 5 {
		t.Fatalf("hits=%d want 5", srv.hits.Load())
	}
	if NewBreakers(nil) != nil {
		t.Fatal("NewBreakers(nil) must be a nil registry, never a half-armed one")
	}
}

// The slug rule is shared with the loadable LiteLLM module so a rung and a tier naming the same upstream
// coordinate on ONE row.
func TestBreakerNameIsMetricSafe(t *testing.T) {
	for in, want := range map[string]string{
		"primary": "model-primary", "z.ai/glm-4.6": "model-z-ai-glm-4-6", "": "model-",
	} {
		if got := BreakerName(in); got != want {
			t.Errorf("BreakerName(%q)=%q want %q", in, got, want)
		}
	}
}
