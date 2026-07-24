package actuate

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
)

// fakeDecider is a scripted PolicyDecider: it returns a fixed verdict and records the mode it was handed, so a
// test can assert the interceptor consults Decide before it actuates and threads the active mode into the input.
type fakeDecider struct {
	verdict  policy.Verdict
	gotMode  policy.Mode
	gotOp    string
	calls    int
	failWith error
}

func (d *fakeDecider) Decide(_ context.Context, in policy.EvalInput) (policy.PolicyDecision, error) {
	d.calls++
	d.gotMode = in.Mode
	d.gotOp = in.OpClass
	if d.failWith != nil {
		return policy.PolicyDecision{}, d.failWith
	}
	return policy.NewPolicyDecision(d.verdict, "rule-x", in.Band, nil, in.Mode, "scripted", policy.DecisionAudit{}), nil
}

// The policy authorize layer (spec/015 T-015-13): the interceptor consults Decide before it actuates — an
// INDEPENDENT control from the mechanical mode chokepoint. Here the chokepoint actuates (mutation ON) and the
// request carries NO recorded approval (goodRequest, Approved=false), so ONLY the policy verdict decides:
// `auto` executes; `approve` (needs a human vote, none on file) refuses; `deny` refuses. The
// approve-WITH-a-recorded-approval EXECUTES path is proven in TestPolicyApproveHonorsRecordedApproval.
func TestPolicyAuthorizeGatesExecution(t *testing.T) {
	cases := []struct {
		name     string
		verdict  policy.Verdict
		wantExec bool
	}{
		{"auto authorizes", policy.VerdictAuto, true},
		{"approve without a recorded approval refuses", policy.VerdictApprove, false},
		{"deny refuses", policy.VerdictDeny, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := safety.NewActuatingChokepoint()
			act := &fakeActuator{}
			dec := &fakeDecider{verdict: tc.verdict}
			i := NewInterceptor(cp, act, audit.NewLedger()).
				WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto })
			out, err := i.Do(context.Background(), goodRequest(t))
			if err != nil {
				t.Fatal(err)
			}
			if dec.calls != 1 {
				t.Fatalf("the interceptor must consult the policy decider exactly once, got %d", dec.calls)
			}
			if dec.gotMode != policy.ModeFullAuto || dec.gotOp != "restart-service" {
				t.Fatalf("the interceptor must thread the active mode+op-class into the policy input: mode=%v op=%q", dec.gotMode, dec.gotOp)
			}
			if tc.wantExec {
				if !out.Executed || act.execs != 1 {
					t.Fatalf("an `auto` verdict must execute: %+v execs=%d", out, act.execs)
				}
			} else {
				if out.Executed || act.execs != 0 || !contains3(out.Reason, "policy verdict") {
					t.Fatalf("a non-auto verdict must refuse before execute: %+v execs=%d", out, act.execs)
				}
			}
		})
	}
}

// A policy engine error fails CLOSED — the interceptor refuses, never executes on an unresolved authorization.
func TestPolicyEngineErrorFailsClosed(t *testing.T) {
	cp := safety.NewActuatingChokepoint()
	act := &fakeActuator{}
	dec := &fakeDecider{failWith: context.DeadlineExceeded}
	i := NewInterceptor(cp, act, audit.NewLedger()).WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto })
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if out.Executed || act.execs != 0 || !contains3(out.Reason, "policy engine error") {
		t.Fatalf("a policy engine error must fail closed (refuse, no execute): %+v execs=%d", out, act.execs)
	}
}

// The NEGATIVE CONTROL (REQ-1520/1521): the mode chokepoint is an INDEPENDENT floor beneath the policy verdict.
// Even when the policy engine resolves `auto`, an action must NOT execute while the mode is Shadow — no code
// path actuates at Shadow. This proves the two layers are distinct: the policy `auto` alone cannot actuate.
func TestModeChokepointFloorsEvenWhenPolicyAuto(t *testing.T) {
	cp := safety.NewReadOnlyChokepoint() // mode Shadow — the deployed default
	act := &fakeActuator{}
	dec := &fakeDecider{verdict: policy.VerdictAuto} // policy WOULD authorize
	i := NewInterceptor(cp, act, audit.NewLedger()).WithPolicyDecider(dec, func() policy.Mode { return policy.ModeShadow })
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if out.Executed || act.execs != 0 {
		t.Fatalf("no code path may actuate at Shadow, even with a policy `auto`: %+v execs=%d", out, act.execs)
	}
	if !contains3(out.Reason, "mutation disabled") {
		t.Fatalf("the refusal must cite the mode chokepoint (mutation disabled), got %q", out.Reason)
	}
}

// A nil decider is a documented pass-through on the policy layer — the mechanical mode chokepoint still gates
// every mutation. With mutation ON and no decider wired, a fully-admissible request executes (the mode
// chokepoint is the always-present floor; the policy layer is additive).
func TestNilDeciderIsPassThroughModeStillGates(t *testing.T) {
	// pass-through + mode ON ⇒ executes
	cpOn := safety.NewActuatingChokepoint()
	actOn := &fakeActuator{}
	iOn := NewInterceptor(cpOn, actOn, audit.NewLedger()) // no WithPolicyDecider
	if out, _ := iOn.Do(context.Background(), goodRequest(t)); !out.Executed || actOn.execs != 1 {
		t.Fatalf("nil decider + mode ON must execute (mode chokepoint is the floor): %+v execs=%d", out, actOn.execs)
	}
	// pass-through + mode Shadow ⇒ refuses at the mode chokepoint
	cpOff := safety.NewReadOnlyChokepoint()
	actOff := &fakeActuator{}
	iOff := NewInterceptor(cpOff, actOff, audit.NewLedger())
	if out, _ := iOff.Do(context.Background(), goodRequest(t)); out.Executed || actOff.execs != 0 {
		t.Fatalf("nil decider + mode Shadow must refuse at the mode chokepoint: %+v execs=%d", out, actOff.execs)
	}
}

// pollApprovedRequest is the canary shape: a reversible POLL_PAUSE action (restart-service) whose required
// human approval is on file — the exact request the live canary produced (operator vote 202 on
// restart-service@librespeed01) that step 4d wrongly dead-refused before this fix.
func pollApprovedRequest(t *testing.T) Request {
	return Request{
		Manifest: pollReversibleManifest(t), Gated: true, Argv: []string{"systemctl", "restart", "nginx"},
		Evidence: boundEvidence(), Observe: noObserved, Approved: true, Band: safety.BandPollPause,
	}
}

// The graduation-deadlock fix (spec/015 REQ-1506/1514): an `approve` verdict is "route to a human vote", so
// once that vote is recorded the interceptor PROCEEDS — this is how an ungraduated op-class earns its clean
// runs toward `auto`. Before the fix, step 4d refused EVERY non-`auto` verdict, so an unseen class (which
// always resolves to `approve`) could never execute its first human-approved run and the ladder dead-locked.
func TestPolicyApproveHonorsRecordedApproval(t *testing.T) {
	// (a) approve verdict + a recorded human approval ⇒ EXECUTES (the class accrues its clean run).
	t.Run("approve plus recorded approval executes", func(t *testing.T) {
		act := &fakeActuator{}
		dec := &fakeDecider{verdict: policy.VerdictApprove}
		i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
			WithPolicyDecider(dec, func() policy.Mode { return policy.ModeSemiAuto })
		out, err := i.Do(context.Background(), pollApprovedRequest(t))
		if err != nil {
			t.Fatal(err)
		}
		if !out.Executed || act.execs != 1 {
			t.Fatalf("an `approve` verdict with a recorded human approval must execute: %+v execs=%d", out, act.execs)
		}
	})

	// (b) approve verdict WITHOUT a recorded approval ⇒ REFUSES at 4d (fail closed). Use an AUTO band so the
	//     admission gate at 1b does NOT itself refuse — isolating step 4d's own approval floor.
	t.Run("approve without an approval refuses at 4d", func(t *testing.T) {
		act := &fakeActuator{}
		dec := &fakeDecider{verdict: policy.VerdictApprove}
		i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
			WithPolicyDecider(dec, func() policy.Mode { return policy.ModeSemiAuto })
		r := goodRequest(t) // AUTO band, Approved=false
		out, err := i.Do(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		if out.Executed || act.execs != 0 || !contains3(out.Reason, "needs a human approval") {
			t.Fatalf("an `approve` verdict with NO recorded approval must refuse at 4d: %+v execs=%d", out, act.execs)
		}
	})

	// (c) deny verdict ⇒ REFUSES even WITH a recorded approval (an approval can never lift a deny).
	t.Run("deny refuses even when approved", func(t *testing.T) {
		act := &fakeActuator{}
		dec := &fakeDecider{verdict: policy.VerdictDeny}
		i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
			WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto })
		out, err := i.Do(context.Background(), pollApprovedRequest(t)) // Approved=true
		if err != nil {
			t.Fatal(err)
		}
		if out.Executed || act.execs != 0 || !contains3(out.Reason, "deny") {
			t.Fatalf("a `deny` verdict must refuse even with a recorded approval: %+v execs=%d", out, act.execs)
		}
	})

	// (d) the constitutional never-auto floor (step 2) STILL refuses an irreversible/floor-class op even when a
	//     human approved it AND the policy would resolve `auto` — proving the fix opens no floor bypass. The
	//     refusal is at the adapter floor BEFORE 4d, so the decider is never even consulted.
	t.Run("never-auto floor refuses an irreversible op even if approved and auto", func(t *testing.T) {
		act := &fakeActuator{}
		dec := &fakeDecider{verdict: policy.VerdictAuto} // policy WOULD authorize
		i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
			WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto })
		req := Request{
			Manifest: floorManifest(t), Gated: true, Argv: []string{"dropdb", "x"},
			Evidence: boundEvidence(), Observe: noObserved, Approved: true, Band: safety.BandAuto, // irreversible dropdb, fresh AUTO + human-approved
		}
		out, err := i.Do(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if out.Executed || act.execs != 0 || !contains3(out.Reason, "never-auto floor") {
			t.Fatalf("an irreversible floor-class op must be refused even when approved+auto: %+v execs=%d", out, act.execs)
		}
		if dec.calls != 0 {
			t.Fatalf("the never-auto floor (step 2) must refuse BEFORE the policy decider is consulted, got calls=%d", dec.calls)
		}
	})
}

// bandComposingDecider models the REAL policy engine's band composition (spec/015 ComposeBand under
// BandRespect, REQ-1509): it takes a per-op-class BASE verdict — `auto` for a GRADUATED class — and composes
// it most-restrictive-first against the safety band it was handed in EvalInput.Band. This is the exact
// mechanism by which a POLL_PAUSE band floors an otherwise-`auto` graduated class to `approve` (a human is
// required). It CAPTURES the band it received so a test can prove the interceptor threads the FRESH per-incident
// band into 4d (TG-126), not the sealed manifest's frozen first-seal band.
type bandComposingDecider struct {
	base    policy.Verdict // the per-op-class base verdict (a graduated class resolves `auto`)
	gotBand safety.Band
	calls   int
}

func (d *bandComposingDecider) Decide(_ context.Context, in policy.EvalInput) (policy.PolicyDecision, error) {
	d.calls++
	d.gotBand = in.Band
	composed, _ := policy.ComposeBand(d.base, in.Band, policy.BandRespect, false)
	return policy.NewPolicyDecision(composed, "graduated-rule", in.Band, nil, in.Mode, "band-composed", policy.DecisionAudit{}), nil
}

// TG-126 (step 4d): the policy authorization composes the FRESH per-incident band, NOT the sealed manifest's
// frozen first-seal band — so a de-noveled + graduated op-class self-heals hands-off, and a stale frozen band
// neither dead-refuses a fresh AUTO nor leaks an auto-authorization past a fresh POLL_PAUSE. Uses the REAL
// policy.ComposeBand so the composition is the production mechanism, not a stand-in.
func TestPolicyAuthorizeComposesFreshBandNotFrozenManifest(t *testing.T) {
	// (a) HANDS-OFF POSITIVE: the manifest is FROZEN POLL_PAUSE (first-sealed during graduation), but THIS
	//     incident classifies AUTO and the op-class has GRADUATED (base verdict `auto`). 4d must compose the
	//     FRESH AUTO band → `auto` and PROCEED with NO human approval. With the frozen POLL_PAUSE band it would
	//     compose to `approve` and re-block — the exact failure this fix removes.
	t.Run("fresh AUTO over a frozen POLL_PAUSE manifest, graduated class, self-heals hands-off", func(t *testing.T) {
		act := &fakeActuator{}
		dec := &bandComposingDecider{base: policy.VerdictAuto}
		i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
			WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto })
		req := Request{
			Manifest: pollReversibleManifest(t), // FROZEN POLL_PAUSE (first-seal-wins)
			Gated:    true, Argv: []string{"systemctl", "restart", "nginx"},
			Evidence: boundEvidence(), Observe: noObserved,
			Band: safety.BandAuto, Approved: false, // FRESH AUTO incident, no human vote
		}
		out, err := i.Do(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if dec.gotBand != safety.BandAuto {
			t.Fatalf("4d must evaluate the FRESH AUTO band, got %v", dec.gotBand)
		}
		if !out.Executed || act.execs != 1 {
			t.Fatalf("a fresh AUTO + graduated class must auto-authorize at 4d and execute (no re-block): %+v execs=%d", out, act.execs)
		}
	})

	// (b) MIRROR / NO STALE-AUTO LEAK: the manifest is FROZEN AUTO, but THIS incident classifies POLL_PAUSE.
	//     Approved so the request passes 1b and ISOLATES 4d. 4d must compose the FRESH POLL_PAUSE band →
	//     `approve` (a human is required), NEVER the frozen AUTO's silent `auto`; it then proceeds ONLY via the
	//     approve-honored vote path. A frozen AUTO would have composed to `auto` and needed no vote (the leak).
	t.Run("fresh POLL_PAUSE over a frozen AUTO manifest composes to approve, never a silent auto", func(t *testing.T) {
		act := &fakeActuator{}
		dec := &bandComposingDecider{base: policy.VerdictAuto}
		ledger := audit.NewLedger()
		i := NewInterceptor(safety.NewActuatingChokepoint(), act, ledger).
			WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto })
		req := Request{
			Manifest: reversibleManifest(t), // FROZEN AUTO
			Gated:    true, Argv: []string{"systemctl", "restart", "nginx"},
			Evidence: boundEvidence(), Observe: noObserved,
			Band: safety.BandPollPause, Approved: true, // FRESH POLL_PAUSE; approved so 1b admits and 4d is isolated
		}
		out, err := i.Do(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if dec.gotBand != safety.BandPollPause {
			t.Fatalf("4d must evaluate the FRESH POLL_PAUSE band (not the frozen AUTO manifest band), got %v", dec.gotBand)
		}
		if !out.Executed || act.execs != 1 {
			t.Fatalf("a fresh POLL_PAUSE + recorded approval must proceed via the approve-honored vote path: %+v execs=%d", out, act.execs)
		}
		// The proof of NO stale-AUTO leak: the action executed via the approve-honored path (a recorded vote),
		// NOT a silent policy `auto`. A frozen-AUTO composition would have logged no approve-honored decision.
		sawApproveHonored := false
		for _, e := range ledger.Entries() {
			if contains3(e.Decision, "policy-approve-honored") {
				sawApproveHonored = true
			}
		}
		if !sawApproveHonored {
			t.Fatal("the fresh POLL_PAUSE must force the approve-honored vote path at 4d — a frozen AUTO would have silently auto-authorized (stale-AUTO leak)")
		}
	})

	// (c) the SAME fresh POLL_PAUSE over a frozen AUTO manifest but with NO recorded approval is REFUSED and
	//     NOTHING executes — the fresh POLL_PAUSE never composes to a silent auto at 4d, and the 1b admission
	//     stops it upstream. No stale-AUTO leak at either gate.
	t.Run("fresh POLL_PAUSE over a frozen AUTO manifest with no approval is refused", func(t *testing.T) {
		act := &fakeActuator{}
		dec := &bandComposingDecider{base: policy.VerdictAuto}
		i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
			WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto })
		req := Request{
			Manifest: reversibleManifest(t), // FROZEN AUTO
			Gated:    true, Argv: []string{"systemctl", "restart", "nginx"},
			Evidence: boundEvidence(), Observe: noObserved,
			Band: safety.BandPollPause, Approved: false, // FRESH POLL_PAUSE, no vote
		}
		out, err := i.Do(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if !out.Refused || out.Executed || act.execs != 0 {
			t.Fatalf("a fresh POLL_PAUSE with no approval must be refused (no stale-AUTO leak): %+v execs=%d", out, act.execs)
		}
	})

	// (d) FAIL CLOSED at 4d: an ABSENT/zero fresh band over a frozen AUTO manifest is BandPollPause by design, so
	//     4d composes `approve`; with no approval it is refused. A missing band never auto-authorizes at 4d.
	t.Run("absent fresh band fails closed at 4d", func(t *testing.T) {
		act := &fakeActuator{}
		dec := &bandComposingDecider{base: policy.VerdictAuto}
		i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
			WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto })
		req := Request{
			Manifest: reversibleManifest(t), // FROZEN AUTO
			Gated:    true, Argv: []string{"systemctl", "restart", "nginx"},
			Evidence: boundEvidence(), Observe: noObserved,
			// Band omitted ⇒ zero ⇒ BandPollPause; no approval
		}
		out, err := i.Do(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if !out.Refused || out.Executed || act.execs != 0 {
			t.Fatalf("an absent fresh band must fail closed (refuse), never auto-authorize at 4d: %+v execs=%d", out, act.execs)
		}
	})
}
