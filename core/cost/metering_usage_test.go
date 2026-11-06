package cost

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
)

// usageCompleter is a fake gateway that ALSO reports provider usage — the shape adapters/model.Gateway now
// has. reported is what the provider says; it is deliberately different from the chars/4 estimate so a test
// can tell which one was billed.
type usageCompleter struct {
	reply    string
	reported model.Usage
	calls    int
}

func (u *usageCompleter) Complete(ctx context.Context, user, modelName string, msgs []model.Message) (string, error) {
	out, _, err := u.CompleteWithUsage(ctx, user, modelName, msgs)
	return out, err
}

func (u *usageCompleter) CompleteWithUsage(_ context.Context, _, _ string, _ []model.Message) (string, model.Usage, error) {
	u.calls++
	return u.reply, u.reported, nil
}

// TestBillsReportedTokensNotTheEstimate is the TG-44 spend oracle.
//
// The numbers are the LIVE ones measured against dc1tg01's LiteLLM on 2026-08-04: a 3409-char prompt
// with a 2-char reply estimates 852 tokens at chars/4 and the provider REPORTED 1607. At $1 per 1k tokens
// that is $0.852 billed versus $1.607 actually spent — so a $1.00 daily budget looked 15% clear when it
// was already 60% over.
//
// KILLING MUTATION (EXECUTED 2026-08-04): in CompleteWithUsage, replace the measured branch's
// `m.acct.AccrueLLM(ctx, modelName, user, u.TotalTokens)` with the estimate
// (`approxTokens(msgs, out)`) — i.e. put back the pre-TG-44 behaviour. This test fails with
//
//	billed $0.8520 but the provider REPORTED 1607 tokens = $1.6070 — the spend guard is
//	billing a chars/4 guess, so the daily ceiling is ~1.9x higher than the operator set (TG-44)
//
// Restored, green.
func TestBillsReportedTokensNotTheEstimate(t *testing.T) {
	st := NewMemStore()
	acct := newTestAccountant(st, Config{DefaultRate: 1.0}, &fakeForcer{})
	inner := &usageCompleter{
		reply:    "OK",
		reported: model.Usage{PromptTokens: 1603, CompletionTokens: 4, TotalTokens: 1607, Measured: true},
	}
	mc := NewMeteringCompleter(inner, acct, WithMeteringLogf(func(string, ...any) {}))

	ctx := context.Background()
	msgs := []model.Message{{Role: "user", Content: strings.Repeat("x", 3409)}}
	if _, err := mc.Complete(ctx, "runner:ext-1", "fast", msgs); err != nil {
		t.Fatalf("wrapper must be transparent: %v", err)
	}

	got, err := st.Total(ctx, BucketSession, "runner:ext-1")
	if err != nil {
		t.Fatalf("read session total: %v", err)
	}
	const wantUSD = 1.607      // 1607 reported tokens at $1/1k
	const estimateUSD = 0.8520 // (3409+2)/4 = 852 tokens at $1/1k
	if !nearly(got, wantUSD) {
		t.Fatalf("billed $%.4f but the provider REPORTED 1607 tokens = $%.4f — the spend guard is billing a "+
			"chars/4 guess (that estimate would bill $%.4f), so the daily ceiling is ~1.9x higher than the "+
			"operator set (TG-44)", got, wantUSD, estimateUSD)
	}
}

// TestFallsBackToEstimateAndSaysSo. A provider that reports nothing must still be metered — a spend guard
// an outage silently disarms is worse than an approximate one — but the fallback must ANNOUNCE itself.
func TestFallsBackToEstimateAndSaysSo(t *testing.T) {
	st := NewMemStore()
	acct := newTestAccountant(st, Config{DefaultRate: 1.0}, &fakeForcer{})
	inner := &usageCompleter{reply: "OK"} // reported: zero value ⇒ Measured=false
	var logged []string
	mc := NewMeteringCompleter(inner, acct, WithMeteringLogf(func(f string, a ...any) {
		logged = append(logged, f)
	}))

	ctx := context.Background()
	msgs := []model.Message{{Role: "user", Content: strings.Repeat("x", 400)}}
	for i := 0; i < 3; i++ {
		if _, err := mc.Complete(ctx, "runner:ext-2", "fast", msgs); err != nil {
			t.Fatalf("wrapper must be transparent: %v", err)
		}
	}
	total, _ := st.Total(ctx, BucketSession, "runner:ext-2")
	// 3 calls x (400+2)/4 = 100 tokens x $1/1k
	if !nearly(total, 0.3) {
		t.Fatalf("unmeasured calls billed $%.4f, want $0.3000 — an absent usage block must fall back to the "+
			"estimate, never to zero (a guard that stops accruing during an outage is disarmed by it)", total)
	}
	if len(logged) != 1 {
		t.Fatalf("estimate fallback logged %d times over 3 calls, want exactly 1 per tier — silent means "+
			"nobody knows the bill is a guess; per-call means the warning is noise", len(logged))
	}
	if !strings.Contains(logged[0], "ESTIMATE") {
		t.Fatalf("fallback notice %q must say it is an ESTIMATE", logged[0])
	}
}

// TestWrapperDoesNotHideUsageFromCallersOutside: the decorator must not narrow the interface it decorates.
// The composition root wraps the gateway in the metering completer, so if MeteringCompleter satisfied only
// Completer the per-session tally would see Measured=false for every call and every exported trace would
// read tokens_source=unknown — the measurement would be erased by the act of guarding spend.
func TestWrapperDoesNotHideUsageFromCallersOutside(t *testing.T) {
	acct := newTestAccountant(NewMemStore(), Config{DefaultRate: 1.0}, &fakeForcer{})
	inner := &usageCompleter{reply: "OK", reported: model.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, Measured: true}}
	mc := NewMeteringCompleter(inner, acct, WithMeteringLogf(func(string, ...any) {}))

	var outer UsageCompleter = mc // compile-time: the wrapper still reports usage
	_, u, err := outer.CompleteWithUsage(context.Background(), "runner:ext-3", "fast", []model.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("transparent: %v", err)
	}
	if !u.Measured || u.TotalTokens != 12 {
		t.Fatalf("wrapped gateway reported usage %+v — the metering decorator erased the measurement it was "+
			"composed around (TG-44)", u)
	}
}

// TestCompleterWithoutUsageStillMeters: a plain Completer (a test double, a future adapter) must keep
// working exactly as before — billed from the estimate, and told so.
func TestCompleterWithoutUsageStillMeters(t *testing.T) {
	st := NewMemStore()
	acct := newTestAccountant(st, Config{DefaultRate: 1.0}, &fakeForcer{})
	inner := &scriptedCompleter{reply: "0123456789012345"} // 16 chars
	var logged int
	mc := NewMeteringCompleter(inner, acct, WithMeteringLogf(func(string, ...any) { logged++ }))

	ctx := context.Background()
	if _, err := mc.Complete(ctx, "runner:ext-4", "fast", []model.Message{{Role: "user", Content: "aaaaaaaa"}}); err != nil {
		t.Fatalf("transparent: %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner called %d times, want 1", inner.calls)
	}
	total, _ := st.Total(ctx, BucketSession, "runner:ext-4")
	if !nearly(total, 0.006) { // (8+16)/4 = 6 tokens at $1/1k
		t.Fatalf("plain completer billed $%.4f, want $0.0060 (the unchanged pre-TG-44 estimate path)", total)
	}
	if logged != 1 {
		t.Fatalf("a completer that cannot report usage logged %d notices, want 1", logged)
	}
	// Its CompleteWithUsage must report Measured=false rather than fabricating a number.
	_, u, err := mc.CompleteWithUsage(ctx, "runner:ext-4", "fast", []model.Message{{Role: "user", Content: "a"}})
	if err != nil {
		t.Fatalf("transparent: %v", err)
	}
	if u.Measured {
		t.Fatalf("a completer with no usage seam reported Measured=true (%+v) — that is a fabricated measurement", u)
	}
}

func nearly(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}
