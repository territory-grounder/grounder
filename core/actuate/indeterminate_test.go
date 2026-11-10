package actuate

// TG-663 (G4A F4): the INDETERMINATE terminal. An Exec error the leaf marked actuation.ErrIndeterminate — the
// request entered the transport and the acknowledgement was lost — is neither a refusal ("provably did not
// execute") nor an execution (no verdict): the target's state is unknown. The interceptor records it as such
// (ledger `actuate:execute:indeterminate`, a per-run row with verdict `indeterminate` through the optional
// IndeterminateSink), cools the target down, computes no verdict and feeds no ladder.
//
// KILLING MUTATION (executed 2026-11-07): delete the `errors.Is(err, actuation.ErrIndeterminate)` branch at the
// execute site (the pre-TG-663 shape: every leaf error is a refusal). RED: TestIndeterminateExecIsItsOwnTerminal
// (Refused instead of Indeterminate; the ledger carries actuate:refuse) and
// TestIndeterminateCancellationIsBothCancelledAndIndeterminate. Restored from a copy, green.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
)

// indeterminateActuator fires (the effect may have landed) and then loses its acknowledgement.
type indeterminateActuator struct {
	execs int
	err   error
}

func (a *indeterminateActuator) Capability() string { return "test" }
func (a *indeterminateActuator) ReadOnly() bool     { return false }
func (a *indeterminateActuator) Exec(_ context.Context, _ []string, _ []byte) (actuation.Result, error) {
	a.execs++
	return actuation.Result{}, a.err
}

// indeterminateSink records verified rows and indeterminate rows separately.
type indeterminateSink struct{ rows, indeterminate int }

func (s *indeterminateSink) Record(_ context.Context, _, _, _, _ string, _ safety.Verdict, _ bool, _ string) error {
	s.rows++
	return nil
}
func (s *indeterminateSink) RecordIndeterminate(_ context.Context, _, _, _, _, _, _ string) error {
	s.indeterminate++
	return nil
}

// plainSink records verified rows only — it lacks the indeterminate capability.
type plainSink struct{ rows int }

func (s *plainSink) Record(_ context.Context, _, _, _, _ string, _ safety.Verdict, _ bool, _ string) error {
	s.rows++
	return nil
}

type ledgerCapture struct{ decisions []string }

func (c *ledgerCapture) Persist(e audit.LedgerEntry) error {
	c.decisions = append(c.decisions, e.Decision)
	return nil
}

func (c *ledgerCapture) has(decision string) bool {
	for _, d := range c.decisions {
		if d == decision {
			return true
		}
	}
	return false
}

func TestIndeterminateExecIsItsOwnTerminal(t *testing.T) {
	act := &indeterminateActuator{err: actuation.MarkIndeterminate(errors.New("ssh: remote exec on web01:22 lost its acknowledgement after launch: EOF"))}
	cap := &ledgerCapture{}
	sink := &indeterminateSink{}
	adm := &fakeAdmission{}
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger().WithSink(cap)).
		WithExecutionSink(sink).WithTargetAdmission(adm)
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if act.execs != 1 {
		t.Fatalf("the effect must have been attempted exactly once, got %d", act.execs)
	}
	if !out.Indeterminate || out.Executed || out.Refused || out.Cancelled {
		t.Fatalf("want Indeterminate only (not executed, not refused, not cancelled), got %+v", out)
	}
	if !strings.HasPrefix(out.Reason, RefusalIndeterminate) {
		t.Fatalf("reason must start with the stable token %q, got %q", RefusalIndeterminate, out.Reason)
	}
	if out.Verdict != "" {
		t.Fatalf("no verdict can be computed against an unknown effect, got %q", out.Verdict)
	}
	if sink.indeterminate != 1 || sink.rows != 0 {
		t.Fatalf("exactly one INDETERMINATE row and no verified row, got indeterminate=%d rows=%d", sink.indeterminate, sink.rows)
	}
	if !cap.has("actuate:execute:indeterminate") || cap.has("actuate:refuse") {
		t.Fatalf("the ledger must carry execute:indeterminate and no refusal, got %v", cap.decisions)
	}
	if adm.releases != 1 || !adm.lastDisturbed {
		t.Fatalf("the durable admission must release DISTURBED (cooldown) on an indeterminate effect, got releases=%d disturbed=%v", adm.releases, adm.lastDisturbed)
	}
}

// A cancellation that landed on a RUNNING command is both cancelled (the TG-80 terminal keeps its name) and
// indeterminate (the armed revert must not be told the effect provably did not execute).
func TestIndeterminateCancellationIsBothCancelledAndIndeterminate(t *testing.T) {
	act := &indeterminateActuator{err: actuation.MarkIndeterminate(fmt.Errorf("ssh: remote command cancelled: SIGTERM sent to web01:22 before closing the transport: %w", context.DeadlineExceeded))}
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger())
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !out.Indeterminate || !out.Cancelled || out.Refused || out.Executed {
		t.Fatalf("want Indeterminate+Cancelled, got %+v", out)
	}
}

// POSITIVE CONTROL: a plain (pre-send) leaf error stays a refusal and never touches the indeterminate path.
func TestPlainExecErrorIsRefusedNotIndeterminate(t *testing.T) {
	act := &erroringActuator{}
	cap := &ledgerCapture{}
	sink := &indeterminateSink{}
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger().WithSink(cap)).WithExecutionSink(sink)
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !out.Refused || out.Indeterminate || sink.indeterminate != 0 || !cap.has("actuate:refuse") {
		t.Fatalf("a pre-send failure must stay a refusal, got %+v sink=%+v ledger=%v", out, sink, cap.decisions)
	}
}

// A per-run recorder without the indeterminate capability is a LEDGERED gap, never a crash and never a
// fabricated verified row.
func TestIndeterminateWithoutACapableSinkIsLedgeredNotFabricated(t *testing.T) {
	act := &indeterminateActuator{err: actuation.MarkIndeterminate(errors.New("ack lost"))}
	cap := &ledgerCapture{}
	sink := &plainSink{}
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger().WithSink(cap)).WithExecutionSink(sink)
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !out.Indeterminate || sink.rows != 0 || !cap.has("actuate:execution-record-missing:indeterminate") {
		t.Fatalf("want indeterminate with the missing-recorder gap ledgered and no verified row, got %+v rows=%d ledger=%v", out, sink.rows, cap.decisions)
	}
}
