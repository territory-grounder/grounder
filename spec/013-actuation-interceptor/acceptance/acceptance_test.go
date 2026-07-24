package acceptance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/cost"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// --- cost/budget spend-guard acceptance helpers (REQ-1211..1215) ---

// costDay + costClock pin the UTC day so the daily accumulator key is deterministic under test.
const costDay = "2026-07-20"

func costClock() time.Time { t, _ := time.Parse("2006-01-02", costDay); return t }

// costLedgerRec appends the cost-breaker trip note to a real ledger — the acceptance stand-in for the
// worker's costLedgerTripRecorder, so REQ-1212's "records a cost trip to the ledger" drives real code.
type costLedgerRec struct{ l *audit.Ledger }

func (r costLedgerRec) RecordTrip(reason string) {
	_, _ = r.l.Append(audit.GovDecision{Decision: "cost:breaker-trip", Reason: reason, ActionID: "cost-breaker-trip", Withheld: true})
}

// costErrStore fails every read/write — drives the FAIL-OPEN contract (REQ-1215).
type costErrStore struct{}

func (costErrStore) Accrue(context.Context, string, string, float64) (float64, error) {
	return 0, errors.New("cost store down")
}
func (costErrStore) Total(context.Context, string, string) (float64, error) { return 0, errors.New("down") }
func (costErrStore) BreakerOpen(context.Context) (bool, string, error) {
	return false, "", errors.New("down")
}
func (costErrStore) TripBreaker(context.Context, string, float64) error { return errors.New("down") }

// noObserved is a wired post-execution observer that observes nothing (an empty, non-nil observation) — it
// satisfies the interceptor's verifiability gate (an executed action must carry an observer) and, with no
// cascade observed, verifies as match.
func noObserved(context.Context) []verify.ObservedAlert { return []verify.ObservedAlert{} }

type fakeActuator struct {
	execs int
	host  string // when non-empty, declares this leaf single-host-bound (the interceptor's HostBound capability)
}

func (a *fakeActuator) Capability() string { return "test" }
func (a *fakeActuator) ReadOnly() bool     { return false }
func (a *fakeActuator) Exec(context.Context, []string, []byte) (actuation.Result, error) {
	a.execs++
	return actuation.Result{ExitCode: 0}, nil
}

// ActuationHost implements the interceptor's HostBound gate (REQ-1219): "" ⇒ not host-bound (no-op).
func (a *fakeActuator) ActuationHost() string { return a.host }

// ExecLog makes the test effect leaf an actuate.ExecRecorder: it records the executed argv as its own
// compensating inverse (a stand-in for a real reversible op), so the interceptor's execution_log recording
// (REQ-1209) can be exercised against the real chain.
func (a *fakeActuator) ExecLog(_ string, command []string) (forward, rollback []string, err error) {
	return command, command, nil
}

func reversibleManifest() *manifest.ActionManifest {
	m, _ := manifest.New(manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Reversible: true}, safety.BandAuto, "plan#1", "pred#1")
	return m
}

// pollReversibleManifest is a reversible POLL_PAUSE action (needs a human approval) — the canary shape
// (restart-service). floorPolicyManifest is an irreversible floor-class op (dropdb) used to prove the
// never-auto floor still refuses even a human-approved, policy-auto action.
func pollReversibleManifest() *manifest.ActionManifest {
	m, _ := manifest.New(manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Reversible: true}, safety.BandPollPause, "plan#p", "pred#p")
	return m
}

func floorPolicyManifest() *manifest.ActionManifest {
	m, _ := manifest.New(manifest.Action{Target: "db01", OpClass: "dropdb", Op: "drop", Reversible: false}, safety.BandPollPause, "plan#f", "pred#f")
	return m
}

// scriptedDecider is a fixed-verdict policy decider for the acceptance oracles: it lets a scenario drive the
// interceptor's policy-authorize step (spec/015 REQ-1506) with a known verdict, exactly as the real
// *policy.AuditedEngine would resolve it, so the interceptor's honoring of the verdict + recorded approval is
// exercised against the REAL core/actuate.Interceptor chain.
type scriptedDecider struct{ verdict policy.Verdict }

func (d scriptedDecider) Decide(_ context.Context, in policy.EvalInput) (policy.PolicyDecision, error) {
	return policy.NewPolicyDecision(d.verdict, "acceptance-rule", in.Band, nil, in.Mode, "scripted", policy.DecisionAudit{}), nil
}

func boundEvidence() []actuate.Evidence {
	return []actuate.Evidence{{ToolResultID: "tr-1", Captured: true, Successful: true, Recent: true, Relevant: true}}
}

func goodRequest() actuate.Request {
	// Band is the FRESH per-incident band (TG-126): AUTO here (matching the AUTO manifest), so the 1b admission
	// gate admits it without an approval — the interceptor's admission reads THIS, not the sealed manifest band.
	return actuate.Request{Manifest: reversibleManifest(), Gated: true, Argv: []string{"systemctl", "restart", "nginx"}, Evidence: boundEvidence(), Observe: noObserved, Band: safety.BandAuto}
}

func TestActuationInterceptorAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/013 actuation-interceptor",
		ScenarioInitializer: initializeScenario,
		Options:             &godog.Options{Format: "pretty", Paths: []string{"."}, Tags: "~@pending", Strict: true, TestingT: t},
	}
	if suite.Run() != 0 {
		t.Fatal("spec/013 acceptance scenarios failed")
	}
}

type world struct {
	gate   *safety.Chokepoint
	act    *fakeActuator
	ledger *audit.Ledger
	i      *actuate.Interceptor
	req    actuate.Request

	out   actuate.Outcome
	doErr error

	enableUnwiredErr, enableWiredErr error
	enabledAfterWired                bool

	// cross-process shared-kill (REQ-1210): a second worker's interceptor armed with its own breaker over the
	// SAME durable store worker-1 trips, so a trip in one worker force-Shadows this sibling.
	worker1Breaker *safety.MutationBreaker
	sibGate        *safety.Chokepoint
	sibAct         *fakeActuator
	sib            *actuate.Interceptor

	// cost/budget spend guard (REQ-1211..1215): the $-ceiling breaker over a real mode chokepoint.
	costStore   *cost.MemStore
	costGate    *safety.Chokepoint // the mode chokepoint the cost breaker force-Shadows
	costAcct    *cost.Accountant
	costLedger  *audit.Ledger
	sibCostGate *safety.Chokepoint // a sibling worker's chokepoint sharing the same durable cost store
	sibCostAcct *cost.Accountant
	costLogged  bool // set by the fail-open logger (REQ-1215)
}

func initializeScenario(sc *godog.ScenarioContext) {
	w := &world{}
	ctx := context.Background()

	sc.Step(`^a wired interceptor with mutation off$`, func() error {
		w.gate, w.act, w.ledger = safety.NewReadOnlyChokepoint(), &fakeActuator{}, audit.NewLedger()
		w.i = actuate.NewInterceptor(w.gate, w.act, w.ledger)
		w.req = goodRequest()
		return nil
	})
	sc.Step(`^a wired interceptor with mutation enabled$`, func() error {
		w.gate, w.act, w.ledger = safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()
		w.i = actuate.NewInterceptor(w.gate, w.act, w.ledger)
		w.req = goodRequest()
		return nil
	})
	sc.Step(`^an interceptor missing a governed collaborator$`, func() error {
		w.act = &fakeActuator{}
		w.i = actuate.NewInterceptor(nil, w.act, audit.NewLedger()) // nil gate ⇒ unwired
		w.req = goodRequest()
		return nil
	})
	sc.Step(`^a mutation gate that is off$`, func() error {
		w.gate = safety.NewReadOnlyChokepoint()
		return nil
	})

	do := func() error { w.out, w.doErr = w.i.Do(ctx, w.req); return nil }

	sc.Step(`^a fully-admissible mutating request is intercepted$`, do)
	sc.Step(`^a mutating request is intercepted$`, do)
	sc.Step(`^a never-auto floor operation is intercepted$`, func() error {
		m, _ := manifest.New(manifest.Action{Target: "db01", OpClass: "dropdb", Op: "drop", Reversible: false}, safety.BandPollPause, "p", "pr")
		// Fresh band AUTO (TG-126) admits at 1b — isolating the never-auto floor (step 2) as the refusing gate.
		w.req = actuate.Request{Manifest: m, Gated: true, Evidence: boundEvidence(), Argv: []string{"dropdb", "x"}, Band: safety.BandAuto}
		return do()
	})
	sc.Step(`^a mutating action with no committed prediction is intercepted$`, func() error {
		w.req.Gated = false
		return do()
	})
	sc.Step(`^a mutating action whose bound action changed is intercepted$`, func() error {
		w.req.Manifest.Action.Op = "reload" // mutate the sealed action ⇒ derived id != bound id
		return do()
	})
	sc.Step(`^a mutating action citing no orchestrator-captured evidence is intercepted$`, func() error {
		w.req.Evidence = []actuate.Evidence{{ToolResultID: "x", Captured: false, Successful: true, Recent: true, Relevant: true}}
		return do()
	})
	sc.Step(`^a mutating action with no post-execution observer is intercepted$`, func() error {
		w.req.Observe = nil // no observer wired ⇒ the post-state cannot be verified
		return do()
	})
	sc.Step(`^a mutating action whose post-state surprises its prediction is intercepted$`, func() error {
		w.req.Prediction = verify.Prediction{ActionID: w.req.Manifest.ActionID, TargetHost: "web01", Site: "nl", PredictedHosts: map[string]struct{}{"web01": {}}}
		// The surprise APPEARS after the action (a real cascade). The interceptor captures a pre-execute BASELINE
		// (TG-148: the post-state Observe is estate-wide), so a surprise must be NEW to deviate — the first Observe
		// (pre-execute) is quiet, the second (post) surfaces the surprise host.
		surpriseCall := 0
		w.req.Observe = func(context.Context) []verify.ObservedAlert {
			surpriseCall++
			if surpriseCall == 1 {
				return []verify.ObservedAlert{} // pre-execute baseline: quiet
			}
			return []verify.ObservedAlert{{Host: "surprise99", Rule: "HostDown", Site: "nl"}} // post: a host the prediction never named
		}
		return do()
	})
	sc.Step(`^a mutating action targeting a different host than the single-host-bound effect leaf is intercepted$`, func() error {
		// Mutation ON, all other gates admissible, but the effect leaf is bound to a host OTHER than the
		// action's target ("web01") — the host-match gate (REQ-1219) must refuse BEFORE execute.
		w.act = &fakeActuator{host: "other-host"}
		w.i = actuate.NewInterceptor(safety.NewActuatingChokepoint(), w.act, audit.NewLedger())
		w.req = goodRequest()
		return do()
	})
	sc.Step(`^enabling mutation is attempted on an unwired chain and then on a wired chain$`, func() error {
		// unwired: proving preflight behind an interceptor missing its governed collaborators must be refused,
		// and the preflight stays red (no actuation is unlocked).
		unwired := safety.NewChokepoint(safety.NewFixedModeAuthority(true))
		w.enableUnwiredErr = unwired.ProvePreflight(actuate.NewInterceptor(nil, nil, nil))
		if unwired.IsPreflightGreen() {
			return fmt.Errorf("a failed proof must leave the preflight red")
		}
		// wired: an actuating-mode chokepoint whose preflight is proven behind a fully-wired chain becomes able
		// to actuate — the successor to the old EnableMutation-behind-a-proven-chain semantics.
		cp := safety.NewChokepoint(safety.NewFixedModeAuthority(true))
		w.enableWiredErr = cp.ProvePreflight(actuate.NewInterceptor(cp, &fakeActuator{}, audit.NewLedger()))
		w.enabledAfterWired = cp.IsPreflightGreen() && cp.MayActuate()
		return nil
	})

	// Cross-process shared kill (REQ-1210, design-wisdom #3): two workers coordinate through ONE breaker store
	// (a MemStore shared by two breaker values is the in-memory twin of the durable mutation_breaker_state row).
	sc.Step(`^two workers whose armed breakers share one durable breaker store$`, func() error {
		shared := breaker.NewMemStore()
		// worker-1: an armed breaker over the shared store.
		cp1 := safety.NewActuatingChokepoint()
		mb1, err := safety.NewMutationBreaker(cp1, shared, 1, nil)
		if err != nil {
			return err
		}
		w.worker1Breaker = mb1
		// worker-2 (the sibling): a REAL actuating interceptor armed with its OWN breaker over the SAME store.
		w.sibGate = safety.NewActuatingChokepoint()
		w.sibAct = &fakeActuator{}
		mb2, err := safety.NewMutationBreaker(w.sibGate, shared, 1, nil)
		if err != nil {
			return err
		}
		w.sib = actuate.NewInterceptor(w.sibGate, w.sibAct, audit.NewLedger()).WithMutationBreaker(mb2)
		w.req = goodRequest()
		if !w.sibGate.MayActuate() {
			return fmt.Errorf("precondition: the sibling worker must be actuating before it honors the cross-process trip")
		}
		return nil
	})
	sc.Step(`^a deviation trips the first worker's breaker$`, func() error {
		disabled, err := w.worker1Breaker.Trip(ctx, "deviation on worker-1")
		if err != nil || !disabled {
			return fmt.Errorf("worker-1 trip must open the shared breaker: disabled=%v err=%v", disabled, err)
		}
		return nil
	})
	sc.Step(`^the second worker refuses a fully-admissible mutation and drops to read-only$`, func() error {
		out, err := w.sib.Do(ctx, w.req)
		if err != nil {
			return fmt.Errorf("the sibling interceptor failed loud: %v", err)
		}
		if !out.Refused || w.sibAct.execs != 0 {
			return fmt.Errorf("the sibling must refuse and NOT execute after the shared trip: %+v execs=%d", out, w.sibAct.execs)
		}
		if w.sibGate.MayActuate() {
			return fmt.Errorf("the sibling must be force-Shadowed (read-only) after honoring the cross-process trip")
		}
		return nil
	})

	// --- Policy-authorize honoring a recorded human approval (spec/015 REQ-1506, the graduation-deadlock fix).
	sc.Step(`^a wired interceptor with mutation enabled and a policy engine that resolves (auto|approve|deny)$`, func(v string) error {
		w.gate, w.act, w.ledger = safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()
		verdicts := map[string]policy.Verdict{"auto": policy.VerdictAuto, "approve": policy.VerdictApprove, "deny": policy.VerdictDeny}
		w.i = actuate.NewInterceptor(w.gate, w.act, w.ledger).
			WithPolicyDecider(scriptedDecider{verdict: verdicts[v]}, func() policy.Mode { return policy.ModeFullAuto })
		w.req = goodRequest()
		return nil
	})
	sc.Step(`^a reversible poll-band mutating request with a recorded human approval is intercepted$`, func() error {
		w.req.Manifest = pollReversibleManifest() // POLL_PAUSE ⇒ 1b requires the approval that follows
		w.req.Band = safety.BandPollPause         // the FRESH per-incident band is POLL_PAUSE (TG-126) — 1b reads THIS
		w.req.Approved = true                     // the recorded human vote (INV-12)
		return do()
	})
	sc.Step(`^a mutating request with no recorded human approval is intercepted$`, func() error {
		// goodRequest is an AUTO band (passes admission 1b), so the ONLY thing that can refuse an `approve`
		// verdict here is step 4d's own approval floor — isolating it from the poll-band admission gate.
		w.req.Approved = false
		return do()
	})
	sc.Step(`^an irreversible floor-class request with a recorded human approval is intercepted$`, func() error {
		w.req.Manifest = floorPolicyManifest() // irreversible dropdb
		w.req.Argv = []string{"dropdb", "x"}
		w.req.Approved = true // even a human-approved, policy-auto floor op must be refused at the never-auto floor
		return do()
	})

	sc.Step(`^the request is refused and nothing is executed$`, func() error {
		if !w.out.Refused || w.act.execs != 0 {
			return fmt.Errorf("must refuse and not execute: %+v execs=%d", w.out, w.act.execs)
		}
		return nil
	})
	sc.Step(`^the interceptor fails loud and nothing is executed$`, func() error {
		if !errors.Is(w.doErr, actuate.ErrGateUnwired) || w.act.execs != 0 {
			return fmt.Errorf("an unwired chain must fail loud and not execute: err=%v execs=%d", w.doErr, w.act.execs)
		}
		return nil
	})
	sc.Step(`^the unwired attempt is refused and the wired attempt enables mutation$`, func() error {
		if w.enableUnwiredErr == nil {
			return fmt.Errorf("enabling on an unwired chain must be refused")
		}
		if w.enableWiredErr != nil || !w.enabledAfterWired {
			return fmt.Errorf("enabling on a proven wired chain must succeed: err=%v enabled=%v", w.enableWiredErr, w.enabledAfterWired)
		}
		return nil
	})
	sc.Step(`^the action executes once, a mechanical verdict is written, and the decision is audited$`, func() error {
		if !w.out.Executed || w.act.execs != 1 {
			return fmt.Errorf("a fully-admissible request must execute exactly once: %+v execs=%d", w.out, w.act.execs)
		}
		if !safety.ValidVerdict(w.out.Verdict) {
			return fmt.Errorf("a mechanical verdict must be written, got %q", w.out.Verdict)
		}
		if w.ledger.Len() == 0 || w.ledger.Verify() != nil {
			return fmt.Errorf("the decision must be audited to the verifiable ledger, len=%d", w.ledger.Len())
		}
		return nil
	})
	sc.Step(`^the action executes and the verdict is a deviation$`, func() error {
		if !w.out.Executed || w.out.Verdict != safety.VerdictDeviation {
			return fmt.Errorf("a mispredicted post-state must execute and verify as deviation: %+v", w.out)
		}
		if verify.AutoResolvable(w.out.Verdict) {
			return fmt.Errorf("a deviation must never be auto-resolvable (never-auto)")
		}
		return nil
	})
	sc.Step(`^an execution_log bound to the action id is recorded$`, func() error {
		for _, e := range w.ledger.Entries() {
			if strings.Contains(e.Decision, "exec-log") && e.ActionID == w.out.ActionID {
				return nil
			}
		}
		return fmt.Errorf("an executed mutation must record an execution_log bound to the action id (INV-07)")
	})

	// --- cost/budget spend guard (REQ-1211..1215) ---

	newCostAcct := func(cfg cost.Config, forcer cost.ShadowForcer, rec cost.TripRecorder) *cost.Accountant {
		a, err := cost.New(w.costStore, cfg, forcer, rec, cost.WithClock(costClock),
			cost.WithLogf(func(string, ...any) { w.costLogged = true }))
		if err != nil {
			panic(err)
		}
		return a
	}

	sc.Step(`^a cost breaker with a per-1k rate and no budget$`, func() error {
		w.costStore, w.costGate = cost.NewMemStore(), safety.NewActuatingChokepoint()
		w.costAcct = newCostAcct(cost.Config{DefaultRate: 1.0}, w.costGate, nil)
		return nil
	})
	sc.Step(`^two model completions are metered for a session$`, func() error {
		w.costAcct.AccrueLLM(ctx, "fast", "sess-A", 1000) // $1
		w.costAcct.AccrueLLM(ctx, "fast", "sess-A", 2000) // +$2
		return nil
	})
	sc.Step(`^the daily and session accumulators reflect the accrued spend and nothing is force-Shadowed$`, func() error {
		day, _ := w.costStore.Total(ctx, cost.BucketDay, costDay)
		sess, _ := w.costStore.Total(ctx, cost.BucketSession, "sess-A")
		if day != 3.0 || sess != 3.0 {
			return fmt.Errorf("accumulators wrong: day=%v session=%v want 3.0/3.0", day, sess)
		}
		if !w.costGate.MayActuate() || w.costAcct.Tripped(ctx) {
			return fmt.Errorf("no budget armed must not trip or force Shadow (may_actuate=%v tripped=%v)", w.costGate.MayActuate(), w.costAcct.Tripped(ctx))
		}
		return nil
	})

	sc.Step(`^a cost breaker with a small daily budget$`, func() error {
		w.costStore, w.costGate, w.costLedger = cost.NewMemStore(), safety.NewActuatingChokepoint(), audit.NewLedger()
		w.costAcct = newCostAcct(cost.Config{DefaultRate: 1.0, DailyBudgetUSD: 5.0}, w.costGate, costLedgerRec{l: w.costLedger})
		return nil
	})
	sc.Step(`^metered completions push the daily spend over the budget$`, func() error {
		w.costAcct.AccrueLLM(ctx, "fast", "sess-B", 4000) // $4 — under
		w.costAcct.AccrueLLM(ctx, "fast", "sess-B", 2000) // +$2 => $6 >= $5 => trip
		return nil
	})
	sc.Step(`^the cost breaker trips, forces the mode to Shadow, and records a cost trip to the ledger$`, func() error {
		if w.costGate.MayActuate() || !w.costAcct.Tripped(ctx) {
			return fmt.Errorf("over the daily budget must trip + force Shadow (may_actuate=%v tripped=%v)", w.costGate.MayActuate(), w.costAcct.Tripped(ctx))
		}
		for _, e := range w.costLedger.Entries() {
			if e.Decision == "cost:breaker-trip" {
				if w.costLedger.Verify() != nil {
					return fmt.Errorf("the cost trip ledger must verify (hash chain intact)")
				}
				return nil
			}
		}
		return fmt.Errorf("a cost trip must record a cost:breaker-trip decision to the ledger (INV-19)")
	})

	sc.Step(`^a cost breaker with a small session ceiling$`, func() error {
		w.costStore, w.costGate = cost.NewMemStore(), safety.NewActuatingChokepoint()
		w.costAcct = newCostAcct(cost.Config{DefaultRate: 1.0, SessionCeilingUSD: 2.0}, w.costGate, nil)
		return nil
	})
	sc.Step(`^a metered completion pushes one session over the ceiling$`, func() error {
		w.costAcct.AccrueLLM(ctx, "fast", "sess-hot", 3000) // $3 >= $2
		return nil
	})
	sc.Step(`^the cost breaker trips and forces the mode to Shadow$`, func() error {
		if w.costGate.MayActuate() || !w.costAcct.Tripped(ctx) {
			return fmt.Errorf("over the session ceiling must trip + force Shadow (may_actuate=%v tripped=%v)", w.costGate.MayActuate(), w.costAcct.Tripped(ctx))
		}
		return nil
	})

	sc.Step(`^two cost breakers sharing one durable cost store$`, func() error {
		w.costStore = cost.NewMemStore()
		w.costGate, w.sibCostGate = safety.NewActuatingChokepoint(), safety.NewActuatingChokepoint()
		w.costAcct = newCostAcct(cost.Config{DefaultRate: 1.0, DailyBudgetUSD: 1.0}, w.costGate, nil)
		// The sibling has a HUGE budget of its own — it would never trip on its own spend; it must honor the
		// shared OPEN state the first breaker sets.
		sib, err := cost.New(w.costStore, cost.Config{DefaultRate: 1.0, DailyBudgetUSD: 1_000_000}, w.sibCostGate, nil, cost.WithClock(costClock))
		if err != nil {
			return err
		}
		w.sibCostAcct = sib
		if !w.sibCostGate.MayActuate() {
			return fmt.Errorf("precondition: the sibling must be actuating before it honors the shared cost trip")
		}
		return nil
	})
	sc.Step(`^the first exceeds its budget and the sibling meters its next completion$`, func() error {
		w.costAcct.AccrueLLM(ctx, "fast", "sess-1", 2000) // $2 >= $1 => first trips, shared state OPEN
		if w.costGate.MayActuate() {
			return fmt.Errorf("the first worker must have force-Shadowed itself on its own trip")
		}
		w.sibCostAcct.AccrueLLM(ctx, "fast", "sess-2", 100) // sibling reads the shared OPEN state
		return nil
	})
	sc.Step(`^the sibling forces its own mode to Shadow from the shared open state$`, func() error {
		if w.sibCostGate.MayActuate() {
			return fmt.Errorf("the sibling must force its own mode to Shadow after reading the shared OPEN cost breaker")
		}
		return nil
	})

	sc.Step(`^a cost breaker with rates but no budget or ceiling$`, func() error {
		w.costStore, w.costGate = cost.NewMemStore(), safety.NewActuatingChokepoint()
		w.costAcct = newCostAcct(cost.Config{DefaultRate: 1.0}, w.costGate, nil)
		return nil
	})
	sc.Step(`^a very large spend is metered$`, func() error {
		w.costAcct.AccrueLLM(ctx, "fast", "sess-big", 1_000_000) // $1000
		return nil
	})
	sc.Step(`^the cost breaker never trips and the mode stays actuating$`, func() error {
		if w.costAcct.Tripped(ctx) || !w.costGate.MayActuate() {
			return fmt.Errorf("a disabled budget must never trip or force Shadow (tripped=%v may_actuate=%v)", w.costAcct.Tripped(ctx), w.costGate.MayActuate())
		}
		return nil
	})

	sc.Step(`^a cost breaker whose store cannot be read$`, func() error {
		w.costGate, w.costLogged = safety.NewActuatingChokepoint(), false
		acct, err := cost.New(costErrStore{}, cost.Config{DefaultRate: 1.0, DailyBudgetUSD: 0.01}, w.costGate, nil,
			cost.WithClock(costClock), cost.WithLogf(func(string, ...any) { w.costLogged = true }))
		if err != nil {
			return err
		}
		w.costAcct = acct
		return nil
	})
	sc.Step(`^a metered completion would otherwise exceed a tiny budget$`, func() error {
		w.costAcct.AccrueLLM(ctx, "fast", "sess-fail", 100000) // would be $100, but the store is down
		return nil
	})
	sc.Step(`^the cost breaker fails open, does not force Shadow, and logs the error$`, func() error {
		if !w.costGate.MayActuate() {
			return fmt.Errorf("a cost-store outage must FAIL OPEN — never force Shadow (spend guard, not a safety floor)")
		}
		if w.costAcct.Tripped(ctx) {
			return fmt.Errorf("Tripped must fail OPEN (false) on a store read error")
		}
		if !w.costLogged {
			return fmt.Errorf("a fail-open degradation must be LOGGED loudly, not silent")
		}
		return nil
	})
}
