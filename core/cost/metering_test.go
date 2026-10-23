package cost

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
)

// scriptedCompleter is a fake model gateway: it returns a fixed response (or error) and records the last
// call, so the metering wrapper can be driven without a live gateway.
type scriptedCompleter struct {
	reply string
	err   error
	calls int
}

func (s *scriptedCompleter) Complete(_ context.Context, _ string, _ string, _ []model.Message) (string, error) {
	s.calls++
	return s.reply, s.err
}

func newTestAccountant(st Store, cfg Config, f ShadowForcer) *Accountant {
	a, _ := New(st, cfg, f, nil, WithClock(fixedClock("2026-07-20")))
	return a
}

// The wrapper is transparent (returns the inner result) AND accrues the request+response tokens.
func TestMeteringCompleter_TransparentAndAccrues(t *testing.T) {
	st := NewMemStore()
	f := &fakeForcer{}
	// DefaultRate $1/1k tokens; approxTokens = (len(request)+len(reply))/4.
	acct := newTestAccountant(st, Config{DefaultRate: 1.0}, f)
	inner := &scriptedCompleter{reply: "0123456789012345"} // 16 chars response
	mc := NewMeteringCompleter(inner, acct)

	ctx := context.Background()
	req := []model.Message{{Role: "user", Content: "aaaaaaaa"}} // 8 chars request
	out, err := mc.Complete(ctx, "runner:ext-1", "fast", req)
	if err != nil || out != inner.reply {
		t.Fatalf("wrapper must be transparent: out=%q err=%v", out, err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner completer must be called once, got %d", inner.calls)
	}
	// (8 + 16) / 4 = 6 tokens ⇒ 6/1000 * $1 = $0.006 accrued to day + session.
	wantUSD := 6.0 / 1000.0
	if got := acct.TodayUSD(ctx); got != wantUSD {
		t.Fatalf("metered daily accrual = %v want %v", got, wantUSD)
	}
	if got, _ := st.Total(ctx, BucketSession, "runner:ext-1"); got != wantUSD {
		t.Fatalf("metered session accrual = %v want %v", got, wantUSD)
	}
}

// Accrual runs even when the inner completer errors (the request tokens were spent), and the error is
// returned unchanged.
func TestMeteringCompleter_AccruesOnInnerError(t *testing.T) {
	st := NewMemStore()
	f := &fakeForcer{}
	acct := newTestAccountant(st, Config{DefaultRate: 1.0}, f)
	boom := errors.New("gateway 500")
	inner := &scriptedCompleter{err: boom}
	mc := NewMeteringCompleter(inner, acct)

	ctx := context.Background()
	req := []model.Message{{Role: "user", Content: "12345678"}} // 8 chars ⇒ 2 tokens
	out, err := mc.Complete(ctx, "runner:ext-2", "fast", req)
	if !errors.Is(err, boom) || out != "" {
		t.Fatalf("the inner error must be returned unchanged: out=%q err=%v", out, err)
	}
	if got := acct.TodayUSD(ctx); got != 2.0/1000.0 {
		t.Fatalf("request tokens must accrue even on an inner error: %v", got)
	}
}

// A nil accountant leaves the wrapper a pure pass-through (never panics).
func TestMeteringCompleter_NilAccountantPassThrough(t *testing.T) {
	inner := &scriptedCompleter{reply: "ok"}
	mc := NewMeteringCompleter(inner, nil)
	out, err := mc.Complete(context.Background(), "u", "fast", []model.Message{{Content: "x"}})
	if out != "ok" || err != nil || inner.calls != 1 {
		t.Fatalf("nil-accountant wrapper must pass through: out=%q err=%v calls=%d", out, err, inner.calls)
	}
}

// Driving the wrapper repeatedly over a low daily budget trips the breaker through the metering path (the
// live production hook: every agent completion accrues and can halt).
func TestMeteringCompleter_TripsThroughMeteringPath(t *testing.T) {
	st := NewMemStore()
	f := &fakeForcer{}
	acct := newTestAccountant(st, Config{DefaultRate: 1.0, DailyBudgetUSD: 0.01}, f)
	inner := &scriptedCompleter{reply: string(make([]byte, 40))} // 40-char reply ⇒ 10 tokens ⇒ $0.01
	mc := NewMeteringCompleter(inner, acct)
	if _, err := mc.Complete(context.Background(), "runner:ext-3", "fast", nil); err != nil {
		t.Fatal(err)
	}
	if f.forced() == 0 || !acct.Tripped(context.Background()) {
		t.Fatalf("a completion over the daily budget must trip through the metering path: forced=%d", f.forced())
	}
}
