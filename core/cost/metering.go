package cost

import (
	"context"

	"github.com/territory-grounder/grounder/adapters/model"
)

// Completer is the minimal model-gateway seam the metering wrapper decorates — the SAME method the agent
// loop needs (adapters/model.Gateway.Complete and agent.Completer satisfy it). Kept as a local interface so
// core/cost depends only on the message TYPE, never on the agent package.
type Completer interface {
	Complete(ctx context.Context, user, modelName string, msgs []model.Message) (string, error)
}

// MeteringCompleter wraps a model-gateway Completer and accrues the approximate USD cost of every
// completion into the spend guard — the CLEANEST hook, right at the boundary where TG already sees the
// request messages and the response text (the gateway returns no usage count, so tokens are the standard
// ~4-chars/token approximation over the request + response). It is composed at the worker composition root
// around the gateway the agent loop calls, so no runner/interceptor code changes to meter spend.
//
// It is TRANSPARENT: it returns the inner completer's result and error UNCHANGED and NEVER fails a call on
// a cost concern — a spend guard must not break inference. Accrual is a side effect after the inner call.
// The wrapper itself is stateless and safe for concurrent use (the Accountant + Store own all coordination).
type MeteringCompleter struct {
	inner Completer
	acct  *Accountant
}

// NewMeteringCompleter wraps inner so each completion accrues into acct. A nil acct leaves inner behavior
// exactly unchanged (the accountant's methods are nil-safe no-ops), so the wrapper is always safe to apply.
func NewMeteringCompleter(inner Completer, acct *Accountant) *MeteringCompleter {
	return &MeteringCompleter{inner: inner, acct: acct}
}

// Complete calls the inner completer, then accrues the approximate cost of the exchange (request messages +
// response text) keyed by the model tier and the user/session. The inner result/error is returned
// unchanged. Accrual runs even on an inner error: the provider still processed the prompt tokens, so the
// request cost was spent — the response tokens are simply absent (empty out). The Accountant's evaluation
// may force the mode to Shadow when a budget is crossed; it never affects the returned value.
func (m *MeteringCompleter) Complete(ctx context.Context, user, modelName string, msgs []model.Message) (string, error) {
	out, err := m.inner.Complete(ctx, user, modelName, msgs)
	m.acct.AccrueLLM(ctx, modelName, user, approxTokens(msgs, out))
	return out, err
}

// approxTokens is the conventional ~4-chars/token approximation over the request messages plus the
// response text — the same basis core/observe uses for tg_agent_tokens_approx_total (the gateway returns
// no exact usage). It reads only content TG already holds (never a secret); it is a pure function.
func approxTokens(msgs []model.Message, out string) int {
	chars := len(out)
	for _, mm := range msgs {
		chars += len(mm.Content)
	}
	return chars / 4
}
