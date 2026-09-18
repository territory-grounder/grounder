package hostdiag

// ORACLES FOR THE HOSTDIAG YIELD REPORT (TG-271).
//
// THE DEFECT THESE EXIST FOR, measured in production on 2026-08-03: this lane failed on 100% of calls for
// weeks and nothing anywhere said so. The tools were registered, the boot log announced "registered 4
// read-only host-diagnostics tools across 1 access rule(s)", the module was configured — and every read
// came back "(<host> was unreachable or the read errored)" because TG_HOSTDIAG_KNOWN_HOSTS covered 16 of
// the 38 estate hosts TG had alerted on. Host-key verification fails CLOSED.
//
// Why no existing control caught it: the manifest asks "was this seam BOUND?" and the answer was yes. Any
// check counting invocations sees a healthy lane, because the failure path RETURNS — it returns a sentinel
// string, which is a perfectly good return value. Only the produced/attempted PAIR separates a quiet
// estate from a blind agent, and that pair did not exist.

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// unresolvable is the credential-refusal path: no covering rule, so the host is not investigable at all.
type unresolvable struct{}

func (unresolvable) Resolve(_ context.Context, _ credential.Target) (credential.Bundle, error) {
	return credential.Bundle{}, credential.ErrUnresolved
}

// failRunner is the production failure this seam exists to report: every command errors, exactly as a
// host missing from known_hosts behaves (verification refuses before anything runs).
type failRunner struct{}

func (failRunner) Run(_ context.Context, _ syslogng.Server, _ []string) (syslogng.RunResult, error) {
	return syslogng.RunResult{}, errors.New("ssh: handshake failed: knownhosts: key is unknown")
}

// okRunner returns real-looking output.
type okRunner struct{}

func (okRunner) Run(_ context.Context, _ syslogng.Server, _ []string) (syslogng.RunResult, error) {
	return syslogng.RunResult{Stdout: []byte("● nginx.service loaded failed failed"), ExitCode: 0}, nil
}

func invokeOne(t *testing.T, runner syslogng.Runner, res IdentityResolver) (attempted, produced int) {
	t.Helper()
	accs := ParseAccess("dc1|dc1*|root|file:/dev/null")
	tools := NewTools(accs, runner, res,
		WithYield(func(p bool) {
			attempted++
			if p {
				produced++
			}
		}))
	if len(tools) == 0 {
		t.Fatal("no tools built")
	}
	if _, err := tools[0].Invoke(context.Background(), map[string]string{"host": "dc1pve01"}); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	return
}

// KILLING MUTATION: report produced=true unconditionally, or report only on the success path. RED — an
// all-failing lane would read as flowing, which is the state production was in for weeks.
func TestAFailingReadIsAttemptedButNotProduced(t *testing.T) {
	attempted, produced := invokeOne(t, failRunner{}, accessResolver{accs: ParseAccess("dc1|dc1*|root|file:/dev/null")})
	if attempted != 1 {
		t.Fatalf("attempted=%d, want 1 — a read that failed is still a read that was ATTEMPTED, and a "+
			"register that misses it under-reports exactly when the lane is least usable", attempted)
	}
	if produced != 0 {
		t.Fatalf("produced=%d, want 0 — the read returned the unreachable sentinel and no host output, "+
			"so counting it as produced makes a blind agent indistinguishable from a working one", produced)
	}
}

// The control: a working read MUST count as produced, or the alarm fires forever and gets ignored.
func TestAWorkingReadCountsAsProduced(t *testing.T) {
	attempted, produced := invokeOne(t, okRunner{}, accessResolver{accs: ParseAccess("dc1|dc1*|root|file:/dev/null")})
	if attempted != 1 || produced != 1 {
		t.Fatalf("attempted=%d produced=%d, want 1/1 — a read that returned host output must count, or "+
			"the seam alarms on a healthy lane and the alarm becomes noise", attempted, produced)
	}
}

// KILLING MUTATION: report only from the success path (drop the defer). RED — a credential refusal exits
// before SSH is ever attempted, and an unresolvable credential leaves the agent exactly as blind as a
// failed handshake. Missing it biases the ratio toward health.
func TestACredentialRefusalStillCountsAsAnAttemptedRead(t *testing.T) {
	attempted, produced := invokeOne(t, okRunner{}, unresolvable{})
	if attempted != 1 {
		t.Fatalf("attempted=%d, want 1 — a read refused at credential resolution never reaches SSH, but "+
			"the agent is just as unable to see the host", attempted)
	}
	if produced != 0 {
		t.Fatalf("produced=%d, want 0", produced)
	}
}

// A MALFORMED host argument is counted too, and that is deliberate rather than an oversight: the read was
// attempted and produced nothing, and a register that quietly drops the cases it dislikes is a register
// that learns to under-report. The operator's distinction between "the model called the tool wrong" and
// "the estate is unreachable" lives in the worker log, which names the refusal.
//
// KILLING MUTATION: return before the deferred report on the validate-host path. RED.
func TestAMalformedHostIsStillCountedAsAnAttemptedRead(t *testing.T) {
	attempted := 0
	produced := 0
	tools := NewTools(ParseAccess("dc1|dc1*|root|file:/dev/null"), okRunner{},
		accessResolver{accs: ParseAccess("dc1|dc1*|root|file:/dev/null")},
		WithYield(func(p bool) {
			attempted++
			if p {
				produced++
			}
		}))
	// "not a host!" fails validateHost, so Invoke returns at the FIRST early exit.
	if _, err := tools[0].Invoke(context.Background(), map[string]string{"host": "not a host!"}); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if attempted != 1 {
		t.Fatalf("attempted=%d, want 1 — the earliest refusal path escaped the observer, so any exit added "+
			"above it in future would silently stop being counted", attempted)
	}
	if produced != 0 {
		t.Fatalf("produced=%d, want 0 — nothing was read from any host", produced)
	}
}

// A nil observer must be a no-op — every existing caller passes none.
func TestNoObserverIsANoOpNotAPanic(t *testing.T) {
	tools := NewTools(ParseAccess("dc1|dc1*|root|file:/dev/null"), failRunner{}, accessResolver{accs: ParseAccess("dc1|dc1*|root|file:/dev/null")})
	if len(tools) == 0 {
		t.Fatal("no tools built")
	}
	if _, err := tools[0].Invoke(context.Background(), map[string]string{"host": "dc1pve01"}); err != nil {
		t.Fatalf("invoke with no observer: %v", err)
	}
}
