package runner

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// captureSink captures the sealed manifest so a test can assert what SealRollbackActivity sealed.
type captureSink struct{ sealed *manifest.ActionManifest }

func (c *captureSink) Seal(_ context.Context, m *manifest.ActionManifest) error {
	c.sealed = m
	return nil
}

// TestRollbackArgvFor_ReversibleOnly is the REVERSIBILITY GATE — the killing test (TG-462 assertion 2). A manual
// rollback is permitted ONLY for a cleanly-reversible (low-reversible) op-class, and its compensating argv is the
// registry's declared inverse. A rollback of an irreversible/medium/vendor-critical op has NO safe inverse — its
// RollbackArgv() would fall back to a RE-RUN of the forward (re-destroy), so it MUST be refused before the argv is
// ever built.
//
// RED-CONFIRM: delete the `spec.SafetyTier != opschema.TierLowReversible` refusal in rollbackArgvFor and the
// irreversible sub-test goes RED — an irreversible op-class would then produce a (re-run) rollback argv, the exact
// unsafe case the gate exists to forbid.
func TestRollbackArgvFor_ReversibleOnly(t *testing.T) {
	// Positive: a cleanly-reversible class from the LIVE registry renders its declared inverse (start → stop).
	spec, ok := opschema.Lookup("start-service")
	if !ok {
		t.Fatal("start-service must be registered")
	}
	argv, err := rollbackArgvFor(spec, true, map[string]string{"unit": "nginx"})
	if err != nil {
		t.Fatalf("a cleanly-reversible rollback was refused: %v", err)
	}
	if want := []string{"systemctl", "stop", "nginx"}; !reflect.DeepEqual(argv, want) {
		t.Errorf("compensating argv = %v, want %v (the DECLARED inverse, never a re-run of the forward)", argv, want)
	}

	// KILLING ASSERTION: an IRREVERSIBLE op-class must be refused. The synthetic spec carries a VALID (literal)
	// argv template, so RollbackArgv WOULD succeed — the ONLY thing that refuses it is the tier check. That is
	// what makes this a discriminating RED-confirm: neutralize `spec.SafetyTier != TierLowReversible` and this
	// irreversible class produces a rollback argv (a re-run of a destructive op), turning this assertion RED.
	irr := opschema.OpClassSpec{OpClass: "prune-x", Family: opschema.FamilyDiskReclaim, SafetyTier: opschema.TierIrreversible,
		ArgvTemplate: []string{"docker", "system", "prune", "-f"}}
	if _, err := rollbackArgvFor(irr, true, map[string]string{}); err == nil {
		t.Error("an IRREVERSIBLE op-class was NOT refused — a rollback of an irreversible action has no safe inverse " +
			"(RollbackArgv would re-run the forward and re-destroy). Neutralize the tier check in rollbackArgvFor and " +
			"this assertion is what goes RED.")
	}
	// A MEDIUM-tier op-class is also refused — reversible-only is strict (fail closed). Also template-bearing, so
	// the tier check is again the sole refusal.
	med := opschema.OpClassSpec{OpClass: "reboot-x", Family: opschema.FamilyGuestLifecycle, SafetyTier: opschema.TierMedium,
		ArgvTemplate: []string{"reboot"}}
	if _, err := rollbackArgvFor(med, true, nil); err == nil {
		t.Error("a MEDIUM-tier op-class was not refused — a manual rollback is reversible-only")
	}
	// A forward that was NOT sealed reversible is refused even for a low-reversible class (the seal is authority).
	if _, err := rollbackArgvFor(spec, false, map[string]string{"unit": "nginx"}); err == nil {
		t.Error("a forward action not sealed reversible was not refused")
	}

	// KILLING ASSERTION 2 — the start-guest class (the worst-case no-op bug). start-guest is TierLowReversible
	// but declares NO rollback_template, and its op is `start`, so opschema.RollbackArgv falls through to
	// spec.Argv → the FORWARD argv `[start, <guest>]`. Rolling that back re-runs `start` on an already-started
	// guest: a silent NO-OP reported as a rollback. It MUST be refused. Uses the LIVE shipped spec.
	//
	// RED-CONFIRM: remove the declared-rollback / idempotent-verb guard in rollbackArgvFor and this goes RED —
	// start-guest would then produce the forward argv (no error), the exact silent-no-op the guard forbids.
	sg, ok := opschema.Lookup("start-guest")
	if !ok {
		t.Fatal("start-guest must be registered")
	}
	if _, err := rollbackArgvFor(sg, true, map[string]string{"guest": "librespeed01"}); err == nil {
		t.Error("start-guest (TierLowReversible, op=start, NO rollback_template) was NOT refused — its compensating " +
			"argv would fall through to the FORWARD `start`, undoing nothing while the ledger records a rollback. " +
			"Neutralize the declared-rollback/idempotent-verb guard and this is what goes RED.")
	}

	// The classes that ARE safely reversible must still pass:
	//   - restart-service: op=restart (idempotent-reconvergence), no declared template → re-run is a valid undo.
	//   - start-container: op=start BUT declares rollback_template [docker stop <container>] → a real inverse.
	if rs, ok := opschema.Lookup("restart-service"); ok {
		if _, err := rollbackArgvFor(rs, true, map[string]string{"unit": "nginx"}); err != nil {
			t.Errorf("restart-service (idempotent-reconvergence verb) must remain rollback-eligible: %v", err)
		}
	}
	if sc, ok := opschema.Lookup("start-container"); ok {
		if _, err := rollbackArgvFor(sc, true, map[string]string{"container": "mealie"}); err != nil {
			t.Errorf("start-container (declared rollback_template) must remain rollback-eligible: %v", err)
		}
	}
}

// TestBuildRollbackRequest_CarriesInverseReferenceAndGating proves the request that reaches the interceptor
// carries the TG-404 inverse reference and the human-approval posture (TG-462 assertion 1, the request half): the
// InvertsActionID names the forward action, the band is POLL_PAUSE, the argv is the COMPENSATING argv (never the
// forward), the action is gated, the human approval is recorded, and the forward execution record is BOUND
// evidence (INV-11). The inverse also carries its OWN content-addressed id, distinct from the forward it undoes.
func TestBuildRollbackRequest_CarriesInverseReferenceAndGating(t *testing.T) {
	in := RollbackInput{
		ForwardActionID: "forward-being-undone", ForwardOpClass: "start-service", ForwardOp: "start",
		ForwardTarget: "app01", ForwardParams: map[string]string{"unit": "nginx"}, ForwardReversible: true,
		RollbackExternalRef: "rollback:forward-being-undone",
	}
	inverse := inverseActionFor(in)
	m, err := manifest.New(inverse, safety.BandPollPause, "plan-hash", "")
	if err != nil {
		t.Fatalf("seal inverse manifest: %v", err)
	}
	if m.ActionID == in.ForwardActionID {
		t.Error("the inverse manifest's action_id equals the forward id — an inverse is its own execution with its " +
			"own content-addressed id; InvertsActionID is a REFERENCE, not the identity")
	}
	rollbackArgv := []string{"systemctl", "stop", "nginx"}
	probe := func(context.Context) (bool, bool) { return true, true }
	req := buildRollbackRequest(m, rollbackArgv, in, true, nil,
		func(context.Context) ([]verify.ObservedAlert, bool) { return nil, true }, nil, nil, probe)

	if req.InvertsActionID != "forward-being-undone" {
		t.Errorf("InvertsActionID = %q, want the forward id — the inverse must NAME what it undoes (TG-404)", req.InvertsActionID)
	}
	if req.Band != safety.BandPollPause {
		t.Errorf("band = %v, want POLL_PAUSE — a manual rollback is human-approved by construction", req.Band)
	}
	if !reflect.DeepEqual(req.Argv, rollbackArgv) {
		t.Errorf("argv = %v, want the COMPENSATING argv %v (never the forward argv)", req.Argv, rollbackArgv)
	}
	if !req.Gated {
		t.Error("Gated must be true for a sealed inverse (the structure gate refuses an ungated action)")
	}
	if !req.Approved {
		t.Error("Approved must reflect the recorded human vote (INV-12)")
	}
	if len(req.Evidence) == 0 || !req.Evidence[0].Bound() {
		t.Error("the FORWARD execution record must be BOUND evidence — captured, successful, recent, relevant (INV-11)")
	}
	// TG-464 gap B: the injected effect-presence probe must REACH the request — a non-nil StillFaulted is what
	// arms the necessity gate with the rollback-appropriate question instead of TG-462's deliberate nil.
	if req.StillFaulted == nil {
		t.Error("StillFaulted = nil on the built request — the injected effect-presence probe was dropped, so " +
			"every rollback would refuse at the gate's nil-seam branch (TG-462's inert posture, not TG-464's armed one)")
	}
	// And a nil closure (no reader wired) must stay nil — the interceptor's OWN nil-seam refusal is the
	// honest posture for an unwired deployment, never a fabricated probe.
	nilReq := buildRollbackRequest(m, rollbackArgv, in, true, nil,
		func(context.Context) ([]verify.ObservedAlert, bool) { return nil, true }, nil, nil, nil)
	if nilReq.StillFaulted != nil {
		t.Error("an unwired probe must reach the interceptor as nil (its nil-seam refusal), never be fabricated")
	}
}

// TestRollbackInverse_RefusedUnderShadow is TG-462 assertion 1's SAFETY half: the sealed inverse, handed to the
// REAL interceptor under a Shadow (read-only) chokepoint — the default, live posture — does NOT actuate. It is
// refused at the MODE CHOKEPOINT, proving the chokepoint governs the rollback path exactly as it governs a forward
// mutation (the recording actuator's Exec is never reached).
func TestRollbackInverse_RefusedUnderShadow(t *testing.T) {
	in := RollbackInput{
		ForwardActionID: "forward-being-undone", ForwardOpClass: "start-service", ForwardOp: "start",
		ForwardTarget: "app01", ForwardParams: map[string]string{"unit": "nginx"}, ForwardReversible: true,
		RollbackExternalRef: "rollback:forward-being-undone",
	}
	m, err := manifest.New(inverseActionFor(in), safety.BandPollPause, "plan-hash", "")
	if err != nil {
		t.Fatal(err)
	}
	// stillFaulted nil here mirrors an unwired reader: the mode chokepoint sits BEFORE the necessity gate, so
	// this test still proves the chokepoint (not an incidental gate) is what refuses under Shadow.
	req := buildRollbackRequest(m, []string{"systemctl", "stop", "nginx"}, in, true, nil,
		func(context.Context) ([]verify.ObservedAlert, bool) { return nil, true }, nil, nil, nil)

	act := &recordingActuator{}
	i := actuate.NewInterceptor(safety.NewReadOnlyChokepoint(), act, audit.NewLedger()) // mutation OFF (Shadow)
	out, err := i.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do failed loud (unwired chain): %v", err)
	}
	if out.Executed {
		t.Fatal("the rollback EXECUTED under Shadow — the mode chokepoint must make the whole rollback lane inert")
	}
	if act.execs != 0 {
		t.Fatal("the effect leaf ran under Shadow — the rollback reached an actuator it must never reach at Shadow")
	}
	if !out.Refused {
		t.Fatalf("expected a refusal under Shadow, got %+v", out)
	}
	// The refusal must be the MODE CHOKEPOINT, not an incidental earlier gate — that is what proves the chokepoint
	// (not e.g. the evidence or territory gate) is what governs the rollback.
	if !strings.Contains(out.Reason, "read-only") && !strings.Contains(out.Reason, "mutation disabled") {
		t.Errorf("Shadow refusal reason = %q; want the mode chokepoint (\"mutation disabled (read-only)\") — the rollback "+
			"must traverse the chain to the chokepoint, not be turned away by an earlier gate", out.Reason)
	}
}

// TestSealRollbackActivity_SealsDistinctInverseAndRefusesUnknown proves the seal step (TG-462 assertion 1's seal
// half): a cleanly-reversible forward yields a durably-sealed inverse with its OWN action_id at POLL_PAUSE; an
// unregistered op-class is refused (nothing sealed, nothing to approve).
func TestSealRollbackActivity_SealsDistinctInverseAndRefusesUnknown(t *testing.T) {
	in := RollbackInput{
		ForwardActionID: "forward-being-undone", ForwardOpClass: "start-service", ForwardOp: "start",
		ForwardTarget: "app01", ForwardParams: map[string]string{"unit": "nginx"}, ForwardReversible: true,
		RollbackExternalRef: "rollback:forward-being-undone",
	}
	sink := &captureSink{}
	a := &Activities{D: Deps{ManifestSink: sink}}
	res, err := a.SealRollbackActivity(context.Background(), in)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !res.Sealed {
		t.Fatalf("a cleanly-reversible forward was not sealed: %q", res.Reason)
	}
	if res.InverseActionID == in.ForwardActionID {
		t.Error("the sealed inverse must have its own action_id, distinct from the forward")
	}
	if sink.sealed == nil {
		t.Fatal("no manifest reached the durable sink — the authorization must be durable before approval")
	}
	if sink.sealed.Band != safety.BandPollPause {
		t.Errorf("sealed band = %v, want POLL_PAUSE", sink.sealed.Band)
	}
	if sink.sealed.ActionID != res.InverseActionID {
		t.Error("the sealed manifest's id disagrees with the reported inverse id")
	}

	// An UNREGISTERED forward op-class is refused (nothing to roll back).
	unknown := in
	unknown.ForwardOpClass = "no-such-op-class"
	res2, err := a.SealRollbackActivity(context.Background(), unknown)
	if err != nil {
		t.Fatalf("seal(unknown): %v", err)
	}
	if res2.Sealed {
		t.Error("an unregistered op-class was sealed for rollback — it has no execution path and must be refused")
	}
}

// ---- TG-464 gap B: the rollback-appropriate necessity probe (effect-presence) ----

// TestForwardEffectPresentProbe pins the closure's three-way contract over the live alert reader. The
// rollback lane licenses the OPPOSITE reading of the surface the forward probe reads: QUIET target ⇒ the
// forward fix is holding ⇒ effect present ⇒ proceed; ALERTING target ⇒ the effect lapsed or the host is
// mid-incident ⇒ refuse; unreadable ⇒ fail closed (a monitoring outage licenses nothing, TG-182).
func TestForwardEffectPresentProbe(t *testing.T) {
	quiet := forwardEffectPresent(func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return []verify.ObservedAlert{{Host: "other01", Rule: "DiskFull", Site: "nl"}}, true // alerts only ELSEWHERE
	}, "app01", "nl")
	if present, ok := quiet(context.Background()); !present || !ok {
		t.Errorf("a quiet target (alerts only on other hosts) must read effect-PRESENT (true,true), got (%v,%v)", present, ok)
	}
	alerting := forwardEffectPresent(func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return []verify.ObservedAlert{{Host: " App01 ", Rule: "NginxDown", Site: "nl"}}, true // case/space variant must still match
	}, "app01", "nl")
	if present, ok := alerting(context.Background()); present || !ok {
		t.Errorf("an actively-alerting target must read effect-ABSENT (false,true), got (%v,%v)", present, ok)
	}
	unreadable := forwardEffectPresent(func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return nil, false
	}, "app01", "nl")
	if present, ok := unreadable(context.Background()); present || ok {
		t.Errorf("an unreadable surface must fail closed (false,false) — never a quiet, never a licence — got (%v,%v)", present, ok)
	}
}

// TestGuestEffectPresentProbe pins the guest-power lane's three-way contract over the guest_liveness reader —
// the lane (TG-464/TG-461) that grounds a guest rollback on a REAL target-state read the ACTUATE plane can serve,
// where the alert surface forwardEffectPresent reads is 403-scoped-out (the topology token has no alert-read).
// For a start-guest forward "is the effect present?" == "is the guest still RUNNING?".
//
// KILLING MUTATIONS: (1) flip `return running, true` to `return !running, true` and the RUNNING/STOPPED rows go
// RED (the probe would fire the inverse against an already-stopped guest, or refuse the healthy case a rollback
// is FOR); (2) drop the `if !ok` fail-closed guard and the unestablished row goes RED (a stale projection would
// become a licence — unknown treated as still-running, exactly the TG-378 hazard).
func TestGuestEffectPresentProbe(t *testing.T) {
	running := guestEffectPresent(func(context.Context, string) (bool, string, bool) {
		return true, "guest_liveness@nl fresh", true
	}, "dc1app01")
	if present, ok := running(context.Background()); !present || !ok {
		t.Errorf("a RUNNING guest must read effect-PRESENT (true,true) — the undo is meaningful, got (%v,%v)", present, ok)
	}
	stopped := guestEffectPresent(func(context.Context, string) (bool, string, bool) {
		return false, "guest_liveness@nl fresh", true
	}, "dc1app01")
	if present, ok := stopped(context.Background()); present || !ok {
		t.Errorf("a STOPPED guest must read effect-ABSENT (false,true) — nothing to undo, no blind re-stop, got (%v,%v)", present, ok)
	}
	unestablished := guestEffectPresent(func(context.Context, string) (bool, string, bool) {
		return false, "stale/never-observed/reader-error", false
	}, "dc1app01")
	if present, ok := unestablished(context.Background()); present || ok {
		t.Errorf("an unestablished state must fail closed (false,false) — unknown is not still-running (TG-378), got (%v,%v)", present, ok)
	}
}

// TestRollbackNecessityProbeRouting proves the selector routes a GUEST-power rollback (the sealed inverse declares
// RequiresRunning — a stop-guest) to the guest_liveness lane, and every other class to the live active-alert lane.
// It is the reachability guard for the implemented≠reachable class: a guest rollback whose selector silently fell
// through to the alert lane would fail closed forever on the actuate plane (TG-461), reading "done" in code while
// the live drill can only refuse. Each assertion is falsified by removing the routing condition it guards.
func TestRollbackNecessityProbeRouting(t *testing.T) {
	stopGuest, ok := opschema.Lookup("stop-guest")
	if !ok || stopGuest.RequiresTargetState != opschema.RequiresRunning {
		t.Fatalf("precondition: stop-guest must be a registered RequiresRunning class (known=%v state=%q) — the selector keys on exactly this", ok, stopGuest.RequiresTargetState)
	}
	startService, ok := opschema.Lookup("start-service")
	if !ok || startService.RequiresTargetState != "" {
		t.Fatalf("precondition: start-service must be a registered class with no state precondition (known=%v state=%q)", ok, startService.RequiresTargetState)
	}

	// The guest reader answers RUNNING; the alert reader is BROKEN (the TG-461 403). A guest-power rollback must
	// pick the guest lane ⇒ present=true. If it wrongly fell through to the alert lane, the broken read fails closed.
	guestUp := func(context.Context, string) (bool, string, bool) { return true, "gl", true }
	brokenAlert := func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, false }
	probe := rollbackNecessityProbe(stopGuest, true, "dc1app01", guestUp, nil, "", brokenAlert, "dc1app01", "nl")
	if probe == nil {
		t.Fatal("a guest-power rollback with a wired guest reader must have a probe, got nil")
	}
	if present, ok := probe(context.Background()); !present || !ok {
		t.Fatalf("the guest-power lane must answer from guest_liveness (true,true) even with the alert read broken (TG-461) — got (%v,%v); a fall-through to the alert lane is the reachability bug", present, ok)
	}

	// A non-guest reversible class with NO service reader keeps the alert lane: a QUIET alert surface ⇒ present,
	// and the guest reader (here answering DOWN) must be ignored — proving the selector does not misroute
	// non-guest ops to guest state, and that an unwired service lane changes nothing (the pre-TG-464 posture).
	guestDown := func(context.Context, string) (bool, string, bool) { return false, "gl", true }
	quietAlert := func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
	svc := rollbackNecessityProbe(startService, true, "app01", guestDown, nil, "nginx", quietAlert, "app01", "nl")
	if present, ok := svc(context.Background()); !present || !ok {
		t.Fatalf("a non-guest class must read the alert lane (quiet⇒present true,true), NOT the guest reader — got (%v,%v)", present, ok)
	}

	// The SERVICE lane (TG-464 close-out): a service-lifecycle inverse with a wired systemctl reader answers from
	// the unit's ACTUAL state even with the alert read broken — the exact split-deployment shape (TG-461's 403)
	// where the alert lane could only fail closed and the manual rollback's eligible classes were un-executable.
	activeUnit := func(context.Context, string, string) (bool, bool) { return true, true }
	svcLane := rollbackNecessityProbe(startService, true, "app01", guestDown, activeUnit, "nginx", brokenAlert, "app01", "nl")
	if svcLane == nil {
		t.Fatal("a service-lifecycle rollback with a wired service reader must have a probe, got nil")
	}
	if present, ok := svcLane(context.Background()); !present || !ok {
		t.Fatalf("the service lane must answer from systemctl is-active (true,true) even with the alert read broken (TG-461) — got (%v,%v); a fall-through to the alert lane is the same reachability bug the guest lane closed", present, ok)
	}
	// Disjointness: a guest-power rollback with BOTH readers wired must still take the guest lane — the service
	// reader must never answer for a stop-guest (family selection, not readiness, decides).
	if present, ok := rollbackNecessityProbe(stopGuest, true, "dc1app01", guestUp, func(context.Context, string, string) (bool, bool) {
		t.Error("the service reader must not be consulted for a guest-power rollback")
		return false, false
	}, "nginx", brokenAlert, "dc1app01", "nl")(context.Background()); !present || !ok {
		t.Fatalf("guest lane must win for a guest-power inverse with both readers wired, got (%v,%v)", present, ok)
	}
	// A service inverse whose sealed params carry NO unit cannot ground the read — it falls through to the
	// alert lane rather than probing an empty unit name.
	if present, ok := rollbackNecessityProbe(startService, true, "app01", nil, activeUnit, "  ", quietAlert, "app01", "nl")(context.Background()); !present || !ok {
		t.Fatalf("an empty unit must fall through to the alert lane (quiet⇒present), got (%v,%v)", present, ok)
	}

	// A guest-power rollback with NO guest reader falls back to the alert lane (coarser, but the correct posture on
	// a plane where the alert read works); with neither reader wired the probe is the nil fail-closed seam.
	if fb := rollbackNecessityProbe(stopGuest, true, "dc1app01", nil, nil, "", quietAlert, "dc1app01", "nl"); fb == nil {
		t.Error("guest-power rollback with no guest reader but a wired alert reader must fall back to the alert lane, got nil")
	}
	if p := rollbackNecessityProbe(stopGuest, true, "dc1app01", nil, nil, "", nil, "dc1app01", "nl"); p != nil {
		t.Error("guest-power rollback with NO reader of either kind must yield a nil probe (the interceptor's fail-closed seam)")
	}
	if p := rollbackNecessityProbe(startService, true, "app01", nil, nil, "", nil, "app01", "nl"); p != nil {
		t.Error("non-guest rollback with no alert reader must yield a nil probe (fail-closed seam)")
	}
}

// TestServiceEffectPresentProbe pins the service lane's three-way contract over the systemctl-is-active reader —
// the lane (TG-464 close-out) that grounds a service rollback on a REAL unit-state read the ACTUATE plane can
// serve over its own actuation identity, where the alert surface forwardEffectPresent reads is 403-scoped-out
// (TG-461). For a start-service forward "is the effect present?" == "is the unit still ACTIVE?".
//
// KILLING MUTATIONS: (1) flip `return active, true` to `return !active, true` and the ACTIVE/inactive rows go
// RED (the probe would fire the stop against an already-dead unit, or refuse the healthy case a rollback is
// FOR); (2) drop the `if !ok` fail-closed guard and the unestablished row goes RED (a guard denial or transport
// failure would become "inactive" — a refusal with the WRONG reason that masks the misconfiguration forever).
func TestServiceEffectPresentProbe(t *testing.T) {
	active := serviceEffectPresent(func(context.Context, string, string) (bool, bool) {
		return true, true
	}, "app01", "nginx")
	if present, ok := active(context.Background()); !present || !ok {
		t.Errorf("an ACTIVE unit must read effect-PRESENT (true,true) — the undo is meaningful, got (%v,%v)", present, ok)
	}
	inactive := serviceEffectPresent(func(context.Context, string, string) (bool, bool) {
		return false, true
	}, "app01", "nginx")
	if present, ok := inactive(context.Background()); present || !ok {
		t.Errorf("an inactive unit must read effect-ABSENT (false,true) — nothing to undo, no blind re-stop, got (%v,%v)", present, ok)
	}
	unestablished := serviceEffectPresent(func(context.Context, string, string) (bool, bool) {
		return false, false // transport error / host-key failure / the host guard's exit-42 denial
	}, "app01", "nginx")
	if present, ok := unestablished(context.Background()); present || ok {
		t.Errorf("an unestablished unit state must fail closed (false,false) — a denial is not 'inactive', got (%v,%v)", present, ok)
	}
}

// rollbackExecuteWith seals ONE start-service inverse and drives SealRollbackExecuteActivity through the REAL
// interceptor with mutation ON (test-only) and the supplied ClearObserve wiring — the rollback twin of
// necessity_wire_test's executeWith. Mutation ON is what carries the chain PAST the mode chokepoint to the
// necessity gate, so these tests exercise the probe itself rather than the chokepoint in front of it.
func rollbackExecuteWith(t *testing.T, clear func(context.Context, string, string) ([]verify.ObservedAlert, bool)) (ExecuteResult, *recordingActuator) {
	t.Helper()
	ctx := context.Background()
	in := RollbackInput{
		ForwardActionID: "forward-being-undone", ForwardOpClass: "start-service", ForwardOp: "start",
		ForwardTarget: "app01", ForwardParams: map[string]string{"unit": "nginx"}, ForwardReversible: true,
		ForwardSite: "nl", RollbackExternalRef: "rollback:forward-being-undone",
	}
	gate := safety.NewActuatingChokepoint()
	act := &recordingActuator{}
	sink := &fakeManifestSink{}
	a := NewActivities(Deps{
		// withPermissivePolicy for the same reason necessity_wire_test carries it: an ACTUATING posture with no
		// policy authorizer refuses at the policy gate (REQ-1207c), in FRONT of the necessity gate under test.
		Interceptor:  withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger())),
		Mutation:     gate,
		ManifestSink: sink,
		Manifests:    sink,
		PostStateObserve: func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{}, true
		},
		ClearObserve: clear,
	})
	seal, err := a.SealRollbackActivity(ctx, in)
	if err != nil || !seal.Sealed {
		t.Fatalf("seal: err=%v res=%+v", err, seal)
	}
	res, err := a.SealRollbackExecuteActivity(ctx, RollbackExecuteInput{In: in, InverseActionID: seal.InverseActionID})
	if err != nil {
		t.Fatalf("execute must be a recorded refusal or an execution, never an error: %v", err)
	}
	return res, act
}

// THE LIVE SHAPE THIS CLOSES: the operator approves undoing a start-service, and between the vote and the
// effect the started unit dies again (or the host re-enters an incident). The forward effect is gone —
// there is nothing left to undo — and before TG-464 nothing asked: the request carried StillFaulted=nil, so
// EVERY rollback refused (inert lane), and the naive fix (wiring the forward's fault-probe) would have asked
// the inverted question and refused exactly the healthy-host case a rollback is FOR.
//
// KILLING MUTATION (executed for TG-464): make forwardEffectPresent return (true,true) unconditionally and
// THIS test goes RED (the rollback executes against a re-faulted target) while the quiet-path test below
// stays green — proving the assertion discriminates the probe's answer, not the wiring around it. Restored →
// green.
func TestRollbackRefusesWhenForwardEffectAbsent(t *testing.T) {
	res, act := rollbackExecuteWith(t, func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return []verify.ObservedAlert{{Host: "app01", Rule: "NginxDown", Site: "nl"}}, true // the target is faulted AGAIN
	})
	if res.Executed || act.execs != 0 {
		t.Fatalf("a rollback whose forward effect is no longer present must NOT actuate: %+v execs=%d", res, act.execs)
	}
	if !strings.Contains(res.Note, "NO LONGER NECESSARY") {
		t.Fatalf("the refusal must come from the necessity gate's effect-presence answer, got %q — any other "+
			"reason means the probe was not consulted", res.Note)
	}
}

// THE CONTROL AGAINST OVER-REFUSAL: a quiet target — the forward fix holding, the ticket's happy path — must
// let the approved inverse EXECUTE, and execute the COMPENSATING argv. Without this case the test above is
// satisfied by TG-462's inert lane (which refuses everything).
func TestRollbackProceedsWhileForwardEffectPresent(t *testing.T) {
	res, act := rollbackExecuteWith(t, func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return nil, true // the target is QUIET — the forward fix is holding
	})
	if !res.Executed || act.execs != 1 {
		t.Fatalf("an approved rollback whose forward effect is still present must execute — a probe that "+
			"refuses everything is TG-462's inert posture wearing TG-464's name: %+v execs=%d", res, act.execs)
	}
	if !reflect.DeepEqual(act.argv, []string{"systemctl", "stop", "nginx"}) {
		t.Fatalf("the effect leaf must receive the COMPENSATING argv, got %v", act.argv)
	}
}

// A READ ERROR IS NOT A CLEAR AND NOT A LICENCE — the probe propagates (false,false) and the interceptor's
// fail-closed read-error branch refuses, exactly the forward lane's discipline (TG-182).
func TestRollbackFailsClosedWhenEffectProbeCannotRead(t *testing.T) {
	res, act := rollbackExecuteWith(t, func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return nil, false // fetch/token/HTTP failure
	})
	if res.Executed || act.execs != 0 {
		t.Fatalf("an unreadable monitoring surface must not license a rollback: %+v execs=%d", res, act.execs)
	}
	if !strings.Contains(res.Note, "could not be re-observed") {
		t.Fatalf("a read error must surface as the gate's own read-error refusal, got %q", res.Note)
	}
}

// NO READER WIRED ⇒ the seam stays nil ⇒ the interceptor's nil-seam refusal — an unwired deployment keeps
// TG-462's inert posture rather than gaining a fabricated probe.
func TestRollbackRefusesWithNoAlertReaderWired(t *testing.T) {
	res, act := rollbackExecuteWith(t, nil)
	if res.Executed || act.execs != 0 {
		t.Fatalf("a deployment with no alert reader cannot ground effect-presence and must not actuate: %+v execs=%d", res, act.execs)
	}
	if !strings.Contains(res.Note, "no execute-time fault re-check wired") {
		t.Fatalf("the refusal must name the missing control (the nil seam), got %q", res.Note)
	}
}

// rollbackServiceExecuteWith seals ONE start-service inverse and drives SealRollbackExecuteActivity through the
// REAL interceptor with the supplied ServiceActive reader and a BROKEN ClearObserve — the exact live actuate-plane
// shape (TG-461: the topology token 403s the alert read). It is the service twin of rollbackGuestExecuteWith:
// before TG-464's service lane, every one of these executions could only refuse at gate 4i.
func rollbackServiceExecuteWith(t *testing.T, serviceActive func(context.Context, string, string) (bool, bool)) (ExecuteResult, *recordingActuator) {
	t.Helper()
	ctx := context.Background()
	in := RollbackInput{
		ForwardActionID: "forward-being-undone", ForwardOpClass: "start-service", ForwardOp: "start",
		ForwardTarget: "app01", ForwardParams: map[string]string{"unit": "nginx"}, ForwardReversible: true,
		ForwardSite: "nl", RollbackExternalRef: "rollback:forward-being-undone",
	}
	gate := safety.NewActuatingChokepoint()
	act := &recordingActuator{}
	sink := &fakeManifestSink{}
	a := NewActivities(Deps{
		Interceptor:  withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger())),
		Mutation:     gate,
		ManifestSink: sink,
		Manifests:    sink,
		PostStateObserve: func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{}, true
		},
		ClearObserve: func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			return nil, false // the actuate plane's 403 (TG-461) — the lane this fix routes around
		},
		ServiceActive: serviceActive,
	})
	seal, err := a.SealRollbackActivity(ctx, in)
	if err != nil || !seal.Sealed {
		t.Fatalf("seal: err=%v res=%+v", err, seal)
	}
	res, err := a.SealRollbackExecuteActivity(ctx, RollbackExecuteInput{In: in, InverseActionID: seal.InverseActionID})
	if err != nil {
		t.Fatalf("execute must be a recorded refusal or an execution, never an error: %v", err)
	}
	return res, act
}

// THE LIVE SHAPE THIS CLOSES (TG-464's last gap): on the split deployment the actuate plane cannot read the
// LibreNMS alert surface (TG-461), so every manual service rollback — the ONLY manual-eligible classes —
// refused at gate 4i regardless of mode, vote, or estate truth. With the service lane wired, the approved
// stop-service inverse executes off the unit's ACTUAL state even though the alert read still 403s.
//
// KILLING MUTATION: remove the service lane from rollbackNecessityProbe (or its Family selection) and THIS
// test goes RED (the probe falls to the broken alert lane ⇒ refuse) while the alert-lane tests above stay
// green — proving the assertion discriminates the new routing, not the wiring around it.
func TestRollbackServiceLaneFiresDespiteBrokenAlertRead(t *testing.T) {
	res, act := rollbackServiceExecuteWith(t, func(_ context.Context, host, unit string) (bool, bool) {
		if host != "app01" || unit != "nginx" {
			t.Errorf("the probe must read the sealed inverse's own target+unit, got host=%q unit=%q", host, unit)
		}
		return true, true // the started unit is still ACTIVE — the fix is holding, the undo is meaningful
	})
	if !res.Executed || act.execs != 1 {
		t.Fatalf("an approved service rollback with the unit ACTIVE must execute off the systemctl read even "+
			"while the alert surface 403s — the split-deployment shape TG-464 closes: %+v execs=%d", res, act.execs)
	}
	if !reflect.DeepEqual(act.argv, []string{"systemctl", "stop", "nginx"}) {
		t.Fatalf("the effect leaf must receive the COMPENSATING argv, got %v", act.argv)
	}
}

// A unit that is ALREADY DOWN at execute time has nothing left to undo — the service lane answers
// (false,true) and the necessity gate refuses with its effect-lapsed reason, never a blind re-stop.
func TestRollbackServiceLaneRefusesWhenUnitAlreadyDown(t *testing.T) {
	res, act := rollbackServiceExecuteWith(t, func(context.Context, string, string) (bool, bool) {
		return false, true // read succeeded: the unit is inactive — the forward effect has lapsed
	})
	if res.Executed || act.execs != 0 {
		t.Fatalf("a rollback whose unit is already down must NOT actuate: %+v execs=%d", res, act.execs)
	}
	if !strings.Contains(res.Note, "NO LONGER NECESSARY") {
		t.Fatalf("the refusal must come from the necessity gate's effect-presence answer, got %q", res.Note)
	}
}

// THE AUTOFIRED HALF RIDES THE SAME LANE — pinned, not code-read. SealRollbackExecuteActivity is shared by
// the manual vote path and the commit-confirm AutoFired path (spec/029); the necessity-lane selection does
// not discriminate on AutoFired, so a commit-confirmed restart/reload-service inverse now also grounds on
// the unit's actual state where the actuate plane's alert read could only fail closed (TG-461). This test
// drives the AutoFired input shape (AutoFired:true + the forward's recorded ApprovedBasis) through the same
// real interceptor: the service lane must answer and the compensating argv must execute.
func TestRollbackServiceLaneServesTheAutoFiredPathToo(t *testing.T) {
	ctx := context.Background()
	in := RollbackInput{
		ForwardActionID: "forward-being-undone", ForwardOpClass: "start-service", ForwardOp: "start",
		ForwardTarget: "app01", ForwardParams: map[string]string{"unit": "nginx"}, ForwardReversible: true,
		ForwardSite: "nl", RollbackExternalRef: "rollback:forward-being-undone",
	}
	gate := safety.NewActuatingChokepoint()
	act := &recordingActuator{}
	sink := &fakeManifestSink{}
	a := NewActivities(Deps{
		Interceptor:  withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger())),
		Mutation:     gate,
		ManifestSink: sink,
		Manifests:    sink,
		PostStateObserve: func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{}, true
		},
		ClearObserve: func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			return nil, false // the actuate plane's 403 (TG-461)
		},
		ServiceActive: func(context.Context, string, string) (bool, bool) { return true, true },
	})
	seal, err := a.SealRollbackActivity(ctx, in)
	if err != nil || !seal.Sealed {
		t.Fatalf("seal: err=%v res=%+v", err, seal)
	}
	res, err := a.SealRollbackExecuteActivity(ctx, RollbackExecuteInput{
		In: in, InverseActionID: seal.InverseActionID, AutoFired: true, ApprovedBasis: true,
	})
	if err != nil {
		t.Fatalf("execute must be a recorded refusal or an execution, never an error: %v", err)
	}
	if !res.Executed || act.execs != 1 {
		t.Fatalf("an AutoFired service inverse with the unit ACTIVE must execute off the systemctl lane "+
			"(the alert read is broken): %+v execs=%d", res, act.execs)
	}
	if !reflect.DeepEqual(act.argv, []string{"systemctl", "stop", "nginx"}) {
		t.Fatalf("the effect leaf must receive the COMPENSATING argv, got %v", act.argv)
	}
}

// An unestablishable unit state — transport failure, host-key mismatch, the host guard's exit-42 denial —
// is NOT "inactive" and NOT a licence: the lane propagates (false,false) and the gate's read-error branch
// refuses, exactly the alert lane's TG-182 discipline.
func TestRollbackServiceLaneFailsClosedWhenUnitStateUnreadable(t *testing.T) {
	res, act := rollbackServiceExecuteWith(t, func(context.Context, string, string) (bool, bool) {
		return false, false
	})
	if res.Executed || act.execs != 0 {
		t.Fatalf("an unreadable unit state must not license a rollback: %+v execs=%d", res, act.execs)
	}
	if !strings.Contains(res.Note, "could not be re-observed") {
		t.Fatalf("a read failure must surface as the gate's own read-error refusal, got %q", res.Note)
	}
}

// rollbackGuestExecuteWith seals ONE start-guest inverse (a stop-guest) and drives SealRollbackExecuteActivity
// through the REAL interceptor with the supplied guest_liveness reader AND a caller-chosen ClearObserve. It is
// the guest twin of rollbackExecuteWith, and it wires a.D.Gate.GuestRunning exactly as cmd/worker/main.go does on
// BOTH planes (main.go:4983) — so the test exercises the SAME seam the deployed actuate worker runs.
func rollbackGuestExecuteWith(t *testing.T,
	guestRunning func(context.Context, string) (bool, string, bool),
	clear func(context.Context, string, string) ([]verify.ObservedAlert, bool)) (ExecuteResult, *recordingActuator) {
	t.Helper()
	ctx := context.Background()
	// A start-guest's inverse is the CLASS inverse stop-guest, which is sealed by the COMMIT-CONFIRMED lane
	// (SealCommitConfirmInverseActivity), NOT the manual seal — the manual seal refuses guest ops (no self-inverse
	// template), which is exactly why a guest rollback reaches SealRollbackExecuteActivity only via AutoFired. Seal
	// the stop-guest inverse into the durable store and fire it through the AutoFired path, as the elapsed
	// commit-confirm window does, so this drives the REAL execute wiring — a.D.Gate.GuestRunning and the
	// OpClass/Params read off a durably-RELOADED manifest — the reachability the two unit tests below cannot reach.
	inv, err := manifest.New(manifest.Action{
		Op: "rollback:start", OpClass: "stop-guest", Target: "dc1librespeed01",
		Params: map[string]string{"guest": "dc1librespeed01"}, Reversible: true,
	}, safety.BandAuto, "ph-inv", "")
	if err != nil {
		t.Fatal(err)
	}
	sink := &fakeManifestSink{}
	if err := sink.Seal(ctx, inv); err != nil {
		t.Fatal(err)
	}
	gate := safety.NewActuatingChokepoint()
	act := &recordingActuator{}
	a := NewActivities(Deps{
		Interceptor:  withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger())),
		Mutation:     gate,
		ManifestSink: sink,
		Manifests:    sink,
		PostStateObserve: func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{}, true
		},
		ClearObserve: clear,
		// The guest_liveness reader, wired as the deployed actuate worker has it (main.go:4983, not plane-conditional).
		Gate: &predict.PredictionGate{GuestRunning: guestRunning},
	})
	in := RollbackInput{
		ForwardActionID: "forward-start-guest", ForwardOpClass: "start-guest", ForwardOp: "start",
		ForwardTarget: "dc1librespeed01", ForwardParams: map[string]string{"guest": "dc1librespeed01"},
		ForwardReversible: true, ForwardSite: "nl", RollbackExternalRef: "rollback:forward-start-guest",
	}
	// AutoFired + ApprovedBasis: the commit-confirmed revert path (spec/029 T-029-3) — fired by the elapsed window,
	// carrying the forward's durable approval, not an operator vote.
	res, err := a.SealRollbackExecuteActivity(ctx, RollbackExecuteInput{
		In: in, InverseActionID: inv.ActionID, AutoFired: true, ApprovedBasis: true,
	})
	if err != nil {
		t.Fatalf("execute must be a recorded refusal or an execution, never an error: %v", err)
	}
	return res, act
}

// TestRollbackGuestLaneFiresDespiteBrokenAlertRead is the reachability proof for TG-464 past TG-461: a start-guest
// rollback must FIRE its stop-guest inverse while the guest is still running, EVEN THOUGH the actuate plane's
// active-alert read is broken — the 403 (topology token, no alert-read) that let the whole build read "done" while
// every live drill could only refuse. It wires the guest_liveness reader and a BROKEN ClearObserve and asserts the
// inverse executes: the guest lane is consulted, not the 403'd alert lane.
//
// KILLING MUTATION (executed): route the guest-power rollback to the alert lane (rollbackNecessityProbe keying on
// RequiresNotRunning) and THIS goes RED — the broken read fails closed and the inverse never fires, exactly TG-461's
// live symptom. The stopped-guest control proves it is the guest ANSWER discriminating, not the mere wiring.
func TestRollbackGuestLaneFiresDespiteBrokenAlertRead(t *testing.T) {
	brokenAlert := func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, false } // TG-461 403
	res, act := rollbackGuestExecuteWith(t,
		func(context.Context, string) (bool, string, bool) { return true, "guest_liveness@nl fresh", true }, // guest UP
		brokenAlert)
	if !res.Executed || act.execs != 1 {
		t.Fatalf("a guest rollback whose guest is still running must FIRE its inverse via the guest_liveness lane "+
			"despite the broken alert read (TG-461) — got %+v execs=%d", res, act.execs)
	}

	// CONTROL: the guest is already STOPPED between vote and effect — the forward effect has lapsed, nothing to
	// undo. The guest lane reads effect-ABSENT and the necessity gate refuses (no blind re-stop). Same broken alert
	// read, so a green here plus a green above proves the guest ANSWER drives the outcome, not the alert surface.
	stoppedRes, stoppedAct := rollbackGuestExecuteWith(t,
		func(context.Context, string) (bool, string, bool) { return false, "guest_liveness@nl fresh", true }, // guest already DOWN
		brokenAlert)
	if stoppedRes.Executed || stoppedAct.execs != 0 {
		t.Fatalf("a guest rollback whose guest is already stopped must REFUSE (nothing to undo, no blind re-stop) — got %+v execs=%d", stoppedRes, stoppedAct.execs)
	}
}
