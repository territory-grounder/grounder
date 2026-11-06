package sessionspan

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
)

// scriptedUsage returns a scripted usage per call, cycling through the script.
type scriptedUsage struct {
	script []model.Usage
	err    error
	n      int
}

func (s *scriptedUsage) Complete(ctx context.Context, user, m string, msgs []model.Message) (string, error) {
	out, _, err := s.CompleteWithUsage(ctx, user, m, msgs)
	return out, err
}

func (s *scriptedUsage) CompleteWithUsage(_ context.Context, _, _ string, _ []model.Message) (string, model.Usage, error) {
	u := model.Usage{}
	if s.n < len(s.script) {
		u = s.script[s.n]
	}
	s.n++
	return "reply", u, s.err
}

// plainCompleter cannot report usage — a scripted double, or any adapter predating TG-44.
type plainCompleter struct{ n int }

func (p *plainCompleter) Complete(_ context.Context, _, _ string, _ []model.Message) (string, error) {
	p.n++
	return "reply", nil
}

// TestTallySumsReportedTokensAcrossASession is why the tally exists: /metrics counts tokens fleet-wide and
// the cost store holds dollars, so neither answers "what did investigating THIS incident cost".
func TestTallySumsReportedTokensAcrossASession(t *testing.T) {
	inner := &scriptedUsage{script: []model.Usage{
		{PromptTokens: 1000, CompletionTokens: 50, TotalTokens: 1050, Measured: true},
		{PromptTokens: 1200, CompletionTokens: 30, TotalTokens: 1230, Measured: true},
	}}
	ta := NewTally(inner)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := ta.Complete(ctx, "runner:INC-1", "fast", nil); err != nil {
			t.Fatalf("tally must be transparent: %v", err)
		}
	}
	got := ta.Tokens()
	if got.Total != 2280 || got.Prompt != 2200 || got.Completion != 80 {
		t.Fatalf("tally=%+v want total=2280 prompt=2200 completion=80", got)
	}
	if got.Calls != 2 || got.Measured != 2 || got.Source() != "measured" {
		t.Fatalf("tally=%+v source=%q want 2/2 calls measured", got, got.Source())
	}
}

// TestTallyCountsUnmeasuredCallsSoPartialIsVisible. A session where the provider went quiet halfway must
// not report the half it saw as the whole.
func TestTallyCountsUnmeasuredCallsSoPartialIsVisible(t *testing.T) {
	inner := &scriptedUsage{script: []model.Usage{
		{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, Measured: true},
		{}, // provider reported nothing
	}}
	ta := NewTally(inner)
	ctx := context.Background()
	_, _ = ta.Complete(ctx, "u", "fast", nil)
	_, _ = ta.Complete(ctx, "u", "fast", nil)
	got := ta.Tokens()
	if got.Calls != 2 || got.Measured != 1 {
		t.Fatalf("tally=%+v want 2 calls / 1 measured — an unmeasured call must still be COUNTED, or the "+
			"total looks complete because the missing part left no trace", got)
	}
	if got.Source() != "partial" {
		t.Fatalf("source=%q want partial", got.Source())
	}
}

// TestTallyOverAPlainCompleterReportsUnknown — never a fabricated zero-as-measurement.
func TestTallyOverAPlainCompleterReportsUnknown(t *testing.T) {
	inner := &plainCompleter{}
	ta := NewTally(inner)
	if _, err := ta.Complete(context.Background(), "u", "fast", nil); err != nil {
		t.Fatalf("transparent: %v", err)
	}
	got := ta.Tokens()
	if inner.n != 1 {
		t.Fatalf("inner called %d times, want 1", inner.n)
	}
	if got.Calls != 1 || got.Measured != 0 || got.Source() != "unknown" {
		t.Fatalf("tally=%+v source=%q — a completer with no usage seam must report unknown, not zero-measured",
			got, got.Source())
	}
}

// TestTallyIsTransparentOnError: a metering decorator must never change what the loop sees.
func TestTallyIsTransparentOnError(t *testing.T) {
	boom := errors.New("gateway exploded")
	ta := NewTally(&scriptedUsage{err: boom})
	out, err := ta.Complete(context.Background(), "u", "fast", nil)
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v want the inner error passed through unchanged", err)
	}
	if out != "reply" {
		t.Fatalf("out=%q want the inner result passed through unchanged", out)
	}
	if ta.Tokens().Calls != 1 {
		t.Fatal("a failed call must still be counted — otherwise a session of failures reports zero calls")
	}
}
