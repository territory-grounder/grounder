package actuate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
)

// cancellingActuator is an effect leaf whose Exec reports the caller's deadline landing mid-run, the way
// the SSH lane does after signalling the remote command dead (it wraps ctx.Err()).
type cancellingActuator struct{ execs int }

func (a *cancellingActuator) Capability() string { return "test" }
func (a *cancellingActuator) ReadOnly() bool     { return false }
func (a *cancellingActuator) Exec(_ context.Context, _ []string, _ []byte) (actuation.Result, error) {
	a.execs++
	return actuation.Result{}, fmt.Errorf("ssh: remote command cancelled: SIGTERM sent to web01:22 before closing the transport: %w", context.DeadlineExceeded)
}

// TG-80 P1-4: a cancellation that lands mid-effect is a CANCELLED terminal — refused (nothing completed),
// not executed, machine-readable via Outcome.Cancelled, and its reason starts with the stable
// RefusalCancelled token rather than the generic "execute failed" every other leaf error gets.
// KILLING MUTATION: drop the errors.Is branch at the execute site — Cancelled stays false and the reason
// reads "execute failed", and this test names both.
func TestTG80CancelledEffectIsItsOwnTerminal(t *testing.T) {
	act := &cancellingActuator{}
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger())
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if act.execs != 1 {
		t.Fatalf("the effect must have been attempted exactly once, got %d", act.execs)
	}
	if out.Executed {
		t.Fatal("a cancelled effect must not read as executed")
	}
	if !out.Refused || !out.Cancelled {
		t.Fatalf("want Refused+Cancelled, got %+v", out)
	}
	if !strings.HasPrefix(out.Reason, RefusalCancelled) {
		t.Fatalf("reason must start with the stable token %q, got %q", RefusalCancelled, out.Reason)
	}
	if strings.Contains(out.Reason, "execute failed") {
		t.Fatalf("a cancellation must not be filed as a generic execute failure: %q", out.Reason)
	}
}

// A non-cancellation leaf error keeps the generic shape — the new branch must not widen into every error.
func TestTG80PlainExecErrorIsNotCancelled(t *testing.T) {
	act := &erroringActuator{}
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger())
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if out.Cancelled || !strings.Contains(out.Reason, "execute failed") {
		t.Fatalf("a plain leaf error must stay a generic execute failure, got %+v", out)
	}
}

type erroringActuator struct{}

func (a *erroringActuator) Capability() string { return "test" }
func (a *erroringActuator) ReadOnly() bool     { return false }
func (a *erroringActuator) Exec(_ context.Context, _ []string, _ []byte) (actuation.Result, error) {
	return actuation.Result{}, errors.New("ssh: handshake with web01:22 refused: host key mismatch")
}
