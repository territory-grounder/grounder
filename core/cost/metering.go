package cost

import (
	"context"
	"log"
	"sync"

	"github.com/territory-grounder/grounder/adapters/model"
)

// Completer is the minimal model-gateway seam the metering wrapper decorates — the SAME method the agent
// loop needs (adapters/model.Gateway.Complete and agent.Completer satisfy it). Kept as a local interface so
// core/cost depends only on the message TYPE, never on the agent package.
type Completer interface {
	Complete(ctx context.Context, user, modelName string, msgs []model.Message) (string, error)
}

// UsageCompleter is the OPTIONAL richer seam: a completer that also returns the PROVIDER'S REPORTED token
// accounting (adapters/model.Gateway satisfies it, TG-44). MeteringCompleter discovers it by type assertion
// rather than demanding it, so a Completer that cannot report usage — a scripted test double, a future
// adapter — still meters, just from the estimate, and says so.
type UsageCompleter interface {
	CompleteWithUsage(ctx context.Context, user, modelName string, msgs []model.Message) (string, model.Usage, error)
}

// MeteringCompleter wraps a model-gateway Completer and accrues the USD cost of every completion into the
// spend guard — the CLEANEST hook, right at the boundary where TG already sees the request messages and the
// response. It is composed at the worker composition root around the gateway the agent loop calls, so no
// runner/interceptor code changes to meter spend.
//
// WHAT IT BILLS, AND WHY THAT CHANGED (TG-44). It bills the provider's REPORTED total tokens whenever the
// inner completer can report them, and falls back to the chars/4 approximation only when it cannot. Until
// this change the gateway decoded responses into a struct with no `usage` field, so the usage block LiteLLM
// returns on every completion was silently discarded and the spend guard billed the estimate. Measured
// against the live gateway (dc1tg01) on 2026-08-04:
//
//	prompt 47 chars   → estimate    12 tokens · REPORTED   166
//	prompt 3409 chars → estimate   852 tokens · REPORTED  1607
//
// The estimate is LOW, by a factor that grows as the prompt shrinks (per-message and system-prompt overhead
// is invisible to a character count), so a TG_COST_DAILY_BUDGET_USD of $X permitted roughly $2X of real
// spend before the breaker tripped — a ceiling that was meant to be a guard was really a suggestion. The
// fallback is KEPT (a spend guard must not go dark because a provider omitted a field) but it is ANNOUNCED
// once per tier instead of passing as a measurement.
//
// It is TRANSPARENT: it returns the inner completer's result and error UNCHANGED and NEVER fails a call on
// a cost concern — a spend guard must not break inference. Accrual is a side effect after the inner call.
// The wrapper is safe for concurrent use (the Accountant + Store own all coordination; the once-per-tier
// estimate notice has its own mutex).
type MeteringCompleter struct {
	inner Completer
	// usage is inner when it can report provider usage, else nil. Resolved ONCE at construction so the call
	// path is a nil check rather than a per-call type assertion.
	usage UsageCompleter
	acct  *Accountant
	logf  func(format string, args ...any)

	estMu   sync.Mutex
	estSeen map[string]bool
}

// MeteringOption configures a MeteringCompleter.
type MeteringOption func(*MeteringCompleter)

// WithMeteringLogf injects the logger the estimate-fallback notice is announced through. A nil logf leaves
// the default (log.Printf) in place: the fallback must never be able to become SILENT by omission, because
// a silently estimated bill is exactly the condition TG-44 exists to remove.
func WithMeteringLogf(f func(format string, args ...any)) MeteringOption {
	return func(m *MeteringCompleter) {
		if f != nil {
			m.logf = f
		}
	}
}

// NewMeteringCompleter wraps inner so each completion accrues into acct. A nil acct leaves inner behavior
// exactly unchanged (the accountant's methods are nil-safe no-ops), so the wrapper is always safe to apply.
func NewMeteringCompleter(inner Completer, acct *Accountant, opts ...MeteringOption) *MeteringCompleter {
	m := &MeteringCompleter{inner: inner, acct: acct, logf: log.Printf, estSeen: map[string]bool{}}
	if uc, ok := inner.(UsageCompleter); ok {
		m.usage = uc
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Complete calls the inner completer, then accrues the cost of the exchange keyed by the model tier and the
// user/session. The inner result/error is returned unchanged. Accrual runs even on an inner error: the
// provider still processed the prompt tokens, so the request cost was spent. The Accountant's evaluation
// may force the mode to Shadow when a budget is crossed; it never affects the returned value.
func (m *MeteringCompleter) Complete(ctx context.Context, user, modelName string, msgs []model.Message) (string, error) {
	if m.usage == nil {
		out, err := m.inner.Complete(ctx, user, modelName, msgs)
		m.accrueEstimate(ctx, user, modelName, msgs, out, "the wrapped completer cannot report provider usage")
		return out, err
	}
	out, _, err := m.CompleteWithUsage(ctx, user, modelName, msgs)
	return out, err
}

// CompleteWithUsage is Complete plus the provider's reported token accounting, forwarded THROUGH the
// wrapper. It exists so composing the spend guard does not COST a caller the truthful count: without it
// MeteringCompleter would satisfy Completer only, and wrapping the gateway at the composition root would
// erase the very measurement this change adds — a decorator quietly narrowing the interface it decorates.
// When the inner completer cannot report usage the returned Usage is the zero value (Measured=false),
// which is the honest answer rather than a fabricated one.
func (m *MeteringCompleter) CompleteWithUsage(ctx context.Context, user, modelName string, msgs []model.Message) (string, model.Usage, error) {
	if m.usage == nil {
		out, err := m.inner.Complete(ctx, user, modelName, msgs)
		m.accrueEstimate(ctx, user, modelName, msgs, out, "the wrapped completer cannot report provider usage")
		return out, model.Usage{}, err
	}
	out, u, err := m.usage.CompleteWithUsage(ctx, user, modelName, msgs)
	if u.Measured {
		m.acct.AccrueLLM(ctx, modelName, user, u.TotalTokens)
		return out, u, err
	}
	// No measurement: either the call never reached a billable response (a transport failure or an open
	// breaker, where an estimate over the request messages is the only basis there has ever been) or the
	// provider returned no usage block. Bill the estimate rather than billing zero — a spend guard that
	// stops accruing when a provider omits a field is a guard an outage silently disarms.
	m.accrueEstimate(ctx, user, modelName, msgs, out, "no usage block in the response")
	return out, u, err
}

// accrueEstimate bills the chars/4 fallback and announces it once per tier.
func (m *MeteringCompleter) accrueEstimate(ctx context.Context, user, modelName string, msgs []model.Message, out, why string) {
	m.noteEstimating(modelName, why)
	m.acct.AccrueLLM(ctx, modelName, user, approxTokens(msgs, out))
}

// noteEstimating announces, ONCE PER MODEL TIER, that this tier's spend is being billed from an estimate.
// Once per tier rather than per call: a provider that never reports usage would otherwise write a line for
// every completion, and a warning that repeats on every call is a warning that gets filtered. The running
// count lives on /metrics as tg_model_usage_missing_total.
func (m *MeteringCompleter) noteEstimating(modelName, why string) {
	m.estMu.Lock()
	first := !m.estSeen[modelName]
	if first {
		m.estSeen[modelName] = true
	}
	m.estMu.Unlock()
	if !first {
		return
	}
	m.logf("cost: billing model tier %q from the chars/4 ESTIMATE (%s) — the estimate measured 1.9x-13.8x "+
		"LOW against the live gateway on 2026-08-04, so the daily/session ceilings are approximate for this "+
		"tier and real spend can exceed them before the breaker trips (TG-44)", modelName, why)
}

// approxTokens is the conventional ~4-chars/token approximation over the request messages plus the
// response text. It is the FALLBACK basis only — see the MeteringCompleter comment for how wrong it
// measured. It reads only content TG already holds (never a secret); it is a pure function.
func approxTokens(msgs []model.Message, out string) int {
	chars := len(out)
	for _, mm := range msgs {
		chars += len(mm.Content)
	}
	return chars / 4
}
