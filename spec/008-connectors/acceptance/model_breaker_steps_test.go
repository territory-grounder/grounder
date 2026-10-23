package acceptance

// spec/008 REQ-826/REQ-827 — the PRODUCTION model-path circuit breaker (TG-221, PORT-FIDELITY-AUDIT #24).
//
// These steps drive the REAL production pairing: adapters/model.NewGateway (the constructor cmd/worker uses)
// holding a real core/breaker over the real MemStore, against a real httptest server. The only injected
// seams are core/breaker's own documented ones (the in-memory Store twin and WithClock), so trip / half-open
// / reset are deterministic rather than wall-clock flaky. The load-bearing assertion is the SERVER HIT
// COUNT — a "breaker" that errors but still performs the round trip has bounded nothing.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"time"

	"github.com/cucumber/godog"

	model "github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/config"
)

func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerModelBreakerSteps)
}

// breakerStoreDown is a breaker.Store that cannot be read or written — the "we lost breaker persistence"
// degraded mode of REQ-827.
type breakerStoreDown struct{}

func (breakerStoreDown) Load(context.Context, string) (breaker.Record, bool, error) {
	return breaker.Record{}, false, errors.New("breaker store unreachable")
}
func (breakerStoreDown) Save(context.Context, breaker.Record) error {
	return errors.New("breaker store unreachable")
}
func (breakerStoreDown) List(context.Context) ([]breaker.Record, error) {
	return nil, errors.New("breaker store unreachable")
}

type modelBreakerWorld struct {
	srv      *httptest.Server
	hits     *atomic.Int64
	status   *atomic.Int64
	gw       *model.Gateway
	now      *time.Time
	degraded []string

	text      string
	err       error
	vecs      [][]float32
	embedErr  error
	hitsAtTop int64
}

func registerModelBreakerSteps(sc *godog.ScenarioContext) {
	w := &modelBreakerWorld{}
	_ = os.Setenv("TG_MODEL_BREAKER_ACCEPT_KEY", "sk-b")

	// arm builds the real gateway + real breaker over a real server. status is the code the server returns
	// until a step changes it.
	arm := func(status int, store breaker.Store) {
		hits := &atomic.Int64{}
		st := &atomic.Int64{}
		st.Store(int64(status))
		w.hits, w.status = hits, st
		w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			rw.Header().Set("Content-Type", "application/json")
			code := int(st.Load())
			rw.WriteHeader(code)
			if code == http.StatusOK {
				_, _ = rw.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"untrusted model text"}}],"data":[{"index":0,"embedding":[0.5]}]}`))
				return
			}
			_, _ = rw.Write([]byte(`{"error":{"message":"upstream down"}}`))
		}))
		now := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
		w.now = &now
		w.degraded = nil
		w.gw = model.NewGateway(w.srv.URL, config.SecretRef("env:TG_MODEL_BREAKER_ACCEPT_KEY"))
		w.gw.Breakers = model.NewBreakers(store,
			breaker.WithThreshold(3),
			breaker.WithCooldown(60*time.Second),
			breaker.WithHalfOpenSuccesses(1),
			breaker.WithClock(func() time.Time { return *w.now }))
		w.gw.Breakers.Degraded = func(name string, err error) {
			w.degraded = append(w.degraded, name+": "+err.Error())
		}
	}
	complete := func(tier string) (string, error) {
		return w.gw.Complete(context.Background(), "agent-1", tier, []model.Message{{Role: "user", Content: "hi"}})
	}
	// tripOpen drives the REAL failure path until the circuit is open.
	tripOpen := func(tier string) error {
		for i := 0; i < 3; i++ {
			if _, err := complete(tier); err == nil {
				return fmt.Errorf("call %d on %q should have failed upstream", i, tier)
			}
		}
		if _, err := complete(tier); !errors.Is(err, breaker.ErrOpen) {
			return fmt.Errorf("the circuit on %q did not open after the threshold: %v", tier, err)
		}
		return nil
	}

	sc.Step(`^the production model gateway is guarded by a named per-tier circuit breaker$`, func() error {
		arm(http.StatusServiceUnavailable, breaker.NewMemStore())
		return nil
	})
	sc.Step(`^a production model tier whose circuit has tripped open$`, func() error {
		arm(http.StatusServiceUnavailable, breaker.NewMemStore())
		return tripOpen("judge-tier")
	})
	sc.Step(`^the production model gateway is guarded by a breaker whose state store cannot be read$`, func() error {
		arm(http.StatusOK, breakerStoreDown{})
		return nil
	})

	sc.Step(`^one tier fails upstream up to its trip threshold$`, func() error { return tripOpen("judge-tier") })
	sc.Step(`^the cooldown elapses and the upstream has recovered$`, func() error {
		*w.now = w.now.Add(90 * time.Second)
		w.status.Store(int64(http.StatusOK))
		w.hitsAtTop = w.hits.Load()
		w.text, w.err = complete("judge-tier")
		return nil
	})
	sc.Step(`^a completion and an embedding are requested on that tier$`, func() error {
		w.hitsAtTop = w.hits.Load()
		w.text, w.err = complete("judge-tier")
		w.vecs, w.embedErr = w.gw.Embeddings(context.Background(), "judge-tier", []string{"a"})
		return nil
	})
	sc.Step(`^a completion is requested$`, func() error {
		w.hitsAtTop = w.hits.Load()
		w.text, w.err = complete("primary")
		return nil
	})

	sc.Step(`^the next call on that tier is short-circuited with no round trip while a healthy tier still serves$`, func() error {
		before := w.hits.Load()
		if _, err := complete("judge-tier"); !errors.Is(err, breaker.ErrOpen) {
			return fmt.Errorf("the tripped tier must be short-circuited, got %v", err)
		}
		if w.hits.Load() != before {
			return fmt.Errorf("the short-circuited call still reached the gateway — nothing was bounded")
		}
		// Per-tier isolation: a healthy tier is unaffected by another tier's trip.
		w.status.Store(int64(http.StatusOK))
		if _, err := complete("agent-tier"); err != nil {
			return fmt.Errorf("a healthy tier must keep serving while another tier is open: %w", err)
		}
		return nil
	})
	sc.Step(`^exactly one probe reaches the gateway and its success closes the circuit$`, func() error {
		if w.err != nil {
			return fmt.Errorf("the half-open probe must be admitted and served: %w", w.err)
		}
		if got := w.hits.Load() - w.hitsAtTop; got != 1 {
			return fmt.Errorf("the cooldown must admit exactly ONE probe, %d round trips were made", got)
		}
		rec, rerr := w.gw.Breakers.For("judge-tier").Snapshot(context.Background())
		if rerr != nil || rec.State != breaker.StateClosed {
			return fmt.Errorf("a successful probe must CLOSE the circuit, state=%s err=%v", rec.State, rerr)
		}
		return nil
	})
	sc.Step(`^each returns a typed breaker-open error carrying no text and no vectors$`, func() error {
		if !errors.Is(w.err, breaker.ErrOpen) || !errors.Is(w.embedErr, breaker.ErrOpen) {
			return fmt.Errorf("both calls must fail with the breaker cause, got completion=%v embedding=%v", w.err, w.embedErr)
		}
		var me *model.ModelError
		if !errors.As(w.err, &me) || me.Class != model.ClassBreakerOpen {
			return fmt.Errorf("the refusal must be a typed %s error, got %#v", model.ClassBreakerOpen, w.err)
		}
		if w.text != "" {
			return fmt.Errorf("a short-circuited completion returned text %q — an empty result with a nil error is the exact shape that becomes an empty scorecard", w.text)
		}
		if w.vecs != nil {
			return fmt.Errorf("a short-circuited embedding returned %d vectors; it must fabricate none", len(w.vecs))
		}
		if w.hits.Load() != w.hitsAtTop {
			return fmt.Errorf("a short-circuited call reached the gateway")
		}
		return nil
	})
	sc.Step(`^the call is served and the degraded breaker state is reported$`, func() error {
		if w.err != nil || w.text == "" {
			return fmt.Errorf("an unreadable breaker store must FAIL OPEN (the call is served), got (%q, %v)", w.text, w.err)
		}
		if w.hits.Load() != w.hitsAtTop+1 {
			return fmt.Errorf("the call did not reach the gateway")
		}
		if len(w.degraded) == 0 {
			return fmt.Errorf("a breaker-store outage must be REPORTED, never silently swallowed")
		}
		return nil
	})
}
