package runner

// TG-496 fix (c) — the deterministic guest-down auto-heal fast-path oracles. This is SAFETY-CRITICAL: the
// change auto-PROPOSES an actuation, so the suite pins BOTH halves — the fast path fires for exactly the
// confirmed case, and it fails CLOSED for every other. The load-bearing claims, each with its RED mutation:
//
//  1. A CONFIRMED-STOPPED pve-liveness guest-down emits a start-guest PROPOSAL with ZERO agent-loop/model
//     grounding calls (RED on main: no fast path ⇒ InvestigateActivity runs ⇒ the model IS called and the
//     collapsed brain proposes nothing).
//  2. A FLAP / not-confirmed-stopped signal does NOT take the fast path (RED-proved by flipping the
//     precondition: the model IS called, the class stays STANDARD_AGENT).
//  3. The proposal carries commit-confirmed arming and traverses the arm block + the TG-378 seal gate.
//  4. A criticality-tier incident never fast-paths (the !critical guard).
//  5. A non-guest-down / non-pve-liveness incident classifies byte-identically (KnownProcedure stays false).
//  6. The disposition is an HONEST deterministic-heal record, never a fabricated investigation.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/correlate"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/safety"
	pveliveness "github.com/territory-grounder/grounder/modules/ingest/pveliveness"
)

const healGuest = "librespeed01"

var healAt = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// stopped/running/unknown are the three GuestRunning answers the load-bearing precondition turns on.
func stopped(context.Context, string) (bool, string, bool) {
	return false, "guest_liveness age=3s", true
}
func runningGuest(context.Context, string) (bool, string, bool) {
	return true, "guest_liveness age=3s", true
}
func unknownGuest(context.Context, string) (bool, string, bool) {
	return false, "no observation", false
}

// isolatedWindow is a single-observation correlation window — Assess returns an ISOLATED verdict
// (Correlated=false), the precondition for a single-guest deterministic heal.
func isolatedWindow(ref, source, host string) correlate.Window {
	return correlate.Window{Span: 10 * time.Minute, Observations: []correlate.Observation{
		{ExternalRef: ref, SourceType: source, Host: host, AlertRule: pveliveness.DeviceDownRule,
			Severity: "critical", At: healAt},
	}}
}

type guestDownOpts struct {
	source       string
	rule         string
	host         string
	guestRunning func(context.Context, string) (bool, string, bool) // nil ⇒ reader left UNWIRED (fail closed)
	critical     func(string) bool                                  // nil ⇒ nothing is P0
	window       correlate.Window
	windowErr    error
}

type guestDownHarness struct {
	res        RunnerResult
	decisions  []correlate.Decision
	triage     []judge.TriageRow
	modelCalls int32
	arms       []db.CommitConfirmRow
}

// runGuestDown drives the REAL RunnerWorkflow end-to-end with a pve-liveness guest-down envelope, a
// call-counting model, and a scripted guest-liveness reader + correlation window. It captures what the
// routing actually DID — the class, the durable decision inputs, the triage row, the commit-confirm arm, and
// the number of model calls — so every assertion is over live behaviour, not a pure function's return.
func runGuestDown(t *testing.T, ref string, o guestDownOpts) guestDownHarness {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	h := guestDownHarness{}
	cm := &countingModel{}
	fake := newCCT0292Fake()
	deps := testDeps()
	deps.Model = cm
	deps.CommitConfirm = fake
	if o.guestRunning != nil {
		deps.Gate.GuestRunning = o.guestRunning
	}
	deps.CriticalityTier = o.critical
	deps.CorrelationWindow = func(context.Context, time.Time) (correlate.Window, error) {
		return o.window, o.windowErr
	}
	deps.ExecClassRecord = func(_ context.Context, d correlate.Decision) error {
		h.decisions = append(h.decisions, d)
		return nil
	}
	deps.TriageRecord = func(_ context.Context, row judge.TriageRow) error {
		h.triage = append(h.triage, row)
		return nil
	}
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: ref, SourceID: o.source, Host: o.host, AlertRule: o.rule,
		Severity: ingest.SeverityCritical, Site: "dc1", ReceivedAt: healAt,
	})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	if err := env.GetWorkflowResult(&h.res); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	h.modelCalls = atomic.LoadInt32(&cm.calls)
	h.arms, _ = fake.snapshot()
	return h
}

func confirmedOpts(ref string) guestDownOpts {
	return guestDownOpts{
		source: pveliveness.SourceType, rule: pveliveness.DeviceDownRule, host: healGuest,
		guestRunning: stopped, window: isolatedWindow(ref, pveliveness.SourceType, healGuest),
	}
}

// TestConfirmedGuestDownEmitsStartGuestWithZeroGroundingCalls is THE core oracle (RED on main): a confirmed
// pve-liveness guest-down proposes start-guest through a deterministic path that NEVER calls the model.
func TestConfirmedGuestDownEmitsStartGuestWithZeroGroundingCalls(t *testing.T) {
	h := runGuestDown(t, "tg496-confirmed", confirmedOpts("tg496-confirmed"))

	if h.modelCalls != 0 {
		t.Fatalf("a confirmed guest-down must heal with ZERO model/agent-loop grounding calls, got %d — the "+
			"fast path did not bypass InvestigateActivity, so it still depends on the collapsed brain", h.modelCalls)
	}
	if h.res.ExecClass != string(execclass.Deterministic) {
		t.Fatalf("class = %q, want DETERMINISTIC — the confirmed guest-down did not route to the fast path", h.res.ExecClass)
	}
	if len(h.triage) != 1 {
		t.Fatalf("want exactly one durable triage row, got %d", len(h.triage))
	}
	if row := h.triage[0]; !row.Proposed || row.OpClass != "start-guest" {
		t.Fatalf("triage row proposed=%v op_class=%q, want a proposed start-guest — the deterministic heal did "+
			"not emit the reversible remediation", row.Proposed, row.OpClass)
	}
	// The recorded classifier INPUT is honest about WHY the class is DETERMINISTIC (TG-42's wired signals).
	if len(h.decisions) != 1 || !h.decisions[0].Inputs.KnownProcedure || !h.decisions[0].Inputs.Reversible {
		t.Fatalf("recorded decision inputs = %+v, want KnownProcedure && Reversible — the exec_class_decision "+
			"row must say why a confirmed guest-down is deterministic, or the audit cannot be re-derived", h.decisions)
	}
}

// TestConfirmedGuestDownArmsCommitConfirmedRevert proves the deterministic proposal traverses the arm block:
// start-guest is commit-confirmed eligible, so the armed stop-guest revert is durably recorded BEFORE any
// effect. (The seal that precedes the arm also re-confirms observed-not-running — GuestRunning=stopped here.)
func TestConfirmedGuestDownArmsCommitConfirmedRevert(t *testing.T) {
	h := runGuestDown(t, "tg496-arm", confirmedOpts("tg496-arm"))
	if len(h.arms) != 1 {
		t.Fatalf("the deterministic start-guest heal must arm exactly one commit-confirmed revert, got %d — the "+
			"proposal did not flow through the arm block (no dead-man cover for the fast-path's first live runs)", len(h.arms))
	}
	if h.arms[0].OpClass != "start-guest" {
		t.Fatalf("armed revert op_class = %q, want start-guest", h.arms[0].OpClass)
	}
}

// TestFlappingGuestDownDoesNotTakeFastPath is the flap-rejection oracle (RED-prove by flipping the
// precondition): an UNCONFIRMED-stopped signal must NOT auto-propose a restart — it falls to the normal loop.
func TestFlappingGuestDownDoesNotTakeFastPath(t *testing.T) {
	o := confirmedOpts("tg496-flap")
	o.guestRunning = unknownGuest // the flap: the guest is NOT confirmed stopped
	h := runGuestDown(t, "tg496-flap", o)

	if h.modelCalls == 0 {
		t.Fatal("a flapping/unconfirmed guest-down took the ZERO-model fast path — a signal that is not " +
			"confirmed-stopped must fall to the normal agent loop, never auto-propose a restart")
	}
	if h.res.ExecClass == string(execclass.Deterministic) {
		t.Fatalf("class = DETERMINISTIC for an unconfirmed guest-down — the load-bearing confirmed-stopped "+
			"precondition did not gate the fast path (res=%+v)", h.res)
	}
	if len(h.decisions) == 1 && h.decisions[0].Inputs.KnownProcedure {
		t.Fatal("the classifier set KnownProcedure for an unconfirmed guest-down — the flap must never wire the signal")
	}
}

// TestRunningGuestDoesNotTakeFastPath: a guest observed RUNNING (already recovered) is not a heal case.
func TestRunningGuestDoesNotTakeFastPath(t *testing.T) {
	o := confirmedOpts("tg496-running")
	o.guestRunning = runningGuest
	h := runGuestDown(t, "tg496-running", o)
	if h.modelCalls == 0 || h.res.ExecClass == string(execclass.Deterministic) {
		t.Fatalf("a RUNNING guest took the fast path (calls=%d class=%q) — start is not proposed for a guest "+
			"already up", h.modelCalls, h.res.ExecClass)
	}
}

// TestUnwiredReaderDoesNotTakeFastPath: no guest-state reader ⇒ the precondition cannot be established ⇒ fail
// closed to the normal loop (never a heal on nothing — the pve03 shape).
func TestUnwiredReaderDoesNotTakeFastPath(t *testing.T) {
	o := confirmedOpts("tg496-unwired")
	o.guestRunning = nil // reader UNWIRED
	h := runGuestDown(t, "tg496-unwired", o)
	if h.res.ExecClass == string(execclass.Deterministic) {
		t.Fatalf("an UNWIRED state reader took the fast path (class=%q) — unknown is not not-running", h.res.ExecClass)
	}
}

// TestCriticalTierGuestDownDoesNotTakeFastPath is the !critical guard, LIVE: a criticality-tier (P0) guest is
// never silently fast-healed even when confirmed stopped.
func TestCriticalTierGuestDownDoesNotTakeFastPath(t *testing.T) {
	o := confirmedOpts("tg496-crit")
	o.critical = func(string) bool { return true } // this guest is P0
	h := runGuestDown(t, "tg496-crit", o)
	if h.res.ExecClass == string(execclass.Deterministic) {
		t.Fatalf("a criticality-tier guest-down took the fast path (class=%q) — a P0 guest must get an agent, "+
			"not a silent deterministic heal", h.res.ExecClass)
	}
	// ...and the structural guard in the classifier itself: KnownProcedure+Reversible never beats !critical.
	if got := execclass.Classify(execclass.Input{KnownProcedure: true, Reversible: true, CriticalityTier: "host"}); got != execclass.StandardAgent {
		t.Fatalf("execclass.Classify(KnownProcedure,Reversible,host-tier) = %q, want STANDARD_AGENT — the "+
			"!critical guard on the fast classes was weakened", got)
	}
}

// TestNonPveLivenessDeviceDownClassifiesUnchanged is the byte-identical invariant: a slow LibreNMS Device-Down
// (same rule NAME, different source) — even for a guest observed stopped — never wires the fast-path signal
// and classifies exactly as before. The fast path is scoped to the TG-native edge-triggered detector alone.
func TestNonPveLivenessDeviceDownClassifiesUnchanged(t *testing.T) {
	o := guestDownOpts{
		source: "librenms-dc1", rule: pveliveness.DeviceDownRule, host: healGuest,
		guestRunning: stopped, // even WITH a stopped observation
		window:       isolatedWindow("tg496-librenms", "librenms-dc1", healGuest),
	}
	h := runGuestDown(t, "tg496-librenms", o)
	if h.res.ExecClass == string(execclass.Deterministic) {
		t.Fatalf("a NON-pve-liveness Device-Down took the fast path (class=%q) — the deterministic heal must be "+
			"scoped to the pve-liveness source, never a slow push alert under the same rule name", h.res.ExecClass)
	}
	if len(h.decisions) != 1 || h.decisions[0].Inputs.KnownProcedure || h.decisions[0].Inputs.Reversible {
		t.Fatalf("a non-liveness incident wired the classifier signals: %+v — non-guest-down classification "+
			"must be byte-identical (KnownProcedure/Reversible stay false)", h.decisions)
	}
}

// --- unit oracles over the emitter + predicate + seal gate ------------------------------------------------

// TestConfirmedGuestDownHealPredicateFailsClosed pins the load-bearing precondition directly: every arm but
// the confirmed one returns false (the classification gate can never open on a wrong "yes").
func TestConfirmedGuestDownHealPredicateFailsClosed(t *testing.T) {
	base := CorrelateInput{SourceID: pveliveness.SourceType, AlertRule: pveliveness.DeviceDownRule, Host: healGuest}
	mk := func(gr func(context.Context, string) (bool, string, bool), crit func(string) bool) *Activities {
		d := Deps{Gate: &predict.PredictionGate{GuestRunning: gr}, CriticalityTier: crit}
		return &Activities{D: d}
	}
	cases := []struct {
		name       string
		in         CorrelateInput
		correlated bool
		gr         func(context.Context, string) (bool, string, bool)
		crit       func(string) bool
		want       bool
	}{
		{"confirmed stopped", base, false, stopped, nil, true},
		{"correlated cascade", base, true, stopped, nil, false},
		{"running (recovered)", base, false, runningGuest, nil, false},
		{"unknown/flap", base, false, unknownGuest, nil, false},
		{"criticality tier", base, false, stopped, func(string) bool { return true }, false},
		{"wrong source", CorrelateInput{SourceID: "librenms-dc1", AlertRule: pveliveness.DeviceDownRule, Host: healGuest}, false, stopped, nil, false},
		{"wrong rule", CorrelateInput{SourceID: pveliveness.SourceType, AlertRule: "SomethingElse", Host: healGuest}, false, stopped, nil, false},
		{"empty host", CorrelateInput{SourceID: pveliveness.SourceType, AlertRule: pveliveness.DeviceDownRule}, false, stopped, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mk(c.gr, c.crit).confirmedGuestDownHeal(context.Background(), c.in, c.correlated); got != c.want {
				t.Fatalf("confirmedGuestDownHeal = %v, want %v", got, c.want)
			}
		})
	}
	// The reader-unwired arm is its own path (nil Gate and nil GuestRunning both fail closed).
	if (&Activities{D: Deps{}}).confirmedGuestDownHeal(context.Background(), base, false) {
		t.Fatal("a nil Gate must fail closed")
	}
	if (&Activities{D: Deps{Gate: &predict.PredictionGate{}}}).confirmedGuestDownHeal(context.Background(), base, false) {
		t.Fatal("a nil GuestRunning must fail closed")
	}
}

// TestDeterministicGuestHealActivityShapeAndProvenance: the emitter produces a correct, HONESTLY-labelled
// start-guest proposal with ZERO model calls, and DECLINES (fail-closed) when the precondition does not hold.
func TestDeterministicGuestHealActivityShapeAndProvenance(t *testing.T) {
	cm := &countingModel{}
	acts := NewActivities(Deps{Model: cm, Gate: &predict.PredictionGate{GuestRunning: stopped}})
	env := ingest.IncidentEnvelope{ExternalRef: "tg496-emit", Host: healGuest}

	res, err := acts.DeterministicGuestHealActivity(context.Background(), env)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if atomic.LoadInt32(&cm.calls) != 0 {
		t.Fatal("the deterministic emitter called the model — it must synthesize the proposal with zero grounding calls")
	}
	if !res.Proposed || res.Proposal.Action.OpClass != "start-guest" || res.Proposal.Action.Target != healGuest ||
		!res.Proposal.Action.Reversible || res.Proposal.Action.Params[opschema.ParamGuest] != healGuest {
		t.Fatalf("proposal shape = %+v, want a reversible start-guest on %s with the guest param", res.Proposal.Action, healGuest)
	}
	if !res.OpClassRegistered {
		t.Fatal("start-guest must resolve in the registry (OpClassRegistered) so it takes the real lane, not shadow-divert")
	}
	// HONEST RECORD: labelled deterministic-heal, never a fabricated investigation.
	if res.DecisionTier != deterministicHealTier || res.ModelTier != deterministicHealTier {
		t.Fatalf("tiers = model %q / decision %q, want %q — a deterministic heal must not read as an LLM decision",
			res.ModelTier, res.DecisionTier, deterministicHealTier)
	}
	if len(res.SkillLoads) != 1 || res.SkillLoads[0] != deterministicHealProvenance {
		t.Fatalf("skill loads = %v, want the [%s] provenance marker", res.SkillLoads, deterministicHealProvenance)
	}

	// DECLINE arms (recovered / unknown): Proposed=false ⇒ the workflow falls back to the normal loop.
	for name, gr := range map[string]func(context.Context, string) (bool, string, bool){
		"running": runningGuest, "unknown": unknownGuest,
	} {
		t.Run("declines when "+name, func(t *testing.T) {
			d := NewActivities(Deps{Gate: &predict.PredictionGate{GuestRunning: gr}})
			r, err := d.DeterministicGuestHealActivity(context.Background(), env)
			if err != nil {
				t.Fatalf("emit: %v", err)
			}
			if r.Proposed {
				t.Fatalf("the emitter proposed for a %s guest — it must re-confirm stopped and decline otherwise", name)
			}
		})
	}
	// Unwired reader declines too (never a blind start).
	if r, _ := NewActivities(Deps{}).DeterministicGuestHealActivity(context.Background(), env); r.Proposed {
		t.Fatal("the emitter proposed with no state reader wired — it must decline (fail closed)")
	}
}

// TestDeterministicHealSealGateReconfirmsStopped proves the actuation-time re-confirmation: the SAME
// start-guest shape the emitter produces is refused at the seal gate (GateActivity → PredictionGate.Commit)
// unless the guest is observed not-running at COMMIT — a guest that recovered can never be blind-started.
func TestDeterministicHealSealGateReconfirmsStopped(t *testing.T) {
	action := manifest.Action{Target: healGuest, OpClass: "start-guest", Op: "start",
		Params: map[string]string{opschema.ParamGuest: healGuest}, Reversible: true}
	gi := GateInput{Proposal: proposal.Proposal{ExternalRef: "tg496-seal", Action: action},
		Band: safety.BandAuto, PlanHash: "ph", Site: "dc1"}
	mkGate := func(gr func(context.Context, string) (bool, string, bool)) *predict.PredictionGate {
		graph := predict.NewDependencyGraph(map[string][]string{healGuest: nil})
		return &predict.PredictionGate{Store: predict.NewMemPredictionStore(),
			Model: &predict.InfragraphModel{Graph: graph, MaxDepth: 3}, Mode: predict.ModeEnforce, GuestRunning: gr}
	}

	if _, err := NewActivities(Deps{Gate: mkGate(stopped)}).GateActivity(context.Background(), gi); err != nil {
		t.Fatalf("a start-guest for a guest observed STOPPED must seal, got %v", err)
	}
	if _, err := NewActivities(Deps{Gate: mkGate(runningGuest)}).GateActivity(context.Background(), gi); err == nil {
		t.Fatal("a start-guest for a guest observed RUNNING sealed — the TG-378 seal gate did not re-confirm not-running")
	}
	if _, err := NewActivities(Deps{Gate: mkGate(nil)}).GateActivity(context.Background(), gi); err == nil {
		t.Fatal("a start-guest with no state reader sealed — an unestablished precondition must refuse (fail closed)")
	}
}

// TestStartGuestArmBlockPrerequisites documents the registry facts the arm block + seal gate depend on, so a
// future edit that (say) drops start-guest's inverse or its precondition trips HERE rather than in production.
func TestStartGuestArmBlockPrerequisites(t *testing.T) {
	spec, ok := opschema.Lookup("start-guest")
	if !ok {
		t.Fatal("start-guest must be a registered actuatable class")
	}
	if spec.CommitConfirmed == nil {
		t.Fatal("start-guest must be commit-confirmed eligible — the fast-path's first live runs need dead-man cover")
	}
	if spec.RollbackOpClass != "stop-guest" {
		t.Fatalf("start-guest inverse = %q, want stop-guest (the armed revert)", spec.RollbackOpClass)
	}
	if spec.RequiresTargetState != opschema.RequiresNotRunning {
		t.Fatalf("start-guest precondition = %q, want not-running (the seal-gate re-confirmation)", spec.RequiresTargetState)
	}
	if !opschema.AutoEligible(spec.SafetyTier) {
		t.Fatalf("start-guest tier %q is not auto-eligible — a deterministic heal that can never auto is inert", spec.SafetyTier)
	}
}
