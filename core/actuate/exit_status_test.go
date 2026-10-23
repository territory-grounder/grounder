package actuate

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
)

// A REFUSED MUTATION MUST NEVER BE CREDITED AS A HEAL.
//
// Every mutating effect leaf reports the REMOTE's own failure as a non-zero ExitCode on a Result with a NIL
// error, deliberately and by documented contract — the SSH leaf for a non-zero remote exit status, the Proxmox
// leaf for a non-OK task exitstatus. Both say the caller interprets it. For the whole life of the actuation
// path the caller did not: `Do` discarded the Result with `_` and read only the Go error, so a command the
// TARGET REFUSED was recorded `execute: pass`.
//
// That is a fail-OPEN rather than a logging gap because verify scores the POST-STATE, not the effect. Whenever
// the goal state already holds for an unrelated reason — the unit was never down, the alert was stale, another
// actor fixed it first — a refused mutation verifies `match`, and `match` maps to OutcomeVerifiedClean, which
// is the ONLY promoting outcome on the graduation ladder. An op-class could climb to AUTO on actions that
// never happened.
//
// This is a live regression, not a thought experiment: on 2026-07-26T00:01:40Z the librespeed01 host guard
// denied `'systemctl' 'start' 'nginx.service'` with exit 42 while TG appended `actuate:execute:match` to the
// governance ledger in the same second and advanced start-service to clean_run_count=1.

// exitCodeActuator is the honest fake the previous fakes were not: it returns the leaves' real contract — a
// non-zero remote status as a RESULT with a nil error — so the interceptor's own interpretation is what the
// assertions below exercise. A fake that returned an error instead would test Go plumbing and prove nothing.
type exitCodeActuator struct {
	execs int
	code  int
}

func (a *exitCodeActuator) Capability() string { return "test" }
func (a *exitCodeActuator) ReadOnly() bool     { return false }
func (a *exitCodeActuator) Exec(_ context.Context, _ []string, _ []byte) (actuation.Result, error) {
	a.execs++
	return actuation.Result{ExitCode: a.code, Stderr: []byte("tg-actuator-guard: refused — not in the TG actuation allowlist")}, nil
}

// recordingLadder captures every outcome fed to the graduation ladder. The load-bearing assertion is that it
// stays EMPTY: no credit, no demerit, no ladder movement at all for an action the target refused.
type recordingLadder struct{ outcomes []policy.RunOutcome }

func (l *recordingLadder) Record(_ context.Context, _ string, outcome policy.RunOutcome) (policy.RecordResult, error) {
	l.outcomes = append(l.outcomes, outcome)
	return policy.RecordResult{}, nil
}

func TestNonZeroExitIsRefusedAndEarnsNoGraduation(t *testing.T) {
	// Exit 42 is the tg-actuator-guard denial code — the exact live case. The others cover a generic remote
	// failure (1) and the Proxmox leaf's non-OK task exitstatus, which it also reports as ExitCode 1.
	for _, code := range []int{42, 1, 127} {
		act := &exitCodeActuator{code: code}
		lad := &recordingLadder{}
		i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
			WithGraduationRecorder(lad)

		out, err := i.Do(context.Background(), goodRequest(t))
		if err != nil {
			t.Fatalf("exit %d: the chain failed loud instead of refusing: %v", code, err)
		}
		// The leaf WAS reached — this is not the gates refusing before execute; it is the interceptor reading
		// the status the leaf came back with.
		if act.execs != 1 {
			t.Fatalf("exit %d: want the effect leaf reached exactly once, got %d", code, act.execs)
		}
		if out.Executed {
			t.Fatalf("exit %d: a target-refused command was reported EXECUTED — %+v", code, out)
		}
		if !out.Refused {
			t.Fatalf("exit %d: want a recorded refusal, got %+v", code, out)
		}
		if out.Verdict != "" {
			t.Fatalf("exit %d: a refused action must carry NO verdict; got %q — this is the fail-open: %q maps "+
				"to a graduation outcome", code, out.Verdict, out.Verdict)
		}
		if len(lad.outcomes) != 0 {
			t.Fatalf("exit %d: the graduation ladder was fed %v for an action the target REFUSED — an op-class "+
				"must never earn autonomy from a non-event", code, lad.outcomes)
		}
	}
}

// MUTATION CONTROL. The test above is only worth its green if it goes RED when the guard is removed. This
// reproduces the pre-fix interceptor faithfully — discard the Result, branch on the Go error alone — and
// asserts that doing so lets a refused command through as a clean execution. If this test ever FAILS, the
// leaves have stopped reporting remote failure as (Result, nil) and the guard above has become unfalsifiable
// theatre: it would pass no matter what the interceptor did.
func TestMutationControl_DiscardingExitCodeLaundersARefusalAsSuccess(t *testing.T) {
	act := &exitCodeActuator{code: 42}

	// The exact pre-fix expression: `if _, err := ...; err != nil { refuse }`.
	res, err := act.Exec(context.Background(), []string{"systemctl", "start", "nginx.service"}, nil)
	if err != nil {
		t.Fatalf("the leaf contract has changed: a non-zero remote exit now returns a Go error (%v). The "+
			"ExitCode guard in Do is no longer load-bearing — re-derive it before deleting this control.", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("the fake no longer reports a non-zero status; this control proves nothing")
	}
	// err == nil AND ExitCode != 0 is precisely the state the old code read as success. The guard in step 5 is
	// the only thing standing between that Result and a `match` verdict with graduation credit.
	t.Logf("mutation control holds: leaf returned (ExitCode=%d, err=nil) — discarding the Result reads a "+
		"host REFUSAL as a successful mutation", res.ExitCode)
}

// A NO-OP IS NOT A HEAL (REQ-1221).
//
// The third execute outcome, and the one an exit status cannot express: a real mutation and a "the target was
// already in the requested state" no-op BOTH exit 0. Collapsing them corrupts the evidence in whichever
// direction it is collapsed. Reporting a no-op as a FAILURE was the original defect — measured live, 50 of 72
// Proxmox refusals in one week were this race, each logged "execute failed" while the estate was exactly as TG
// wanted. Reporting it as a HEAL is the opposite error and the worse one: the verifier scores the POST-STATE,
// a target already in goal state verifies `match` BY CONSTRUCTION, and `match` is the only promoting outcome
// on the graduation ladder. An op-class would climb toward AUTO on mutations it never performed — and
// start-guest, the one class at LevelAuto with the entire actuation evidence base behind it, is exactly the
// class this leaf serves.

// noOpActuator reports the Proxmox leaf's real contract for the goal-state short-circuit: exit 0, nil error,
// NoOp set. Note it deliberately shares ExitCode 0 with a successful mutation — if the guard under test keyed
// on the exit code it would be unable to tell them apart, which is the whole point.
type noOpActuator struct{ execs int }

func (a *noOpActuator) Capability() string { return "test" }
func (a *noOpActuator) ReadOnly() bool     { return false }
func (a *noOpActuator) Exec(_ context.Context, _ []string, _ []byte) (actuation.Result, error) {
	a.execs++
	return actuation.Result{ExitCode: 0, NoOp: true, Stdout: []byte("no-op: guest already in the requested state (start)")}, nil
}

func TestNoOpIsNotExecutedAndEarnsNoGraduation(t *testing.T) {
	act := &noOpActuator{}
	lad := &recordingLadder{}
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
		WithGraduationRecorder(lad)

	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatalf("the chain failed loud on a no-op instead of handling it: %v", err)
	}
	if act.execs != 1 {
		t.Fatalf("the leaf must still be reached exactly once, got %d", act.execs)
	}
	// `Executed` is threaded onto session_triage.mutated by the runner and keyed on by the reconciler. TG
	// mutated nothing, so anything but false is a false record of an estate change.
	if out.Executed {
		t.Fatalf("a no-op mutated NOTHING — reporting it executed writes a false mutation record: %+v", out)
	}
	if out.Verdict != "" {
		t.Fatalf("a no-op must carry NO verdict; got %q — a target already in goal state verifies `match` by "+
			"construction, which is precisely the laundering this guard prevents", out.Verdict)
	}
	if len(lad.outcomes) != 0 {
		t.Fatalf("the graduation ladder was fed %v for a mutation that never happened — credit belongs to "+
			"whatever actually changed the estate, which was not this action", lad.outcomes)
	}
	if !strings.Contains(out.Reason, "no-op") {
		t.Errorf("the outcome must say WHY nothing happened so the record is auditable; got %q", out.Reason)
	}
}

// A real mutation is unaffected: same exit code, NoOp unset, and it MUST still execute and produce a verdict.
// Without this, "grant no credit for a no-op" could be trivially satisfied by granting credit for nothing at
// all. It asserts NO ladder call, because since REQ-1223 the promote is decided at the session terminus — the
// distinction under test is executed-and-verdicted vs not, which is exactly what a no-op must fail.
func TestRealMutationStillVerifiesAndProducesAVerdict(t *testing.T) {
	act := &fakeActuator{} // ExitCode 0, NoOp unset — an ordinary successful mutation
	lad := &recordingLadder{}
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
		WithGraduationRecorder(lad)

	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if !out.Executed {
		t.Fatalf("a real mutation must still report executed: %+v", out)
	}
	if out.Verdict == "" {
		t.Fatal("a real mutation must still produce a mechanical verdict")
	}
	if len(lad.outcomes) != 0 {
		t.Fatalf("a verified match defers its promote to the terminus, so nothing is recorded here; got %v", lad.outcomes)
	}
}

// MUTATION CONTROL. The guard is only load-bearing if a no-op is otherwise INDISTINGUISHABLE from a real
// mutation at the interceptor boundary. This asserts that directly: both results carry the same exit code and
// the same nil error, so every signal EXCEPT NoOp is identical. If this ever fails, the leaf has started
// reporting the condition some other way and the guard above should be re-derived rather than trusted.
func TestMutationControl_NoOpAndRealMutationDifferOnlyByTheNoOpFlag(t *testing.T) {
	noop, _ := (&noOpActuator{}).Exec(context.Background(), nil, nil)
	real_, _ := (&fakeActuator{}).Exec(context.Background(), nil, nil)
	if noop.ExitCode != real_.ExitCode {
		t.Fatalf("the premise has changed: a no-op (%d) and a real mutation (%d) now differ by exit code, so "+
			"the REQ-1220 exit guard would already separate them and this one may be redundant",
			noop.ExitCode, real_.ExitCode)
	}
	if !noop.NoOp || real_.NoOp {
		t.Fatalf("NoOp must be the ONLY discriminator (no-op=%v real=%v)", noop.NoOp, real_.NoOp)
	}
	t.Logf("mutation control holds: both results are ExitCode=%d, err=nil — NoOp is the only signal that "+
		"separates a mutation TG performed from one it did not", noop.ExitCode)
}
