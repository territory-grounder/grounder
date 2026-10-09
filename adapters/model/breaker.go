package model

// The PRODUCTION model path's bounded-failure guard (PORT-FIDELITY-AUDIT finding #24, TG-221).
//
// CONSTITUTION.md:130 promises "named, observable circuit breakers with persisted state" on the
// model-gateway / judge / RAG calls. core/breaker built that machine and two lanes the finding did not name
// (the safety mutation breaker, the cost breaker) consume it — but every PRODUCTION model call goes through
// *Gateway, which had no breaker at all. The guarded per-rung module (modules/model/litellm) has no
// production constructor, so it was an unexercised copy of the control. During a gateway flap the judge cron
// and the skill generator therefore retried an unavailable upstream on EVERY call, unbounded, where the
// predecessor's ladder short-circuited to a fallback after three consecutive failures.
//
// The guard lives HERE, at Gateway.Complete/Gateway.Embeddings, for the same reason CallObserver does (see
// model.go): every production caller — agent loop, judge cron, skill generator, offline gate, calibrator,
// the RAG embedder — shares ONE *Gateway. A guard at this chokepoint cannot be bypassed by adding a caller;
// a per-caller decorator can.
//
// Provenance: [F] scripts/lib/circuit_breaker.py + the four named RAG breakers (rag_embed_ollama,
// rag_synth_haiku, …) — one breaker per named DEPENDENCY, consulted IMPERATIVELY at the call site
// (allow → call → record_success/record_failure) rather than as a decorator, which is what lets each call
// site keep its own typed failure contract.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/territory-grounder/grounder/core/breaker"
)

// ClassBreakerOpen is the bounded ModelError class (and the metric outcome label) for a call the breaker
// short-circuited. It is deliberately DISTINCT from every upstream-failure class so an operator reading
// tg_model_calls_total{outcome="breaker_open"} sees "TG refused to call" rather than "the provider failed".
const ClassBreakerOpen = "breaker_open"

// Breakers is the per-model-tier circuit-breaker registry a Gateway consults on every completion and every
// embedding call. One breaker per model NAME (mirroring the predecessor's one-breaker-per-named-dependency
// rule): a judge tier whose ladder is exhausted trips its own circuit without short-circuiting the agent
// tier that is still healthy, and the RAG embedding model gets the named, persisted breaker the audit found
// missing (its lexical fallback was bounded per-call only).
//
// Breakers holds no state of its own — core/breaker's Store is authoritative — so sibling workers sharing
// the pgx store coordinate on one row per model name, exactly as the mutation breaker does.
type Breakers struct {
	store breaker.Store
	opts  []breaker.Option

	// Degraded is called when the breaker's OWN store cannot be read or written. The guard then fails OPEN
	// (the call proceeds): losing breaker persistence must never block a healthy gateway. It is reported, never
	// swallowed — an unobservable guard that silently stops guarding is the dead-capability failure this whole
	// port exists to avoid. Optional; nil ⇒ no report (behaviour otherwise identical).
	Degraded func(name string, err error)

	mu     sync.Mutex
	byName map[string]*breaker.Breaker
}

// NewBreakers builds the registry over a shared breaker store. opts (threshold / cooldown / half-open
// successes / clock) are applied to EVERY breaker the registry creates, so one operator setting governs the
// whole model plane. A nil store yields a nil registry — an unwired guard is an honest no-op, never a
// silently-half-armed one.
func NewBreakers(store breaker.Store, opts ...breaker.Option) *Breakers {
	if store == nil {
		return nil
	}
	return &Breakers{store: store, opts: opts, byName: map[string]*breaker.Breaker{}}
}

// BreakerName maps a model name to the stable, metric-safe breaker slug ("primary" → "model-primary").
// Exported so the loadable LiteLLM module names its per-rung breakers by the SAME rule: a rung and a tier
// that denote the same upstream must share one row, never two half-counting ones.
func BreakerName(modelName string) string {
	var sb strings.Builder
	sb.WriteString("model-")
	for _, r := range modelName {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteRune('-')
		}
	}
	return sb.String()
}

// For returns the breaker guarding modelName, creating it lazily over the shared store. A nil registry
// returns a nil breaker (the guard is simply absent).
func (bs *Breakers) For(modelName string) *breaker.Breaker {
	if bs == nil {
		return nil
	}
	name := BreakerName(modelName)
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if b, ok := bs.byName[name]; ok {
		return b
	}
	b, err := breaker.New(name, bs.store, bs.opts...)
	if err != nil {
		// An unnameable model (empty string) still gets a guard rather than none — a call TG cannot name is
		// exactly the one that should not run unbounded.
		if b, err = breaker.New("model-unnamed", bs.store, bs.opts...); err != nil {
			return nil
		}
	}
	bs.byName[name] = b
	return b
}

// allow reports whether the call may proceed. A store error fails OPEN (allow) and is reported through
// Degraded — the read/advisory-lane direction core/breaker documents.
func (bs *Breakers) allow(ctx context.Context, b *breaker.Breaker) bool {
	if bs == nil || b == nil {
		return true
	}
	ok, err := b.Allow(ctx)
	if err != nil {
		bs.degraded(b.Name(), err)
		return true
	}
	return ok
}

// record folds one completed call into the breaker. tripWorthy decides what counts as an UPSTREAM failure;
// a store error is reported and otherwise ignored (the call already happened).
func (bs *Breakers) record(ctx context.Context, b *breaker.Breaker, class string, callErr error) {
	if bs == nil || b == nil {
		return
	}
	var err error
	if tripWorthy(class, callErr) {
		err = b.RecordFailure(ctx)
	} else {
		err = b.RecordSuccess(ctx)
	}
	if err != nil {
		bs.degraded(b.Name(), err)
	}
}

func (bs *Breakers) degraded(name string, err error) {
	if bs != nil && bs.Degraded != nil {
		bs.Degraded(name, err)
	}
}

// tripWorthy reports whether a completed call counts against the breaker. A success never does. Every
// failure class does EXCEPT bad_request: a 400 means the request TG SENT was malformed, so it is a defect in
// this process, not an upstream outage — tripping on it would let one over-long prompt short-circuit every
// other component's model calls, a self-inflicted outage. auth/timeout/transport/provider_error and an empty
// choices array are all upstream conditions a retry cannot fix on the next call, so they accrue. rate_limit
// also accrues, but note it only REACHES this function as a final outcome after CompleteWithUsage's bounded
// backoff already failed to ride it out (a transient 429 is retried-then-succeeds and records "ok", never
// rate_limit, TG-534) — so a recorded rate_limit is a genuine sustained cap, exactly what the breaker should
// see, and the retry loop does not re-arm the storm the breaker prevents.
func tripWorthy(class string, callErr error) bool {
	if callErr == nil {
		return false // includes the 200-with-blank-content "empty" case: not a failure, callers tolerate it
	}
	return class != "bad_request"
}

// breakerFor resolves the guard for one call: nil when no registry is wired.
func (g *Gateway) breakerFor(modelName string) *breaker.Breaker {
	if g == nil || g.Breakers == nil {
		return nil
	}
	return g.Breakers.For(modelName)
}

// errBreakerOpen is the typed, LOUD refusal returned when the circuit is open.
//
// DEGRADED-MODE CONTRACT (the whole point of finding #24): a trip returns an ERROR and an empty string is
// never returned with a nil error, so
//
//   - the ACTUATION-relevant path fails CLOSED — the agent loop's Complete error stops the ReAct loop with
//     OutcomeStop, so no proposal is produced and there is nothing for the interceptor to execute; and
//   - the JUDGING / EVAL path fails LOUD — the judge activity surfaces the error rather than writing a
//     fabricated or empty scorecard, and an unjudged session is recorded as a visible skip, never a silent
//     one.
//
// It wraps breaker.ErrOpen so a caller can distinguish "TG refused to call" (errors.Is(err, breaker.ErrOpen))
// from "the provider failed", which is what lets the judge escalate a systemic trip instead of retrying it.
func errBreakerOpen(breakerName, modelName string) *BreakerRefusal {
	return &BreakerRefusal{ModelError: &ModelError{
		Class: ClassBreakerOpen,
		Message: fmt.Sprintf("circuit %q OPEN for model %q — call short-circuited to a LOUD error "+
			"(never a silent empty result); it re-arms on the half-open probe after the cooldown", breakerName, modelName),
		wrapped: breaker.ErrOpen,
	}}
}

// BreakerRefusal is a DELIBERATE REFUSAL, and it exists so Temporal can tell it apart from a failure.
//
// ★ WHY A SEPARATE GO TYPE (TG-400). Temporal matches RetryPolicy.NonRetryableErrorTypes on the
// ApplicationError TYPE, and it derives that type from the Go type name of the returned error
// (sdk internal/error.go getErrType: dereference pointers, return reflect Type.Name()). Every model
// failure — rate_limit, provider_error and a breaker refusal alike — was a *ModelError, so all three
// serialised as "ModelError" and no list could single one out.
//
// The cost, measured across all 159 session_triage rows for 2026-08-06 with every Temporal history
// retrieved: 135 of 159 investigations ran attempt 2, and for 78 of them attempt 1 was breaker_open and
// attempt 2 was breaker_open again — the retry of a refusal. With TG_MODEL_BREAKER_COOLDOWN=60s and a 1s
// initial interval, attempt 2 lands inside the open window WITH CERTAINTY. 134 of 135 ended
// RETRY_STATE_MAXIMUM_ATTEMPTS_REACHED. Median cost 3.04s against ~0.5s had it failed on attempt 1.
//
// Retrying a refusal is not merely wasteful — it is incoherent. The breaker is TG deciding not to call;
// a retry asks the same question inside the window the decision created.
//
// It EMBEDS *ModelError rather than replacing it, so every existing caller keeps working:
// errors.As(err, &me) still recovers the *ModelError (the metrics layer classifies on .Class), and
// errors.Is(err, breaker.ErrOpen) still reaches through to the wrapped sentinel.
type BreakerRefusal struct{ *ModelError }

// Unwrap returns the embedded *ModelError, and it is NOT redundant — this method is the whole reason the
// embedding is compatible.
//
// Without it, the promoted Unwrap is *ModelError's, which returns the wrapped sentinel directly. The error
// chain then reads BreakerRefusal -> breaker.ErrOpen and SKIPS the *ModelError in between, so
// errors.As(err, &me) fails and every refusal loses its Class — which would have reclassified all of them
// as outcome="other", the exact defect TG-369 was filed for. Caught by
// TestExistingCallersStillSeeAModelError, which failed on the first implementation.
func (e *BreakerRefusal) Unwrap() error { return e.ModelError }

// NewBreakerRefusalForTest builds the refusal an open circuit returns. Exported for oracles in other
// packages (temporal/runner asserts what Temporal does with its TYPE, which cannot be tested from inside
// this package because the retry policy lives there). Production builds it through errBreakerOpen.
func NewBreakerRefusalForTest(breakerName, modelName string) *BreakerRefusal {
	return errBreakerOpen(breakerName, modelName)
}

// NewModelErrorForTest builds an ordinary typed model failure — the CONTRAST case for the oracle above,
// which asserts a refusal and a failure do not serialise under the same Temporal type.
func NewModelErrorForTest(class, msg string) *ModelError {
	return &ModelError{Class: class, Message: msg}
}
