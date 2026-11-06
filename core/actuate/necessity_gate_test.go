package actuate

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// necessity_gate_test.go — TG-166(b). THE DEFECT UNDER TEST: the chain proved an action SAFE and never asked
// whether it was still NECESSARY. Twelve gates checked reversibility, allowlisting, evidence binding, the
// committed prediction, the territory, the policy verdict, the mode, the host and the baseline — and not one
// of them re-observed the FAULT. The evidence that justified the mutation was captured during the
// investigation, minutes and a model round-trip earlier; between then and the effect the unit can be
// restarted by a human, by config management, or by systemd's own Restart= policy, and the alert can clear.
// The chain fired anyway, because "was true when we looked" was all it had ever checked.
//
// The gate is a necessity FALSIFIER, never a prover: present=false withdraws the licence, present=true means
// only "not refuted". These oracles hold it to exactly that, in both directions — a gate that refused
// everything would be just as broken as one that refused nothing, and the last two cases are the controls
// that keep it honest.

// clearedProbe reports the fault as GONE (present=false, ok=true) — the target healed between the
// investigation that justified this mutation and the effect.
func clearedProbe(context.Context) (bool, bool) { return false, true }

// unreadableProbe could not perform the re-check at all (ok=false) — a monitoring read error, which is not
// evidence of a healthy estate.
func unreadableProbe(context.Context) (bool, bool) { return false, false }

// AN ALREADY-HEALED TARGET IS NOT MUTATED. This is the whole ticket half: every other gate says yes, the box
// is fine, and the chain must not restart it.
//
// KILLING MUTATION (executed 2026-08-04): in interceptor.go gate 4i, delete the
// `if !stillFaulted { return refuseGate("necessity", …) }` branch — keeping the probe call and the `ok`
// handling, i.e. the realistic regression where the answer is read and then ignored. This test then FAILS with
//
//	"a target whose fault had ALREADY cleared was restarted anyway (leaf reached 1 time). The chain proved
//	 the action safe and never asked whether it was still needed — it dropped live connections on a healthy
//	 box and credited restart-service with a clean run for a non-event."
//
// Restored → green.
func TestAnAlreadyHealedTargetIsNotMutated(t *testing.T) {
	act := &fakeActuator{}
	i := wired(safety.NewActuatingChokepoint(), act)
	r := goodRequest(t)
	r.StillFaulted = clearedProbe

	out, err := i.Do(context.Background(), r)
	if err != nil {
		t.Fatalf("failed loud instead of refusing: %v", err)
	}
	if act.execs != 0 {
		t.Fatalf("a target whose fault had ALREADY cleared was restarted anyway (leaf reached %d time). The chain "+
			"proved the action safe and never asked whether it was still needed — it dropped live connections on "+
			"a healthy box and credited restart-service with a clean run for a non-event.", act.execs)
	}
	if !out.Refused || out.Executed {
		t.Fatalf("the refusal must be recorded as one: %+v", out)
	}
	// The refusal has to be legible: an operator seeing it must know the heal was skipped because it was no
	// longer needed, not because something was broken.
	if !strings.Contains(out.Reason, "NO LONGER NECESSARY") || !strings.Contains(out.Reason, "web01") {
		t.Fatalf("the refusal must say plainly that the action was unnecessary and name the target, got %q", out.Reason)
	}
}

// AN UNREADABLE PROBE IS NOT A CLEAR — and it is not a pass either. The gate fails CLOSED on both edges, the
// same discipline gate 4c (nil observer) and the baseline gate already apply: we do not mutate what we cannot
// adjudicate. Flattening ok into the boolean would make a monitoring outage read as either "healthy, skip" or
// "faulted, proceed", and the second is an unconditional pass wearing a check's clothes.
//
// KILLING MUTATION (executed 2026-08-04): in interceptor.go gate 4i, drop the `ok` result —
// `stillFaulted, _ := r.StillFaulted(ctx)`. Because the production probe returns (false,false) on a read
// error, the discarded ok then turns a monitoring outage into a "cleared" refusal rather than an
// "unre-observable" one; inverting the probe to (true,false) turns it into an unconditional EXECUTE. This
// test pins the distinct reason so neither collapse is silent, and it then FAILS with
//
//	"an unreadable necessity probe was reported as %q; a read error must refuse with its OWN reason, not be
//	 laundered into a clear (or, with the probe returning true, into an execution on an unobservable estate)."
//
// Restored → green.
func TestAnUnreadableNecessityProbeRefusesWithItsOwnReason(t *testing.T) {
	act := &fakeActuator{}
	i := wired(safety.NewActuatingChokepoint(), act)
	r := goodRequest(t)
	r.StillFaulted = unreadableProbe

	out, err := i.Do(context.Background(), r)
	if err != nil {
		t.Fatalf("failed loud instead of refusing: %v", err)
	}
	if act.execs != 0 || out.Executed {
		t.Fatalf("an unobservable estate must not be mutated: %+v execs=%d", out, act.execs)
	}
	if !strings.Contains(out.Reason, "could not be re-observed") {
		t.Fatalf("an unreadable necessity probe was reported as %q; a read error must refuse with its OWN reason, "+
			"not be laundered into a clear (or, with the probe returning true, into an execution on an "+
			"unobservable estate).", out.Reason)
	}
}

// AN UNWIRED CHECK IS NOT A PASSED CHECK. This is the optional-control failure mode the codebase keeps
// hitting: a gate that is present in the design, absent in the deployment, and invisible in both. Gate 4c
// made the same call for a nil post-execution observer, and this follows it.
//
// KILLING MUTATION (executed 2026-08-04): in interceptor.go gate 4i, replace the nil-seam refusal with the
// tempting backwards-compatible form — `if r.StillFaulted != nil { …checks… }` — so an unwired probe silently
// passes. This test then FAILS with
//
//	"a request with NO necessity re-check wired executed anyway. An optional necessity gate is a gate that is
//	 absent in every deployment that forgot to wire it, and nothing in the boot or the ledger would say so."
//
// Restored → green.
func TestAnUnwiredNecessityCheckRefusesRatherThanPasses(t *testing.T) {
	act := &fakeActuator{}
	i := wired(safety.NewActuatingChokepoint(), act)
	r := goodRequest(t)
	r.StillFaulted = nil // the seam was never wired

	out, err := i.Do(context.Background(), r)
	if err != nil {
		t.Fatalf("failed loud instead of refusing: %v", err)
	}
	if act.execs != 0 || out.Executed {
		t.Fatalf("a request with NO necessity re-check wired executed anyway. An optional necessity gate is a "+
			"gate that is absent in every deployment that forgot to wire it, and nothing in the boot or the "+
			"ledger would say so. (%+v execs=%d)", out, act.execs)
	}
	if !strings.Contains(out.Reason, "no execute-time fault re-check wired") {
		t.Fatalf("the refusal must name the missing control, got %q", out.Reason)
	}
}

// THE CONTROL AGAINST OVER-REFUSAL. A gate that refused every actuation would satisfy every test above and
// destroy the product: TG would stop healing anything. A still-faulted target must still be healed, and the
// verdict must still be computed — the necessity gate is a pre-condition, not a new verdict authority.
func TestAStillFaultedTargetStillHeals(t *testing.T) {
	act := &fakeActuator{}
	i := wired(safety.NewActuatingChokepoint(), act)
	out, err := i.Do(context.Background(), goodRequest(t)) // goodRequest carries the still-faulted probe
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !out.Executed || act.execs != 1 {
		t.Fatalf("a target whose fault is STILL present must still be healed — a necessity gate that refuses "+
			"everything is an outage, not a control: %+v execs=%d", out, act.execs)
	}
	if out.Verdict != safety.VerdictMatch {
		t.Fatalf("the post-execution verdict must still be computed by the verifier, got %q", out.Verdict)
	}
}

// ORDERING: the re-check must happen at the LAST pre-effect instant — after the baseline reads and
// immediately before the effect — not somewhere up in admission. Its whole value is that T_recheck is seconds
// rather than minutes before T_execute; a check moved earlier in the chain re-introduces exactly the staleness
// it exists to remove.
//
// KILLING MUTATION (executed 2026-08-04): move gate 4i above the baseline gate (immediately after the
// mode-chokepoint gate 4f). This test then FAILS with
//
//	"the necessity probe ran BEFORE the pre-execution baseline read (order: [necessity baseline exec]).
//	 Checking necessity earlier in the chain re-introduces the staleness the gate exists to remove."
//
// Restored → green.
func TestNecessityIsRecheckedAtTheLastPreEffectInstant(t *testing.T) {
	act := &fakeActuator{}
	i := wired(safety.NewActuatingChokepoint(), act)

	var order []string
	r := goodRequest(t)
	r.Observe = func(context.Context) ([]verify.ObservedAlert, bool) {
		order = append(order, "baseline-or-post")
		return []verify.ObservedAlert{}, true
	}
	r.StillFaulted = func(context.Context) (bool, bool) {
		order = append(order, "necessity")
		return true, true
	}
	origExec := act.execs
	if _, err := i.Do(context.Background(), r); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if act.execs != origExec+1 {
		t.Fatal("precondition: the action must have executed for this ordering oracle to say anything")
	}
	// VACUITY FLOOR: this oracle inspects a recorded call order. An empty order means neither seam was
	// reached and every index assertion below would pass over nothing.
	if len(order) < 3 {
		t.Fatalf("the ordering oracle recorded %v — fewer calls than the chain must make (pre-observe, "+
			"necessity, post-observe); it would pass vacuously", order)
	}
	if order[0] != "baseline-or-post" || order[1] != "necessity" {
		t.Fatalf("the necessity probe ran BEFORE the pre-execution baseline read (order: %v). Checking necessity "+
			"earlier in the chain re-introduces the staleness the gate exists to remove.", order)
	}
}
