package sessionspan

import (
	"context"
	"sync"

	"github.com/territory-grounder/grounder/adapters/model"
)

// Completer is the model-gateway seam the tally decorates — the same single method agent.Completer and
// adapters/model.Gateway.Complete satisfy. Declared locally so this leaf package never imports the agent.
type Completer interface {
	Complete(ctx context.Context, user, modelName string, msgs []model.Message) (string, error)
}

// UsageCompleter is the optional richer seam that also reports the provider's token accounting (TG-44).
// adapters/model.Gateway and core/cost.MeteringCompleter both satisfy it.
type UsageCompleter interface {
	CompleteWithUsage(ctx context.Context, user, modelName string, msgs []model.Message) (string, model.Usage, error)
}

// Tally is a PER-SESSION completer decorator that sums the provider-reported tokens of every completion one
// investigation made.
//
// It exists because the token truth arrives per CALL and the question an operator asks is per SESSION
// ("what did investigating INC-1234 cost?"). The process-wide /metrics counters cannot answer that — they
// are a fleet total — and the cost store answers it in dollars, not tokens, after a rate has already been
// applied. One short-lived Tally is constructed around the shared gateway for the duration of one agent
// run, so no cross-session state exists to leak or to mis-attribute.
//
// It is TRANSPARENT: result and error pass through unchanged, and it never fails a call. It counts CALLS
// as well as measured calls, so a session where the provider went quiet halfway renders "partial" rather
// than silently reporting the half it saw as the whole.
//
// Safe for concurrent use — the agent loop is sequential today, but the gateway is shared and a future
// parallel tool phase must not race the counters.
type Tally struct {
	inner Completer
	usage UsageCompleter

	mu  sync.Mutex
	tok Tokens
}

// NewTally wraps inner for one session. A nil inner is not valid (the caller has a completer or it has no
// agent); an inner that cannot report usage yields Source()=="unknown", which is the honest outcome.
func NewTally(inner Completer) *Tally {
	t := &Tally{inner: inner}
	if uc, ok := inner.(UsageCompleter); ok {
		t.usage = uc
	}
	return t
}

// Complete forwards the completion and records its usage. Nil-receiver safe is deliberately NOT offered:
// a nil Tally would silently drop every call, and a completer that returns "" with no error is the exact
// failure this repo has been bitten by.
func (t *Tally) Complete(ctx context.Context, user, modelName string, msgs []model.Message) (string, error) {
	if t.usage == nil {
		out, err := t.inner.Complete(ctx, user, modelName, msgs)
		t.record(model.Usage{})
		return out, err
	}
	out, u, err := t.usage.CompleteWithUsage(ctx, user, modelName, msgs)
	t.record(u)
	return out, err
}

// CompleteWithUsage forwards the richer seam so wrapping the gateway in a Tally does not hide the usage
// from anything composed further out.
func (t *Tally) CompleteWithUsage(ctx context.Context, user, modelName string, msgs []model.Message) (string, model.Usage, error) {
	if t.usage == nil {
		out, err := t.inner.Complete(ctx, user, modelName, msgs)
		t.record(model.Usage{})
		return out, model.Usage{}, err
	}
	out, u, err := t.usage.CompleteWithUsage(ctx, user, modelName, msgs)
	t.record(u)
	return out, u, err
}

// record accumulates one completion. An unmeasured call still increments Calls — that is what makes the
// "partial" verdict possible instead of a total that looks complete because the missing part left no trace.
func (t *Tally) record(u model.Usage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tok.Calls++
	if !u.Measured {
		return
	}
	t.tok.Measured++
	t.tok.Prompt += u.PromptTokens
	t.tok.Completion += u.CompletionTokens
	t.tok.Total += u.TotalTokens
}

// Tokens returns the session's accumulated accounting.
func (t *Tally) Tokens() Tokens {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tok
}

var (
	_ Completer      = (*Tally)(nil)
	_ UsageCompleter = (*Tally)(nil)
)
