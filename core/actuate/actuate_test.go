package actuate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/territory"
	"github.com/territory-grounder/grounder/core/verify"
)

// noObserved is a wired post-execution observer that observes NOTHING (an empty, non-nil observation) — a
// quiet post-state, so the verifier returns match. It satisfies the interceptor's verifiability gate (an
// executed action must carry an observer) without asserting a cascade; tests that need a deviation supply
// their own observer that returns a surprise alert.
func noObserved(context.Context) []verify.ObservedAlert { return []verify.ObservedAlert{} }

// fakeActuator records whether Exec was reached — so a refusal can be proven to NOT execute.
type fakeActuator struct{ execs int }

func (a *fakeActuator) Capability() string { return "test" }
func (a *fakeActuator) ReadOnly() bool     { return false }
func (a *fakeActuator) Exec(_ context.Context, _ []string, _ []byte) (actuation.Result, error) {
	a.execs++
	return actuation.Result{ExitCode: 0}, nil
}

// hostBoundActuator is a fakeActuator that also declares a single bound host (the SSH mutating leaf's
// HostBound capability), so the interceptor's host-match gate can be exercised. host=="" ⇒ not host-bound.
type hostBoundActuator struct {
	fakeActuator
	host string
}

func (a *hostBoundActuator) ActuationHost() string { return a.host }

func reversibleManifest(t *testing.T) *manifest.ActionManifest {
	m, err := manifest.New(manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Reversible: true}, safety.BandAuto, "plan#1", "pred#1")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func floorManifest(t *testing.T) *manifest.ActionManifest {
	m, err := manifest.New(manifest.Action{Target: "db01", OpClass: "dropdb", Op: "drop", Reversible: false}, safety.BandPollPause, "plan#2", "pred#2")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func boundEvidence() []Evidence {
	return []Evidence{{ToolResultID: "tr-1", Captured: true, Successful: true, Recent: true, Relevant: true}}
}

// goodRequest is a fully-admissible mutating request (gated, reversible, bound evidence, matching id, and a
// wired post-execution observer so the verifiability gate is satisfied). Its FRESH per-incident band is AUTO
// (matching its AUTO manifest) so the 1b admission gate admits it without an approval (TG-126).
func goodRequest(t *testing.T) Request {
	m := reversibleManifest(t)
	return Request{Manifest: m, Gated: true, Argv: []string{"systemctl", "restart", "nginx"}, Evidence: boundEvidence(), Observe: noObserved, Band: safety.BandAuto}
}

// wired builds an interceptor over a chokepoint (mode-driven actuation chokepoint) and a fake actuator. Tests
// pass safety.NewActuatingChokepoint() for a mutation-ON posture (mode Semi/Full + preflight green — the
// successor to the retired NewMutationGate()+EnableMutation pair) or safety.NewReadOnlyChokepoint() for OFF.
func wired(cp *safety.Chokepoint, act *fakeActuator) *Interceptor {
	return NewInterceptor(cp, act, audit.NewLedger())
}

func TestMutationOffRefusesEverything(t *testing.T) {
	act := &fakeActuator{}
	i := wired(safety.NewReadOnlyChokepoint(), act) // mode Shadow (read-only)
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Refused || act.execs != 0 {
		t.Fatalf("read-only system must refuse and NOT execute: %+v execs=%d", out, act.execs)
	}
}

func TestSelfTestAndDoFailLoudWhenUnwired(t *testing.T) {
	for name, i := range map[string]*Interceptor{
		"nil chokepoint": NewInterceptor(nil, &fakeActuator{}, audit.NewLedger()),
		"nil actuator":   NewInterceptor(safety.NewReadOnlyChokepoint(), nil, audit.NewLedger()),
		"nil ledger":     NewInterceptor(safety.NewReadOnlyChokepoint(), &fakeActuator{}, nil),
	} {
		if err := i.SelfTest(); !errors.Is(err, ErrGateUnwired) {
			t.Fatalf("%s: SelfTest must fail loud, got %v", name, err)
		}
		if _, err := i.Do(context.Background(), goodRequest(t)); !errors.Is(err, ErrGateUnwired) {
			t.Fatalf("%s: Do on an unwired chain must fail loud and not execute, got %v", name, err)
		}
	}
}

// The preflight proof (the successor to the retired EnableMutation proof gate, REQ-1520/1521): the boot
// preflight can be marked green ONLY behind a wired interception chain (SelfTest passes); an unwired chain is
// refused. Marking the preflight green does NOT actuate — the mode governs that — so this asserts the proof
// gate, not enablement.
func TestProvePreflightOnlyBehindProvenChain(t *testing.T) {
	// unwired interceptor cannot prove the preflight
	cp := safety.NewChokepoint(safety.NewFixedModeAuthority(true))
	if err := cp.ProvePreflight(NewInterceptor(nil, nil, nil)); err == nil || cp.IsPreflightGreen() {
		t.Fatalf("preflight must not go green behind an unwired chain: err=%v green=%v", err, cp.IsPreflightGreen())
	}
	// a wired interceptor proves it green — and because the bound mode actuates, the chokepoint now actuates.
	cp2 := safety.NewChokepoint(safety.NewFixedModeAuthority(true))
	if err := cp2.ProvePreflight(wired(cp2, &fakeActuator{})); err != nil || !cp2.IsPreflightGreen() || !cp2.MayActuate() {
		t.Fatalf("a proven wired chain must mark preflight green: err=%v green=%v mayActuate=%v", err, cp2.IsPreflightGreen(), cp2.MayActuate())
	}
}

func TestFloorRefusedAtAdapterEvenWithMutationOn(t *testing.T) {
	cp := safety.NewActuatingChokepoint()
	act := &fakeActuator{}
	i := wired(cp, act)
	// a never-auto floor op (dropdb, irreversible) is refused at the adapter — defense in depth. Fresh band AUTO
	// so the 1b admission gate ADMITS it — isolating the floor (step 2) as the refusing gate, not 1b.
	m := floorManifest(t)
	out, _ := i.Do(context.Background(), Request{Manifest: m, Gated: true, Evidence: boundEvidence(), Argv: []string{"dropdb", "x"}, Band: safety.BandAuto})
	if !out.Refused || act.execs != 0 {
		t.Fatalf("a never-auto floor op must be refused at the adapter even with mutation on: %+v execs=%d", out, act.execs)
	}
}

// A destructive floor op UNDER-DECLARED with a benign op_class + reversible=true is still refused at the
// adapter floor via the actual-command re-derivation (IsDestructiveOp) — even with the territory acknowledged
// (which would otherwise let it through). A plan cannot hide a mutation.
func TestFloorRefusesMisdeclaredDestructiveOp(t *testing.T) {
	cp := safety.NewActuatingChokepoint()
	act := &fakeActuator{}
	i := NewInterceptor(cp, act, audit.NewLedger())
	m, err := manifest.New(manifest.Action{Target: "prod-cluster", OpClass: "restart-service", Op: "kubectl delete pvc data-postgres-0 -n prod", Reversible: true}, safety.BandAuto, "plan#md", "pred#md")
	if err != nil {
		t.Fatal(err)
	}
	req := Request{
		Manifest:     m,
		Gated:        true,
		Evidence:     boundEvidence(),
		Acknowledged: map[territory.Territory]bool{territory.TerritoryK8s: true}, // territory would ALLOW — isolate the floor
		Argv:         []string{"kubectl", "delete", "pvc", "data-postgres-0", "-n", "prod"},
		Band:         safety.BandAuto, // fresh band AUTO admits at 1b — isolating the adapter floor as the refuser
	}
	out, _ := i.Do(context.Background(), req)
	if !out.Refused || act.execs != 0 {
		t.Fatalf("a mis-declared destructive op (benign op_class, reversible) must be refused at the adapter floor: %+v execs=%d", out, act.execs)
	}
	if !contains3(out.Reason, "floor") {
		t.Fatalf("the refusal must cite the adapter floor, got %q", out.Reason)
	}
}

func TestStructureAndEvidenceGates(t *testing.T) {
	cp := safety.NewActuatingChokepoint()
	act := &fakeActuator{}
	i := wired(cp, act)

	// ungated ⇒ refuse
	r := goodRequest(t)
	r.Gated = false
	if out, _ := i.Do(context.Background(), r); !out.Refused {
		t.Fatal("an ungated action must be refused (no committed prediction)")
	}
	// action_id mismatch ⇒ refuse. Tamper the sealed manifest's bound action after sealing.
	r2 := goodRequest(t)
	r2.Manifest.Action.Op = "reload" // mutate the action so its derived id != sealed id
	if out, _ := i.Do(context.Background(), r2); !out.Refused {
		t.Fatal("a tampered action_id must be refused")
	}
	// no bound evidence ⇒ refuse
	r3 := goodRequest(t)
	r3.Evidence = []Evidence{{ToolResultID: "tr", Captured: false, Successful: true, Recent: true, Relevant: true}}
	if out, _ := i.Do(context.Background(), r3); !out.Refused {
		t.Fatal("an action with no bound (orchestrator-captured) evidence must be refused")
	}
	if act.execs != 0 {
		t.Fatalf("no refusal path may reach Execute, got execs=%d", act.execs)
	}
}

// A nil manifest is an inadmissible request, not an unwired chain: Do must refuse (not error), must not
// execute, and must still audit the refusal — proving error returns are reserved for the unwired chain and
// that a refusal with no bound action id is not silently dropped by the ledger (INV-19).
func TestNilManifestIsRecordedRefusalNotError(t *testing.T) {
	cp := safety.NewActuatingChokepoint()
	act := &fakeActuator{}
	ledger := audit.NewLedger()
	i := NewInterceptor(cp, act, ledger)
	out, err := i.Do(context.Background(), Request{Manifest: nil, Gated: true, Evidence: boundEvidence()})
	if err != nil {
		t.Fatalf("a nil-manifest request is inadmissible, not an unwired chain — must refuse, not error: %v", err)
	}
	if !out.Refused || out.Executed || act.execs != 0 {
		t.Fatalf("a nil-manifest request must be refused and never execute: %+v execs=%d", out, act.execs)
	}
	if ledger.Len() == 0 || ledger.Verify() != nil {
		t.Fatalf("the nil-manifest refusal must be audited to a verifiable ledger, len=%d", ledger.Len())
	}
}

func TestGovernedActuationExecutesVerifiesAudits(t *testing.T) {
	cp := safety.NewActuatingChokepoint()
	act := &fakeActuator{}
	ledger := audit.NewLedger()
	i := NewInterceptor(cp, act, ledger)
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || out.Refused || act.execs != 1 {
		t.Fatalf("a fully-admissible request must execute exactly once: %+v execs=%d", out, act.execs)
	}
	// a quiet remediation with no observed cascade ⇒ match verdict.
	if out.Verdict != safety.VerdictMatch {
		t.Fatalf("a quiet execution should verify as match, got %q", out.Verdict)
	}
	// the execution appended a verifiable audit record.
	if ledger.Len() == 0 || ledger.Verify() != nil {
		t.Fatalf("execution must append a verifiable audit record, len=%d", ledger.Len())
	}
}

// Host-match gate (B1): a single-host-bound effect leaf (the SSH mutating leaf) runs the argv on its
// CONFIGURED host and never reads the action's target, so an action admitted for a DIFFERENT host must be
// refused BEFORE execute — otherwise it would mis-actuate on the configured host. goodRequest targets "web01".
func TestHostMatchGateRefusesTargetMismatch(t *testing.T) {
	act := &hostBoundActuator{host: "other-host"} // bound to a DIFFERENT host than the "web01" target
	i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger())
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Refused || out.Executed || act.execs != 0 {
		t.Fatalf("a target≠bound-host action must be refused BEFORE execute, got %+v execs=%d", out, act.execs)
	}
}

// The mirror: when the leaf's bound host matches the action target, the gate passes and the action executes.
func TestHostMatchGateExecutesWhenTargetMatches(t *testing.T) {
	act := &hostBoundActuator{host: "web01"} // bound to the request's "web01" target
	i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger())
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || out.Refused || act.execs != 1 {
		t.Fatalf("a matching-host action must execute exactly once, got %+v execs=%d", out, act.execs)
	}
}

// An empty ActuationHost means the leaf is NOT single-host-bound (a per-target/resource-id leaf), so the gate
// is a no-op and the action executes regardless of target — the gate must not over-reach to other leaves.
func TestHostMatchGateNoOpWhenUnbound(t *testing.T) {
	act := &hostBoundActuator{host: ""} // empty ⇒ not host-bound
	i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger())
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || act.execs != 1 {
		t.Fatalf("an empty bound-host must not gate (no-op), got %+v execs=%d", out, act.execs)
	}
}

// k8sManifest is a reversible, non-floor action in the k8s territory (passes the floor/structure/evidence
// gates and reaches the territory step).
func k8sManifest(t *testing.T) *manifest.ActionManifest {
	m, err := manifest.New(manifest.Action{Target: "prod-cluster", OpClass: "restart-service", Op: "kubectl rollout restart deploy/api", Reversible: true}, safety.BandAuto, "plan#k", "pred#k")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The territory gate is wired into the chain: a mutating action in a high-stakes territory whose ground
// rules were NOT acknowledged is refused (never executes); acknowledging the territory lets it through.
func TestTerritoryGateWiredIntoChain(t *testing.T) {
	// unacknowledged k8s action → refused at the territory gate, no execute.
	cp := safety.NewActuatingChokepoint()
	act := &fakeActuator{}
	i := NewInterceptor(cp, act, audit.NewLedger())
	req := Request{Manifest: k8sManifest(t), Gated: true, Argv: []string{"kubectl", "rollout", "restart", "deploy/api"}, Evidence: boundEvidence(), Observe: noObserved, Band: safety.BandAuto}
	out, err := i.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Refused || act.execs != 0 {
		t.Fatalf("an unacknowledged high-stakes territory action must be refused and NOT execute: %+v execs=%d", out, act.execs)
	}
	if !contains3(out.Reason, "territory gate") {
		t.Fatalf("the refusal must cite the territory gate, got %q", out.Reason)
	}

	// same action WITH the k8s territory acknowledged → executes.
	cp2 := safety.NewActuatingChokepoint()
	act2 := &fakeActuator{}
	i2 := NewInterceptor(cp2, act2, audit.NewLedger())
	req.Acknowledged = map[territory.Territory]bool{territory.TerritoryK8s: true}
	out2, err := i2.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !out2.Executed || act2.execs != 1 {
		t.Fatalf("an acknowledged high-stakes action must execute: %+v execs=%d", out2, act2.execs)
	}
}

func contains3(s, sub string) bool { return strings.Contains(s, sub) }

// pollReversibleManifest is a reversible, non-floor action classified POLL_PAUSE (needs approval).
func pollReversibleManifest(t *testing.T) *manifest.ActionManifest {
	m, err := manifest.New(manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Reversible: true}, safety.BandPollPause, "plan#p", "pred#p")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Admission gate: a POLL_PAUSE-band action auto-executes ONLY with a recorded approval (INV-12); an AUTO band
// needs none. Here the FRESH per-incident band is POLL_PAUSE (TG-126) — the admission gate reads it.
func TestAdmissionGateRequiresApprovalForPollBand(t *testing.T) {
	// poll band, NOT approved → refused, no execute.
	cp := safety.NewActuatingChokepoint()
	act := &fakeActuator{}
	i := NewInterceptor(cp, act, audit.NewLedger())
	req := Request{Manifest: pollReversibleManifest(t), Gated: true, Argv: []string{"systemctl", "restart", "nginx"}, Evidence: boundEvidence(), Observe: noObserved, Band: safety.BandPollPause}
	if out, _ := i.Do(context.Background(), req); !out.Refused || act.execs != 0 || !contains3(out.Reason, "approval") {
		t.Fatalf("an unapproved poll-band action must be refused: %+v execs=%d", out, act.execs)
	}
	// same, APPROVED → executes.
	cp2 := safety.NewActuatingChokepoint()
	act2 := &fakeActuator{}
	i2 := NewInterceptor(cp2, act2, audit.NewLedger())
	req.Approved = true
	if out, _ := i2.Do(context.Background(), req); !out.Executed || act2.execs != 1 {
		t.Fatalf("an approved poll-band action must execute: %+v execs=%d", out, act2.execs)
	}
	// an AUTO band needs no approval (goodRequest is AUTO) — already covered by TestGovernedActuationExecutes.
}

// TG-126 — THE FIX: the 1b admission gate reads the FRESH per-incident band, NOT the frozen content-addressed
// manifest band. An action whose sealed manifest was FIRST classified POLL_PAUSE (frozen forever by
// first-seal-wins), but whose CURRENT incident classifies AUTO, is ADMITTED at 1b WITHOUT an approval — it
// proceeds and executes, because the stale frozen POLL_PAUSE manifest band no longer blocks it. This is the
// exact on-box canary shape (restart nginx on librespeed01 sealed POLL_PAUSE during graduation, later
// re-classified AUTO) that dead-refused before the fix.
func TestAdmissionUsesFreshBandNotFrozenManifestBand(t *testing.T) {
	cp := safety.NewActuatingChokepoint()
	act := &fakeActuator{}
	i := NewInterceptor(cp, act, audit.NewLedger())
	// manifest FROZEN POLL_PAUSE, but THIS incident's fresh band is AUTO and NO approval is on file.
	req := Request{
		Manifest: pollReversibleManifest(t), Gated: true, Argv: []string{"systemctl", "restart", "nginx"},
		Evidence: boundEvidence(), Observe: noObserved, Band: safety.BandAuto, Approved: false,
	}
	out, err := i.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || act.execs != 1 {
		t.Fatalf("a fresh AUTO band must be ADMITTED at 1b despite a frozen POLL_PAUSE manifest band, and execute: %+v execs=%d", out, act.execs)
	}
	if contains3(out.Reason, "poll-band") {
		t.Fatalf("the frozen POLL_PAUSE manifest band must NOT block a fresh AUTO incident at 1b, got %q", out.Reason)
	}
}

// TG-126 — THE MIRROR SAFETY TEST (#1 safety assertion): a fresh POLL_PAUSE band MUST win even when the frozen
// manifest band is AUTO. An action first-sealed AUTO, whose CURRENT incident re-classifies POLL_PAUSE with NO
// recorded approval, is REFUSED at 1b — the stale frozen AUTO band must NEVER leak an auto-execute past a
// fresh POLL_PAUSE classification. This is the exact inverse of the bug and the reason 1b reads the fresh band
// alone (never OR-ing in the manifest band).
func TestAdmissionFreshPollPauseBandWinsOverFrozenAutoManifest(t *testing.T) {
	cp := safety.NewActuatingChokepoint()
	act := &fakeActuator{}
	i := NewInterceptor(cp, act, audit.NewLedger())
	// manifest FROZEN AUTO (reversibleManifest is AUTO), but THIS incident's fresh band is POLL_PAUSE, unapproved.
	req := Request{
		Manifest: reversibleManifest(t), Gated: true, Argv: []string{"systemctl", "restart", "nginx"},
		Evidence: boundEvidence(), Observe: noObserved, Band: safety.BandPollPause, Approved: false,
	}
	out, err := i.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Refused || out.Executed || act.execs != 0 {
		t.Fatalf("a fresh POLL_PAUSE band must REFUSE at 1b even with a frozen AUTO manifest (no stale-AUTO leak): %+v execs=%d", out, act.execs)
	}
	if !contains3(out.Reason, "approval") {
		t.Fatalf("the refusal must cite the missing approval (1b), got %q", out.Reason)
	}
	// same incident WITH a recorded approval → admitted (INV-12 path, fresh POLL_PAUSE + vote).
	cp2 := safety.NewActuatingChokepoint()
	act2 := &fakeActuator{}
	i2 := NewInterceptor(cp2, act2, audit.NewLedger())
	req.Approved = true
	if out2, _ := i2.Do(context.Background(), req); !out2.Executed || act2.execs != 1 {
		t.Fatalf("a fresh POLL_PAUSE band WITH a recorded approval must execute (INV-12): %+v execs=%d", out2, act2.execs)
	}
}

// TG-126 — FAIL-CLOSED: an ABSENT/zero fresh band is safety.BandPollPause by design, so a request with no band
// set and no approval is REFUSED at 1b — a missing band NEVER admits an auto-execute, regardless of the frozen
// manifest band (here AUTO). This guards against a caller that forgets to thread the fresh band.
func TestAdmissionAbsentFreshBandFailsClosed(t *testing.T) {
	cp := safety.NewActuatingChokepoint()
	act := &fakeActuator{}
	i := NewInterceptor(cp, act, audit.NewLedger())
	// NO Band field set (zero value) — even though the frozen manifest band is AUTO, and no approval.
	req := Request{
		Manifest: reversibleManifest(t), Gated: true, Argv: []string{"systemctl", "restart", "nginx"},
		Evidence: boundEvidence(), Observe: noObserved, // Band omitted ⇒ zero ⇒ BandPollPause
	}
	out, err := i.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Refused || out.Executed || act.execs != 0 || !contains3(out.Reason, "approval") {
		t.Fatalf("an absent/zero fresh band must fail closed (refuse at 1b, no execute): %+v execs=%d", out, act.execs)
	}
}

// fakeVerdictSink records committed verdicts (and can be made to fail).
type fakeVerdictSink struct {
	got map[string]safety.Verdict
	err error
}

func (f *fakeVerdictSink) Commit(_ context.Context, actionID, _, _, _ string, v safety.Verdict) error {
	if f.err != nil {
		return f.err
	}
	if f.got == nil {
		f.got = map[string]safety.Verdict{}
	}
	f.got[actionID] = v
	return nil
}

// The interceptor durably persists the mechanical verdict for an executed action; a persist failure surfaces
// (the execution stands — it cannot be un-done) so the caller can reconcile.
func TestInterceptorPersistsVerdict(t *testing.T) {
	cp := safety.NewActuatingChokepoint()
	act := &fakeActuator{}
	sink := &fakeVerdictSink{}
	i := NewInterceptor(cp, act, audit.NewLedger()).WithVerdictSink(sink)
	req := goodRequest(t)
	out, err := i.Do(context.Background(), req)
	if err != nil || !out.Executed {
		t.Fatalf("must execute: %+v err=%v", out, err)
	}
	if sink.got[out.ActionID] != safety.VerdictMatch {
		t.Fatalf("the verdict must be durably persisted, got %v", sink.got)
	}
	// a persist failure: the action executed, but the outcome flags the un-persisted verdict.
	cp2 := safety.NewActuatingChokepoint()
	act2 := &fakeActuator{}
	i2 := NewInterceptor(cp2, act2, audit.NewLedger()).WithVerdictSink(&fakeVerdictSink{err: errors.New("db down")})
	out2, _ := i2.Do(context.Background(), goodRequest(t))
	if !out2.Executed || act2.execs != 1 || !contains3(out2.Reason, "not persisted") {
		t.Fatalf("a verdict-persist failure must surface while the execution stands: %+v execs=%d", out2, act2.execs)
	}
}

// After a governed execution the sealed manifest carries the executed → verified lifecycle stages and the
// whole chain binds one action_id (INV-07).
func TestInterceptorRecordsLifecycleChain(t *testing.T) {
	cp := safety.NewActuatingChokepoint()
	act := &fakeActuator{}
	i := NewInterceptor(cp, act, audit.NewLedger())
	req := goodRequest(t)
	out, err := i.Do(context.Background(), req)
	if err != nil || !out.Executed {
		t.Fatalf("must execute cleanly: %+v err=%v", out, err)
	}
	stages := req.Manifest.Stages
	if len(stages) != 2 || stages[0].Stage != manifest.StageExecuted || stages[1].Stage != manifest.StageVerified {
		t.Fatalf("the manifest must record executed→verified, got %+v", stages)
	}
	// every stage binds the one action id, and the whole chain verifies.
	for _, s := range stages {
		if s.ActionID != req.Manifest.ActionID {
			t.Fatalf("stage %s bound to a different action", s.Stage)
		}
	}
	if err := req.Manifest.VerifyChain(); err != nil {
		t.Fatalf("the lifecycle chain must verify: %v", err)
	}
}

// The verifiability gate (BUILD-4a): a fully-admissible mutating request with mutation ON but NO
// post-execution observer is refused BEFORE it executes — the chain will not execute what it cannot verify,
// so ComputeVerdict is never reached with a nil observation on an executed action. Proven structurally: the
// actuator is never touched.
func TestVerifiabilityGateRefusesWithoutObserver(t *testing.T) {
	cp := safety.NewActuatingChokepoint()
	act := &fakeActuator{}
	i := wired(cp, act)
	r := goodRequest(t)
	r.Observe = nil // no post-execution observer wired ⇒ unverifiable
	out, err := i.Do(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Refused || out.Executed || act.execs != 0 {
		t.Fatalf("a request with no observer must be refused BEFORE execute: %+v execs=%d", out, act.execs)
	}
	if !contains3(out.Reason, "unverifiable") {
		t.Fatalf("the refusal must cite unverifiability, got %q", out.Reason)
	}
}

// The verifier is no longer theater (BUILD-4a / red-team chain #1): with a wired observer, a mispredicted
// post-state — a surprise alert on a host the committed prediction never named — yields a DEVIATION verdict
// (not match), and a deviation is never auto-resolvable (INV-10). This is exactly the catch that would be
// dead if ComputeVerdict ran against a nil observation.
func TestMispredictedPostStateYieldsDeviation(t *testing.T) {
	cp := safety.NewActuatingChokepoint()
	act := &fakeActuator{}
	i := wired(cp, act)
	r := goodRequest(t) // targets web01
	r.Prediction = verify.Prediction{
		ActionID:       r.Manifest.ActionID,
		TargetHost:     "web01",
		Site:           "nl",
		PredictedHosts: map[string]struct{}{"web01": {}},
	}
	// the post-state SURPRISES the prediction: an alert on a host it never named APPEARS after the action (a real
	// cascade). It must be NEW vs the pre-execute baseline (TG-148), so the first Observe (pre) is quiet.
	call := 0
	r.Observe = func(context.Context) []verify.ObservedAlert {
		call++
		if call == 1 {
			return []verify.ObservedAlert{} // pre-execute baseline: quiet
		}
		return []verify.ObservedAlert{{Host: "surprise99", Rule: "HostDown", Site: "nl"}} // post: the surprise appears
	}
	out, err := i.Do(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || act.execs != 1 {
		t.Fatalf("the admissible action must execute: %+v execs=%d", out, act.execs)
	}
	if out.Verdict != safety.VerdictDeviation {
		t.Fatalf("a mispredicted post-state must verify as deviation, got %q", out.Verdict)
	}
	if verify.AutoResolvable(out.Verdict) {
		t.Fatal("a deviation must never be auto-resolvable (never-auto)")
	}
}
