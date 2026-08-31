// Package model is the client for the bundled LiteLLM model-gateway.
//
// Provenance: [corrections] native agent loop over a bundled LiteLLM gateway — NO Claude Code CLI ·
// [R] paradigm-rules 3, 6 (model-and-vendor-agnostic; local-first is a mode, not the mission),
// "centralized model routing", P0-6.
//
// TG never talks to a provider directly and never launches a coding-CLI subprocess. It calls ONE
// OpenAI-compatible endpoint (the LiteLLM gateway, in deploy/docker-compose.yml). The auto-fallback
// ladder (z.ai → DeepSeek → Mistral → …), retries, rate-limit handling, and org budgets/quotas live
// as LiteLLM config (deploy/litellm-config.yaml). The Go side only selects a model name; LiteLLM maps
// it to the ladder. Provider keys resolve through core/config secret references only (INV-13).
//
// WHERE THE component→model RESOLVER OF RECORD LIVES, AND WHY THERE IS NO GO STRUCT FOR IT (TG-298).
// This package used to export a `Resolver` — a map[component]modelName with a Default fallback. It was
// constructed at exactly ONE site in the whole tree (a spec acceptance test) and reached no composition
// root, so for its entire life it routed nothing. It was deleted rather than wired, and the reasoning
// belongs here because "keep the single component→provider/model source-of-truth resolver" is an ADR
// decision (docs/adr/0004) that a future reader will find and try to honour by re-adding the struct:
//
//   - The resolver of record already EXISTS and is live: deploy/litellm-config.yaml's model_list maps
//     every tier name the Go side selects ("primary", "fast", "embed-nomic", the TG-204 arm aliases) to a
//     provider + fallback chain, is operator-editable without a rebuild, and is CI-guarded (the
//     `litellm-config` job validates that every router fallback reference resolves). A Go map of the same
//     mapping would be a SECOND source of truth for one decision — the opposite of what the ADR asks for.
//   - The production selector it would have replaced is strictly MORE capable, not less. temporal/runner
//     activities.go investigateTierFor is per-INCIDENT: it reads the computed execclass and routes a deep
//     investigation UP to the reasoner (the MECH-402 safety floor), with the TG-204 eval-arm override held
//     behind a second gate. A static component→model map cannot express either; wiring it over the floor
//     would have been a safety regression that looked like centralization.
//   - Its miss path was silent. Model("unknown-component") returned Default with no error and no log —
//     this repo's most-repeated bug shape (a lookup that stops matching and says nothing).
//
// So: to change which model a component uses, edit deploy/litellm-config.yaml. To change WHICH TIER a
// component asks for, edit the tier constant at its call site — there are a handful and they are greppable.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/territory-grounder/grounder/core/config"
)

// Message is one OpenAI-style chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// CallObserver receives one record per completed gateway call — the model tier, a classified outcome, the
// HTTP status (0 if the call never reached the server), the wall-clock seconds, and a bounded error detail
// ("" on success). It is the observability seam at the gateway boundary: EVERY caller (agent loop, offline
// gate, judge cron, skill generator, calibrator) shares one *Gateway, so recording here — rather than in a
// per-caller decorator — guarantees no model call is invisible. Implementations meter + log; they must be
// side-effect-only, nil-tolerant, and MUST NOT block or fail the inference (a slow/broken observer must not
// break a completion). The detail string is provider error prose, never a secret (INV-13).
type CallObserver interface {
	// caller is the requesting subsystem's tag (the Complete `user` argument — "hyde", "runner:<ref>", …;
	// "" on the embed lane, which carries no caller). It exists so a failing call NAMES its origin in the
	// log (TG-530: ~600 doomed empty-model calls/day were anonymous for days because only the tier was
	// logged). It is UNBOUNDED text: log it, never make it a metric label.
	ObserveCall(tier, caller, outcome string, statusCode int, seconds float64, detail string)
}

// UsageObserver is the OPTIONAL second half of CallObserver: the REPORTED token accounting of one billed
// completion. It is a separate interface, discovered by type assertion on Gateway.Obs, so every existing
// CallObserver keeps compiling and keeps behaving identically — an observer that does not implement it
// simply receives no usage.
//
// It fires only for calls that actually reached the provider and returned a body it was willing to bill
// (outcome ok/empty). A transport failure, an auth rejection or an open breaker is NOT a missing usage
// block — nothing was spent — and reporting those here would make the "we had to guess" signal read high
// for a reason that has nothing to do with token accounting.
type UsageObserver interface {
	ObserveUsage(tier string, u Usage)
}

// Usage is the token accounting the PROVIDER reported for ONE completion — the OpenAI-compatible `usage`
// block LiteLLM returns beside `choices`.
//
// TG-44, and Measured is the load-bearing field. Before this type existed the gateway decoded the response
// into a struct with no `usage` member, so encoding/json silently dropped the block on EVERY call, and
// every token number in the system (the cost breaker's accrual, tg_agent_tokens_approx_total) was a
// chars/4 GUESS wearing a confident label. Measured against the live gateway on 2026-08-04:
//
//	prompt 47 chars   → guess    12 tokens · REPORTED   166 (13.8x low)
//	prompt 3409 chars → guess   852 tokens · REPORTED  1607 (1.9x low)
//
// The error is not a constant factor — it varies with prompt size because per-message and system-prompt
// overhead is invisible to a character count — so it cannot be corrected with a multiplier. The direction
// matters: the guess is LOW, so a daily budget of $X let TG spend roughly $2X before the cost breaker
// tripped. That is why Measured is carried rather than smoothed away: an estimate must never be billed or
// exported as if it were a measurement, because a wrong number gets trusted while a missing one gets asked
// about.
//
// Measured is FALSE when the response carried no usage block AND when it carried an empty one (all zeros,
// which some proxies emit for a streamed response with no stream_options). A present-but-zero block treated
// as measured would bill every call at $0 forever — a quieter and worse lie than the estimate it replaced.
type Usage struct {
	PromptTokens     int // tokens the provider charged for the request
	CompletionTokens int // tokens the provider charged for the response
	TotalTokens      int // the provider's total; derived as prompt+completion when the field is absent
	// Measured is true ONLY when the provider actually reported a non-empty usage block. False means TG has
	// no measurement for this call and any token figure downstream is an estimate that must say so.
	Measured bool
}

// Gateway is a minimal client for the LiteLLM OpenAI-compatible endpoint.
type Gateway struct {
	BaseURL   string           // e.g. http://litellm:4000
	APIKeyRef config.SecretRef // e.g. env:LITELLM_MASTER_KEY
	HTTP      *http.Client
	Obs       CallObserver // optional; nil ⇒ no observation (behaviour is otherwise identical)
	// Breakers is the per-model-tier bounded-failure guard (breaker.go, TG-221). Nil ⇒ unguarded, which is
	// the pre-TG-221 behaviour and is what a CI/in-memory boot without a breaker store gets. When wired,
	// EVERY completion and embedding traverses it: a persistently-failing upstream is short-circuited to a
	// LOUD typed error instead of being retried on every call. Set at the composition root; the Gateway
	// pointer is shared by every caller, so a late assignment guards the callers constructed before it.
	Breakers *Breakers
	// Concurrency, when non-nil, bounds the completions IN FLIGHT through this gateway at once (TG-384). It is
	// the chokepoint the pve03 cascade needed: 157 alerts became 157 simultaneous investigations that tripped
	// the model-primary breaker in SIX seconds against an 8-slot sidecar, and 133 sessions died with an empty
	// diagnosis no retry could recover — a self-inflicted DoS on TG's own brain, triggered by correctly
	// recognising a cascade. It sits at the SAME chokepoint the CallObserver and Breakers occupy, so a new
	// caller cannot bypass it. Overflow WAITS on the caller's context (defer-release, never drop): a queued
	// investigate burns no timeout because ScheduleToStartTimeout is unset, so parking is safe and honours
	// INV-08's under-triage-over-drop preference. Nil ⇒ UNBOUNDED — the pre-TG-384 behaviour, and what a
	// CI/in-memory boot with no env gets. The composition root arms it from TG_MODEL_MAX_CONCURRENCY, which the
	// deployed compose ships at 8 by DEFAULT (deploy/docker-compose.yml, TG-384): prod runs BOUNDED without any
	// operator action, and only an explicit 0 disables it.
	Concurrency *semaphore.Weighted
	// MaxTokens is the per-completion OUTPUT cap sent as `max_tokens` on every chat request (TG-48). The
	// model client set no token budget at all, so a runaway high-severity investigation — the same shape as
	// the pve03 cascade's 157 simultaneous sessions — could spend an unbounded number of output tokens per
	// call, and the cost breaker only trips AFTER the daily budget is already blown. A ceiling here bounds the
	// worst single call up front. 0 ⇒ omitted from the request ⇒ the model/gateway default (today's
	// behaviour), so this ships INERT; a composition root arms it from TG_MODEL_MAX_TOKENS. Set high enough
	// not to truncate a legitimate long completion — it is a runaway ceiling, not a normal-operation limit.
	// A context-carried per-class cap (WithOutputTokenCap, TG-42) may TIGHTEN it per call, never raise it.
	MaxTokens int
	// SessionTokenBudget bounds the CUMULATIVE output (completion) tokens ONE session may spend across all of
	// its completions (TG-48). MaxTokens caps a single call; this caps the whole investigation. A session that
	// keeps looping — the pve03 cascade shape — can make many individually-in-budget calls and still spend
	// without bound, and the daily cost breaker only trips AFTER the global budget is blown, with no per-session
	// attribution. It is keyed on the per-session `user` ("runner:"+external_ref, activities.go), so it is
	// genuinely per-investigation. Once a session's cumulative completion tokens reach the ceiling its NEXT
	// completion is refused with a typed budget error — fail-SAFE (an INCOMPLETE investigation, never an empty
	// one). 0 ⇒ UNBOUNDED (today's behaviour), so this ships INERT; a composition root arms it from
	// TG_MODEL_SESSION_TOKEN_BUDGET. Set high enough not to truncate a legitimate deep investigation — a runaway
	// ceiling, not a normal-operation limit.
	SessionTokenBudget int
	sessionMu          sync.Mutex     // guards sessionTokens
	sessionTokens      map[string]int // per-session (user) cumulative completion tokens; bounded by budgetSessionCap
	// RateLimitRetries / RateLimitBackoffBase tune the 429 backoff-retry (TG-534). A 429 is cooperative
	// backpressure ("slow down"), not a model failure — so the gateway retries it with backoff before
	// surfacing it, which is what lets the eval change gate's concurrent-session burst survive the provider's
	// per-minute rate limit instead of failing whole arms. RateLimitRetries: 0 ⇒ the default budget
	// (defaultRateLimitRetries); a NEGATIVE value ⇒ retry disabled (a single attempt) — the explicit opt-out
	// for tests that assert one observe per call. RateLimitBackoffBase: 0 ⇒ the default; tests set a tiny base
	// so the backoff sleeps are negligible.
	RateLimitRetries     int
	RateLimitBackoffBase time.Duration
}

const (
	// defaultRateLimitRetries bounds how many times a 429 is retried before it is surfaced. Small: the point
	// is to ride out a brief provider throttle, not to hammer a sustained outage (which the breaker handles).
	defaultRateLimitRetries = 4
	// defaultRateLimitBackoffBase is the first exponential backoff; each attempt doubles it (base * 2^attempt).
	defaultRateLimitBackoffBase = 500 * time.Millisecond
	// rateLimitBackoffMax caps one exponential step so the doubling cannot produce a multi-minute stall at a
	// high attempt. The context deadline governs the TOTAL wait (sleepCtx wakes when it ends); this bounds one step.
	rateLimitBackoffMax = 8 * time.Second
	// retryAfterCap bounds an honored Retry-After so an absurd provider value cannot park a deadline-less
	// caller (context.Background()) indefinitely. Real provider values (Azure sends seconds-to-tens-of-seconds)
	// pass through untouched; the ctx deadline still governs the total for callers that set one.
	retryAfterCap = 20 * time.Second
)

// budgetSessionCap bounds the per-session token ledger so a long-lived worker cannot leak one map entry per
// investigation forever. On overflow the ledger is RESET, which fails OPEN (budgets restart from zero) — the
// safe direction for a guardrail: it can only ever UNDER-enforce, never wrongly block a session.
const budgetSessionCap = 50000

// outputCapKeyType keys the context-carried per-call output-token cap (TG-42). Unexported struct key, so
// no other package can collide with it and the ONLY writer is WithOutputTokenCap below.
type outputCapKeyType struct{}

// WithOutputTokenCap returns a context whose chat completions request AT MOST n output tokens — the
// per-execution-class budget seam (TG-42). It rides the context rather than the Completer signature so
// the per-class decision (made in the runner, where the class is known) reaches the gateway without
// changing every fake and decorator between them.
//
// The gateway honors it TIGHTEN-ONLY against Gateway.MaxTokens (the TG_MODEL_MAX_TOKENS runaway ceiling,
// TG-48): the effective max_tokens is the SMALLER of the two, so a context cap can bound a cheap class
// below the ceiling but can never raise or disarm the ceiling itself — a compromised or misconfigured
// caller cannot widen the budget through this seam, only narrow it. n <= 0 stores nothing (inert, the
// TG-48 convention: an unset knob changes no behavior).
func WithOutputTokenCap(ctx context.Context, n int) context.Context {
	if n <= 0 {
		return ctx
	}
	return context.WithValue(ctx, outputCapKeyType{}, n)
}

// OutputTokenCapFromContext reports the per-call output cap carried on ctx, if any. Exported so the
// runner's oracles can assert the cap actually rides the session context — a seam that cannot be read
// cannot be falsified.
func OutputTokenCapFromContext(ctx context.Context) (int, bool) {
	n, ok := ctx.Value(outputCapKeyType{}).(int)
	if !ok || n <= 0 {
		return 0, false
	}
	return n, true
}

// effectiveMaxTokens resolves the max_tokens for ONE chat request: the gateway's class-blind ceiling,
// tightened by any context-carried per-class cap. TIGHTEN-ONLY: with both set the smaller wins; a context
// cap alone applies as-is; neither ⇒ 0 ⇒ the field is omitted (the model/gateway default — the pre-TG-48
// behavior). Embeddings have no output cap and never consult this.
func (g *Gateway) effectiveMaxTokens(ctx context.Context) int {
	m := g.MaxTokens
	if n, ok := OutputTokenCapFromContext(ctx); ok && (m == 0 || n < m) {
		m = n
	}
	return m
}

// ErrSessionTokenBudget is the sentinel every per-session-token-budget refusal matches under errors.Is, so a
// caller can tell "this session spent its token budget" from any other model error.
var ErrSessionTokenBudget = errors.New("model: session output-token budget reached")

// SetSessionTokenBudget arms the per-session cumulative-output-token ceiling (TG-48). n <= 0 leaves the gateway
// UNBOUNDED (the default), so an unset TG_MODEL_SESSION_TOKEN_BUDGET changes nothing. Called at the composition
// root beside the other late guard assignments; the Gateway pointer is shared, so this one call guards every
// caller.
func (g *Gateway) SetSessionTokenBudget(n int) {
	if n > 0 {
		g.sessionMu.Lock()
		g.SessionTokenBudget = n
		if g.sessionTokens == nil {
			g.sessionTokens = map[string]int{}
		}
		g.sessionMu.Unlock()
	}
}

// sessionOverBudget reports whether this session has already reached its cumulative-token ceiling.
func (g *Gateway) sessionOverBudget(user string) bool {
	if g.SessionTokenBudget <= 0 || user == "" {
		return false
	}
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	return g.sessionTokens[user] >= g.SessionTokenBudget
}

// addSessionTokens accrues a completed call's output tokens to its session's running total (bounded ledger).
func (g *Gateway) addSessionTokens(user string, completionTokens int) {
	if g.SessionTokenBudget <= 0 || user == "" || completionTokens <= 0 {
		return
	}
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	if g.sessionTokens == nil {
		g.sessionTokens = map[string]int{}
	}
	if len(g.sessionTokens) >= budgetSessionCap {
		g.sessionTokens = map[string]int{} // bounded, fail-open (see budgetSessionCap)
	}
	g.sessionTokens[user] += completionTokens
}

// SetMaxConcurrency bounds in-flight completions to n (TG-384). n <= 0 leaves the gateway UNBOUNDED, so an
// explicit 0 disables the bound; the deployed compose ships TG_MODEL_MAX_CONCURRENCY=8 by default, so prod
// arms it without operator action (only a CI/in-memory boot with no env is unbounded). Called at the
// composition root beside the late Obs/Breakers assignment; the Gateway pointer is shared, so this one call
// guards every caller.
func (g *Gateway) SetMaxConcurrency(n int) {
	if n > 0 {
		g.Concurrency = semaphore.NewWeighted(int64(n))
	}
}

// observe reports one completed call to the optional observer. Centralized so every call path (completion,
// embedding, breaker-refused) reports through the same seam and none can be added without one.
func (g *Gateway) observe(tier, caller, outcome string, status int, seconds float64, detail string) {
	if g.Obs != nil {
		g.Obs.ObserveCall(tier, caller, outcome, status, seconds, detail)
	}
}

// observeUsage reports the provider's token accounting to the observer when it implements the optional
// UsageObserver half. Guarded by the same "must not break inference" contract as observe: an observer that
// does not implement it is skipped silently, because usage is an OBSERVATION, never a precondition.
func (g *Gateway) observeUsage(tier string, u Usage) {
	if g.Obs == nil {
		return
	}
	if uo, ok := g.Obs.(UsageObserver); ok {
		uo.ObserveUsage(tier, u)
	}
}

// ModelError is the typed error the gateway returns on a failed completion. It carries the HTTP status and
// a CLASS (rate_limit | timeout | bad_request | auth | provider_error | transport | empty) so a caller can
// distinguish a transient failure (worth a retry/fallback) from a permanent one, and so the observability
// layer can label the outcome. errors.As(err, &me) recovers it; Unwrap exposes the underlying transport
// error (e.g. context.DeadlineExceeded) for errors.Is.
type ModelError struct {
	Status  int    // HTTP status code, or 0 when the call never reached the server (transport/timeout)
	Class   string // classified outcome (see the enum above) — a bounded value, also the metric label
	Message string // gateway/provider error prose (never a secret)
	// RetryAfter is the provider's requested wait before a retry, parsed from a 429's Retry-After header
	// (0 when absent or on any non-429). The rate-limit backoff loop honors it before falling back to
	// exponential backoff (TG-534).
	RetryAfter time.Duration
	wrapped    error // the underlying transport error, if any
}

// Error renders the typed model error.
func (e *ModelError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("model gateway %s (status %d): %s", e.Class, e.Status, e.Message)
	}
	return fmt.Sprintf("model gateway %s: %s", e.Class, e.Message)
}

// Unwrap exposes the underlying transport error (nil for a server-side error) so errors.Is works.
func (e *ModelError) Unwrap() error { return e.wrapped }

// classifyStatus maps an HTTP status to a bounded outcome class.
func classifyStatus(status int) string {
	switch {
	case status == 429:
		return "rate_limit"
	case status == 400:
		return "bad_request"
	case status == 401 || status == 403:
		return "auth"
	case status >= 500:
		return "provider_error"
	default:
		return "other"
	}
}

// classifyTransport distinguishes a timeout/deadline from a generic transport failure (a DNS/dial/reset).
func classifyTransport(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "timeout") || strings.Contains(s, "deadline") || strings.Contains(s, "context canceled") {
		return "timeout"
	}
	return "transport"
}

// parseRetryAfter reads the delta-seconds form of a Retry-After header ("120"). The HTTP-date form is not
// honored: providers here send seconds, and a date interpreted against a skewed local clock would mislead
// the backoff more than an empty value (which falls back to exponential). A non-numeric or negative value
// yields 0 (⇒ fall back to exponential). The returned duration is bounded by the caller (rateLimitBackoff).
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	secs, err := strconv.Atoi(h)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// retryAfterOf extracts a *ModelError's RetryAfter, if the error is one. 0 otherwise.
func retryAfterOf(err error) time.Duration {
	var me *ModelError
	if errors.As(err, &me) {
		return me.RetryAfter
	}
	return 0
}

// rateLimitBackoff returns how long to wait before retrying a 429 on the given zero-based attempt. It prefers
// the provider's Retry-After (capped, so an absurd value cannot park a deadline-less caller) and otherwise
// uses exponential backoff (base * 2^attempt, capped per-step). Either way it adds up to 25% jitter to
// de-synchronize concurrent sessions that hit the same throttle at the same instant — the eval gate's burst
// is exactly that thundering herd, and a fixed backoff would just re-collide them a beat later.
func (g *Gateway) rateLimitBackoff(attempt int, retryAfter time.Duration) time.Duration {
	base := g.RateLimitBackoffBase
	if base <= 0 {
		base = defaultRateLimitBackoffBase
	}
	var d time.Duration
	switch {
	case retryAfter > 0:
		d = min(retryAfter, retryAfterCap)
	default:
		// base << attempt, guarding the shift: a large attempt would overflow to a non-positive value, which
		// the cap check below catches (d <= 0 ⇒ clamp to the max) so an overflow can never yield a zero wait.
		if attempt > 30 {
			d = rateLimitBackoffMax
		} else {
			d = base << uint(attempt)
		}
		if d <= 0 || d > rateLimitBackoffMax {
			d = rateLimitBackoffMax
		}
	}
	// Jitter in [0, d/4]. rand/v2's top-level source is concurrency-safe and auto-seeded; the exact value
	// need not be reproducible — its only job is to spread the herd.
	if d > 0 {
		d += time.Duration(rand.Int64N(int64(d)/4 + 1))
	}
	return d
}

// sleepCtx waits for d or until ctx ends, whichever comes first. It returns ctx.Err() if the context ended
// during the wait (so the caller stops retrying and surfaces the real reason), nil if the full delay elapsed.
// A non-positive delay is a no-op that still honors an already-cancelled context.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NewGateway constructs a gateway client with a sane default timeout.
func NewGateway(baseURL string, keyRef config.SecretRef) *Gateway {
	return &Gateway{BaseURL: baseURL, APIKeyRef: keyRef, HTTP: &http.Client{Timeout: 120 * time.Second}}
}

type chatRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	User      string    `json:"user,omitempty"`       // per-user/agent budget attribution at the gateway (org-global quota)
	MaxTokens int       `json:"max_tokens,omitempty"` // per-completion output cap (TG-48); 0 ⇒ omitted ⇒ the model/gateway default
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	// Usage is the provider's token accounting. It was ABSENT from this struct until TG-44, which is the
	// whole defect: the live gateway returns it on every completion (verified 2026-08-04 against
	// dc1tg01's LiteLLM — top-level keys choices/created/id/model/object/USAGE) and encoding/json
	// dropped it on the floor because there was no field to decode into. A pointer, so "no block at all" is
	// distinguishable from "a block of zeros" — see usageBlock.usage.
	Usage *usageBlock `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// usageBlock is the wire shape of the OpenAI-compatible `usage` object.
type usageBlock struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// usage normalizes the wire block into the typed Usage. Two deliberate rules:
//
//	nil block          → Measured=false (the provider reported nothing)
//	all-zero block     → Measured=false (a block is not a measurement; billing it would charge $0 forever)
//	total absent       → derived as prompt+completion, so a provider that omits the redundant field is
//	                     still MEASURED rather than being demoted to a guess over a formatting difference
func (u *usageBlock) usage() Usage {
	if u == nil {
		return Usage{}
	}
	total := u.TotalTokens
	if total <= 0 {
		total = u.PromptTokens + u.CompletionTokens
	}
	if total <= 0 {
		return Usage{}
	}
	return Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: total, Measured: true}
}

// Complete performs a chat completion for a user/agent against a model name (which LiteLLM resolves
// down the fallback ladder). It returns the assistant text. Model output is returned as DATA — callers
// (the agent loop) must never treat it as control flow, a command, or a query fragment (INV-08).
//
// It is the single observability point for model calls: it times the round trip, classifies the outcome,
// and (when an Obs is wired) reports it exactly once — so a slow reasoning model or a provider error is
// visible in the metrics/logs regardless of which caller made the call. On failure it returns a typed
// *ModelError carrying the HTTP status + class; behaviour for existing callers (who only test err != nil)
// is unchanged.
//
// It is also the bounded-failure chokepoint (TG-221): when a Breakers registry is wired, the call is
// admitted only while that model tier's circuit allows it, and its outcome is recorded back. An OPEN circuit
// returns a typed *ModelError of class breaker_open wrapping breaker.ErrOpen — never an empty string with a
// nil error — so the actuation path fails CLOSED and the judging path fails LOUD (see breaker.go).
func (g *Gateway) Complete(ctx context.Context, user, modelName string, msgs []Message) (string, error) {
	out, _, err := g.CompleteWithUsage(ctx, user, modelName, msgs)
	return out, err
}

// CompleteWithUsage is Complete plus the provider's REPORTED token accounting (TG-44). Complete delegates
// to it and discards the usage, so every existing caller behaves byte-identically and callers that need a
// truthful token count — the cost breaker's metering wrapper — opt in without a second HTTP path.
//
// The returned Usage is the zero value with Measured=false whenever the call did not reach a billable
// response (breaker open, transport failure, 4xx/5xx) or the provider returned no usage block. A caller
// must branch on Measured, never on TotalTokens != 0: those are different questions and conflating them is
// how an estimate gets billed as a measurement.
func (g *Gateway) CompleteWithUsage(ctx context.Context, user, modelName string, msgs []Message) (string, Usage, error) {
	start := time.Now()
	// TG-530: refuse an empty model name HERE, before any budget/breaker/slot accounting — the provider
	// would 400 it anyway, so the only effect of letting it through is a doomed round trip counted against
	// a breaker sample. The embeddings path has carried this exact guard since TG-221 (embed.go); the
	// completions path was the one gateway surface that would happily POST model:"" (observed live:
	// ~170-620 such calls/day from a present-but-empty tier env). Observed + typed so the refusal is still
	// visible in tg_model_calls_total and the caller tag names the misconfigured collaborator in the log.
	if strings.TrimSpace(modelName) == "" {
		me := &ModelError{Class: "bad_request", Message: "empty model name — the caller was constructed with a blank tier (TG-530); refusing the doomed call"}
		g.observe(modelName, user, "bad_request", 0, time.Since(start).Seconds(), me.Error())
		return "", Usage{}, me
	}
	// TG-48: a session that has already spent its cumulative output-token budget is refused BEFORE the breaker
	// and concurrency slot — an over-budget session must not consume a breaker sample or park in a slot it will
	// only be turned away from. Fail-safe: the caller gets a typed budget error (an incomplete investigation),
	// never an empty string with a nil error. Inert unless armed (SessionTokenBudget > 0).
	if g.sessionOverBudget(user) {
		me := &ModelError{Class: "session_budget", Message: "session output-token budget reached for " + user, wrapped: ErrSessionTokenBudget}
		g.observe(modelName, user, "session_budget", 0, time.Since(start).Seconds(), me.Error())
		return "", Usage{}, me
	}
	br := g.breakerFor(modelName)
	if !g.Breakers.allow(ctx, br) {
		err := errBreakerOpen(br.Name(), modelName)
		g.observe(modelName, user, ClassBreakerOpen, 0, time.Since(start).Seconds(), err.Error())
		return "", Usage{}, err
	}
	// TG-384: bound in-flight completions AFTER the breaker check — an open circuit rejects fast without
	// queuing behind a slot it will never use. A full gateway parks the caller here until a slot frees or the
	// context ends; it never drops the call. A context that ends WHILE WAITING is backpressure, not a provider
	// fault, so it returns a typed transport-class error (the actuation path fails closed, the judge path fails
	// loud) rather than an empty string with a nil error.
	if g.Concurrency != nil {
		if aerr := g.Concurrency.Acquire(ctx, 1); aerr != nil {
			me := &ModelError{Class: "transport", Message: "model gateway concurrency wait: " + aerr.Error(), wrapped: aerr}
			g.observe(modelName, user, "concurrency_wait_timeout", 0, time.Since(start).Seconds(), me.Error())
			return "", Usage{}, me
		}
		defer g.Concurrency.Release(1)
	}
	// A 429 is cooperative backpressure, not a model fault: the provider is asking us to slow down, and the
	// identical call will succeed a moment later. So retry it with bounded backoff (honoring Retry-After)
	// BEFORE surfacing or recording it — this is what lets the eval change gate's concurrent-session burst
	// ride out the provider's per-minute cap instead of failing whole arms on the first throttle (TG-534).
	// Only the FINAL attempt's outcome reaches the breaker: a 429 we successfully rode out must NOT accrue
	// toward a trip (that would re-arm the retry-storm the breaker exists to prevent), while a 429 that
	// exhausts our bounded retries is a genuine sustained rate-limit and accrues exactly as before. Every
	// intermediate 429 is observed as "rate_limit_retry" so the burst is visible in metrics without being
	// confused for a surfaced failure.
	maxRetries := g.RateLimitRetries
	switch {
	case maxRetries == 0:
		maxRetries = defaultRateLimitRetries // unset ⇒ the default budget
	case maxRetries < 0:
		maxRetries = 0 // explicit opt-out (tests that assert a single attempt): retry disabled, not defaulted
	}
	var (
		out     string
		usage   Usage
		status  int
		outcome string
		err     error
	)
	for attempt := 0; ; attempt++ {
		out, usage, status, outcome, err = g.do(ctx, user, modelName, msgs)
		if outcome != "rate_limit" || attempt >= maxRetries {
			break
		}
		delay := g.rateLimitBackoff(attempt, retryAfterOf(err))
		g.observe(modelName, user, "rate_limit_retry", status, time.Since(start).Seconds(),
			fmt.Sprintf("429 attempt %d/%d, backoff %s", attempt+1, maxRetries, delay))
		if sleepCtx(ctx, delay) != nil {
			// The caller's context ended during backoff — its deadline is shorter than the throttle. Stop and
			// surface the last 429 as the outcome (the real reason the call could not complete). The post-loop
			// record() then runs under the now-cancelled ctx: a store that honors ctx cancellation will fail
			// the write and degrade OPEN (no accrual) rather than trip. That is the right outcome here — one
			// caller's short deadline is not evidence of a sustained provider cap — so we do NOT force a
			// detached-context write to make it accrue.
			break
		}
	}
	g.Breakers.record(ctx, br, outcome, err)
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	g.observe(modelName, user, outcome, status, time.Since(start).Seconds(), detail)
	// Report usage ONLY for a call the provider was willing to bill (ok/empty). An empty 200 still consumed
	// prompt tokens — and, for a thinking-only model that spent its budget reasoning, completion tokens too —
	// so excluding it would under-count exactly the calls that cost the most and produced the least.
	if outcome == "ok" || outcome == "empty" {
		g.observeUsage(modelName, usage)
		// Accrue this billable call's output tokens to the session's running total (TG-48). An empty 200 that
		// spent completion tokens thinking counts too — that is exactly the call this budget exists to bound.
		g.addSessionTokens(user, usage.CompletionTokens)
	}
	return out, usage, err
}

// do performs the completion and returns the classified (output, usage, status, outcome, error). The
// outcome is a bounded label: ok | empty | rate_limit | timeout | bad_request | auth | provider_error |
// transport. An empty 200 (a reasoning model that spent its whole budget thinking → no content) is NOT an
// error — callers tolerate empty text — but it is reported as "empty" so the condition is not silent.
//
// The usage is the zero value (Measured=false) on every path that did not produce a billable body.
func (g *Gateway) do(ctx context.Context, user, modelName string, msgs []Message) (string, Usage, int, string, error) {
	key, err := g.APIKeyRef.Resolve()
	if err != nil {
		return "", Usage{}, 0, "transport", &ModelError{Class: "transport", Message: "resolve gateway key: " + err.Error(), wrapped: err}
	}
	body, err := json.Marshal(chatRequest{Model: modelName, Messages: msgs, User: user, MaxTokens: g.effectiveMaxTokens(ctx)})
	if err != nil {
		return "", Usage{}, 0, "transport", &ModelError{Class: "transport", Message: err.Error(), wrapped: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, 0, "transport", &ModelError{Class: "transport", Message: err.Error(), wrapped: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := g.HTTP.Do(req)
	if err != nil {
		cls := classifyTransport(err)
		return "", Usage{}, 0, cls, &ModelError{Class: cls, Message: "gateway call: " + err.Error(), wrapped: err}
	}
	defer resp.Body.Close()

	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", Usage{}, resp.StatusCode, "provider_error", &ModelError{Status: resp.StatusCode, Class: "provider_error", Message: "decode: " + err.Error()}
	}
	if cr.Error != nil || resp.StatusCode >= 400 {
		cls := classifyStatus(resp.StatusCode)
		msg := ""
		if cr.Error != nil {
			msg = cr.Error.Message
		}
		me := &ModelError{Status: resp.StatusCode, Class: cls, Message: msg}
		// A 429 carries the provider's requested wait in Retry-After (delta-seconds; the HTTP-date form is
		// not honored — providers here send seconds, and a clock-skewed date would mislead the backoff more
		// than help it). The retry loop prefers this over its own exponential schedule (TG-534).
		if resp.StatusCode == http.StatusTooManyRequests {
			me.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
		return "", Usage{}, resp.StatusCode, cls, me
	}
	usage := cr.Usage.usage()
	if len(cr.Choices) == 0 {
		return "", usage, resp.StatusCode, "empty", &ModelError{Status: resp.StatusCode, Class: "empty", Message: "no choices returned"}
	}
	content := cr.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" {
		// A 200 with empty content — e.g. kimi-k3 (a thinking-only model) spending its whole token budget on
		// reasoning. Not an error (callers tolerate empty text), but surfaced as "empty" so it is not silent.
		return content, usage, resp.StatusCode, "empty", nil
	}
	return content, usage, resp.StatusCode, "ok", nil
}
