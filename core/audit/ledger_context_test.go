package audit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// DEADLINE ORACLES FOR THE GOVERNANCE CHAIN (TG-277).
//
// THE DEFECT. The chain gate is held ACROSS the durable sink write, and that write ran on
// context.Background() with a sync.Mutex guarding it — so neither half of the wait could be cancelled.
// When Postgres stalled on dc1tg01 on 2026-08-04, a sealed-secret write spent its whole 15s activity
// budget inside step one and every other governance decision in the worker (classification, gating, mode
// transition, config write) queued behind it with no deadline and no recourse.
//
// Both tests assert a LATENCY BOUND, because "the append eventually returns" is exactly what the broken
// version also did.

// ctxSink records the deadline it was handed, proving the caller's budget actually reaches the durable
// write rather than being dropped at the sink boundary.
type ctxSink struct {
	gotDeadline bool
	block       time.Duration
	entered     chan struct{} // closed on the FIRST write, so a contention test can wait for the gate to be held
	enteredOnce sync.Once     // the second write must not panic: a broken gate lets one through, and the test
	// has to fail with its own message rather than a fake's crash.
}

func (s *ctxSink) Persist(LedgerEntry) error { return nil }

func (s *ctxSink) PersistContext(ctx context.Context, _ LedgerEntry) error {
	_, s.gotDeadline = ctx.Deadline()
	if s.entered != nil {
		s.enteredOnce.Do(func() { close(s.entered) })
	}
	if s.block > 0 {
		select {
		case <-time.After(s.block):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// TestAppendContextHandsTheCallersDeadlineToTheSink is the propagation half of the fix. Without it the
// sink writes on context.Background() and no caller can bound it, which is the state that let one INSERT
// consume a 15s StartToCloseTimeout.
func TestAppendContextHandsTheCallersDeadlineToTheSink(t *testing.T) {
	sink := &ctxSink{}
	l := NewLedger().WithSink(sink)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := l.AppendContext(ctx, GovDecision{Decision: "secret:put", ActionID: "secret:a:00"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if !sink.gotDeadline {
		t.Fatal("the durable ledger write received NO deadline: a stalled Postgres cannot be given up on, " +
			"so a governed write burns its entire activity budget in the append and reports an opaque " +
			"timeout — the TG-277 failure the operator read as 'the secret store is unreliable'")
	}
}

// TestAppendContextGivesUpOnTheChainGateInsideTheCallersBudget is the contention half. One slow holder of
// the gate must not be able to freeze every other governed decision in the worker for longer than each
// caller's own deadline.
func TestAppendContextGivesUpOnTheChainGateInsideTheCallersBudget(t *testing.T) {
	const hold = 5 * time.Second
	const callerBudget = 150 * time.Millisecond

	// A first append parks inside the gate for `hold`, standing in for the stalled substrate write.
	slow := &ctxSink{block: hold, entered: make(chan struct{})}
	l := NewLedger().WithSink(slow)
	go func() {
		_, _ = l.AppendContext(context.Background(), GovDecision{Decision: "classify:AUTO", ActionID: "a:1"})
	}()
	// Wait until the holder is genuinely INSIDE the gate. Without this the test could measure an
	// uncontended append and pass vacuously — it would prove nothing about contention at all.
	select {
	case <-slow.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the blocking append never entered the sink — the contention this test needs did not happen")
	}

	ctx, cancel := context.WithTimeout(context.Background(), callerBudget)
	defer cancel()
	start := time.Now()
	_, err := l.AppendContext(ctx, GovDecision{Decision: "secret:put", ActionID: "secret:b:00"})
	elapsed := time.Since(start)

	// THE LATENCY BOUND, asserted first: the failure this reproduces is a WAIT, and every other symptom is
	// downstream of it.
	if bound := 10 * callerBudget; elapsed > bound {
		t.Fatalf("a governance append waited %s behind ONE slow write (bound %s, caller budget %s, holder "+
			"%s): a stalled substrate freezes classification, gating, mode transitions and every other "+
			"governed decision in the worker for as long as the database is unwell, not for as long as each "+
			"caller allowed (TG-277)", elapsed, bound, callerBudget, hold)
	}
	if !errors.Is(err, ErrChainBusy) {
		t.Fatalf("waiting for the chain gate returned %v, want ErrChainBusy — the caller must be told the "+
			"append never happened, so nothing is recorded that did not occur", err)
	}
}

// TestAppendKeepsWorkingWithoutAContext guards the safe default: every existing caller of the plain
// Append (45 of them across the worker) must behave exactly as before, including the non-blocking fast
// path on an uncontended gate.
func TestAppendKeepsWorkingWithoutAContext(t *testing.T) {
	l := NewLedger()
	for i := 0; i < 3; i++ {
		if _, err := l.Append(GovDecision{Decision: "classify:AUTO", ActionID: "a"}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if got := l.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}
	if err := l.Verify(); err != nil {
		t.Fatalf("chain no longer verifies after the gate change: %v", err)
	}
}

// TestAppendContextFastPathIgnoresAnAlreadyExpiredContext: an UNCONTENDED append must not be refused just
// because the caller's context is already done. Turning a write that would have succeeded instantly into
// a refusal would over-record nothing but would fail governed writes that were fine.
func TestAppendContextFastPathIgnoresAnAlreadyExpiredContext(t *testing.T) {
	l := NewLedger()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.AppendContext(ctx, GovDecision{Decision: "classify:AUTO", ActionID: "a"}); err != nil {
		t.Fatalf("an uncontended append with an expired context was refused: %v", err)
	}
}
