package acceptance

// REQ-1515 (spec/015, T-015-8) — VERIFY-ON-AUTO.
//
// "WHEN the engine resolves an action to `auto`, the actuation SHALL still run the full
//  predict → execute → verify → breaker sequence (spec/013), and the engine SHALL NOT authorize an
//  `auto` execution whose post-state cannot be verified."
//
// This is the safety floor the graduation ladder (REQ-1514) stands on: graduation changes the APPROVAL
// verdict, never the VERIFICATION obligation. Without this oracle, "graduated to auto" and "no longer
// verified" are indistinguishable from the outside — and every later rung built on the ladder (the
// TG-227 earned-catalog AUTO_NOTICE rung) would inherit an unproven floor.
//
// The oracle drives the REAL chain end-to-end: a REAL policy.Engine resolving `auto` from operator rule
// data, wired into the REAL core/actuate.Interceptor over a REAL chokepoint, REAL mutation breaker, REAL
// audit ledger and REAL manifest lifecycle — with the interceptor's own observe-only gate trail
// (spec/020 REQ-2007) as the witness that each stage actually ran. Nothing about the sequence is
// simulated; only the effect leaf (fakeActuator) and the monitoring observer are test doubles, because
// executing a real systemctl and reading real alerts is not this leaf's subject.
//
// Three properties are proven together, because REQ-1515 fails if ANY of them is missing:
//   1. auto EXECUTES and still emits the verify gate with a mechanical verdict (verify was not skipped).
//   2. auto with NO committed prediction REFUSES at the structure gate (predict is not skipped).
//   3. auto with an UNVERIFIABLE post-state (no observer wired) REFUSES before executing
//      ("cannot verify ⇒ will not execute") — the second sentence of REQ-1515, verbatim.

import (
	"context"
	"fmt"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/trace"
	"github.com/territory-grounder/grounder/core/verify"
)

// countingActuator is the effect leaf stand-in: it counts executions so the oracle can assert that a
// refusal happened BEFORE the leaf ran, not after.
type countingActuator struct{ execs int }

func (a *countingActuator) Capability() string { return "test.mutating" }
func (a *countingActuator) ReadOnly() bool     { return false }
func (a *countingActuator) Exec(context.Context, []string, []byte) (actuation.Result, error) {
	a.execs++
	return actuation.Result{ExitCode: 0}, nil
}

// gateTrail is the observe-only witness (spec/020 REQ-2007). The interceptor emits one ordered row per
// gate as it resolves, so the trail is the honest record of WHICH stages ran — exactly what REQ-1515's
// "the full sequence runs" claim needs. It is a pure side effect: it cannot change any gate outcome.
type gateTrail struct{ rows []trace.GateVerdict }

func (g *gateTrail) Emit(_ context.Context, gv trace.GateVerdict) error {
	g.rows = append(g.rows, gv)
	return nil
}

func (g *gateTrail) verdictOf(gate string) (string, bool) {
	for _, r := range g.rows {
		if r.Gate == gate {
			return r.Verdict, true
		}
	}
	return "", false
}

// verifyOnAuto holds one run of the real chain.
type verifyOnAuto struct {
	trail   *gateTrail
	act     *countingActuator
	out     actuate.Outcome
	err     error
	decider *policy.Engine
}

// autoRuleEngine is a REAL policy engine whose operator rule data resolves the op-class to `auto`. The
// scenario's premise ("the engine resolves an action to auto") must come from the production engine, not
// from a hand-set verdict, or the oracle would prove nothing about the engine.
func autoRuleEngine() (*policy.Engine, error) {
	r, err := policy.NewRule(policy.Rule{
		ID:      "auto-restart",
		Match:   policy.Match{OpClass: "restart-service"},
		Verdict: policy.VerdictAuto,
	})
	if err != nil {
		return nil, err
	}
	return policy.NewEngine(context.Background(), policy.RuleSet{Rules: []policy.Rule{r}})
}

// armedInterceptor builds the real spec/013 chain in an ACTUATING posture with the real breaker armed, so
// the breaker stage is genuinely present rather than nil-skipped.
func (v *verifyOnAuto) armedInterceptor() (*actuate.Interceptor, error) {
	cp := safety.NewActuatingChokepoint()
	v.act = &countingActuator{}
	v.trail = &gateTrail{}
	mb, err := safety.NewMutationBreaker(cp, breaker.NewMemStore(), 1, nil)
	if err != nil {
		return nil, err
	}
	return actuate.NewInterceptor(cp, v.act, audit.NewLedger()).
		WithPolicyDecider(v.decider, func() policy.Mode { return policy.ModeFullAuto }).
		WithMutationBreaker(mb).
		WithGateVerdictSink(v.trail), nil
}

// autoRequest is a fully-admissible mutating request for a reversible, evidence-bound, AUTO-banded action.
// gated=false strips the committed prediction (property 2); observe=false strips the post-execution
// observer (property 3). Everything else is held identical so each property is isolated.
func autoRequest(gated, observe bool) (actuate.Request, error) {
	m, err := manifest.New(
		manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Reversible: true},
		safety.BandAuto, "plan#1515", "pred#1515")
	if err != nil {
		return actuate.Request{}, err
	}
	req := actuate.Request{
		Manifest:   m,
		Gated:      gated,
		Argv:       []string{"systemctl", "restart", "nginx"},
		Evidence:   []actuate.Evidence{{ToolResultID: "tr-1515", Captured: true, Successful: true, Recent: true, Relevant: true}},
		Prediction: verify.Prediction{PlanHash: "plan#1515", TargetHost: "web01"},
		Band:       safety.BandAuto,
		Confidence: 1.0,
	}
	// TG-166b: the execute-time necessity re-check (interceptor gate 4i). Set OUTSIDE the literal so the
	// fixture's existing field alignment is untouched. The fault is still present, so the gate passes.
	req.StillFaulted = func(context.Context) (bool, bool) { return true, true }
	if observe {
		req.Observe = func(context.Context) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{}, true
		}
	}
	return req, nil
}

func initializeREQ1515(sc *godog.ScenarioContext) {
	v := &verifyOnAuto{}

	sc.Step(`^the engine resolves an action to auto$`, func() error {
		eng, err := autoRuleEngine()
		if err != nil {
			return err
		}
		v.decider = eng
		// Prove the PREMISE from the real engine before relying on it: this scenario is only meaningful
		// if the engine genuinely resolves `auto` for this action.
		d, err := eng.Decide(context.Background(), policy.EvalInput{
			OpClass: "restart-service", Host: "web01", Reversible: true,
			Confidence: 1.0, Band: safety.BandAuto, Mode: policy.ModeFullAuto,
		})
		if err != nil {
			return err
		}
		if d.Verdict() != policy.VerdictAuto {
			return fmt.Errorf("precondition: the engine resolved %q, want auto — the scenario premise does not hold", d.Verdict())
		}
		return nil
	})

	sc.Step(`^the action actuates$`, func() error {
		i, err := v.armedInterceptor()
		if err != nil {
			return err
		}
		req, err := autoRequest(true, true)
		if err != nil {
			return err
		}
		v.out, v.err = i.Do(context.Background(), req)
		return v.err
	})

	sc.Step(`^the predict execute verify breaker sequence runs and an unverifiable post-state refuses$`, func() error {
		// --- 1. auto EXECUTED, and the full sequence ran. ---
		if !v.out.Executed {
			return fmt.Errorf("an `auto` verdict must execute: %+v", v.out)
		}
		if v.act.execs != 1 {
			return fmt.Errorf("the effect leaf must run exactly once under auto, got %d", v.act.execs)
		}
		// The predict (structure = committed prediction + action identity), breaker and verify stages must
		// each appear in the interceptor's own ordered trail. Their PRESENCE is the claim: `auto` did not
		// short-circuit the spec/013 sequence.
		for _, gate := range []string{"structure", "breaker", "verify"} {
			verdict, ok := v.trail.verdictOf(gate)
			if !ok {
				return fmt.Errorf("gate %q never ran under an `auto` verdict — the predict→execute→verify→breaker sequence was skipped (trail: %s)", gate, v.trail.gates())
			}
			if gate != "verify" && verdict != "pass" {
				return fmt.Errorf("gate %q resolved %q, want pass", gate, verdict)
			}
		}
		// The verify gate must carry a MECHANICAL verdict, not a placeholder: verification actually happened.
		vv, _ := v.trail.verdictOf("verify")
		switch safety.Verdict(vv) {
		case safety.VerdictMatch, safety.VerdictPartial, safety.VerdictDeviation:
		default:
			return fmt.Errorf("the verify gate produced %q, want a mechanical verdict (match/partial/deviation) — verification did not run", vv)
		}
		if v.out.Verdict == "" {
			return fmt.Errorf("an executed `auto` action must carry a verification verdict, got empty")
		}

		// --- 2. PREDICT is not optional under auto: no committed prediction ⇒ refuse at structure. ---
		{
			i, err := v.armedInterceptor()
			if err != nil {
				return err
			}
			req, err := autoRequest(false, true) // ungated
			if err != nil {
				return err
			}
			out, err := i.Do(context.Background(), req)
			if err != nil {
				return err
			}
			if out.Executed || v.act.execs != 0 {
				return fmt.Errorf("an `auto` verdict with NO committed prediction must refuse before execute: %+v execs=%d", out, v.act.execs)
			}
			if got, _ := v.trail.verdictOf("structure"); got != "refuse" {
				return fmt.Errorf("the ungated refusal must come from the structure (predict) gate, trail: %s", v.trail.gates())
			}
		}

		// --- 3. An UNVERIFIABLE post-state refuses — REQ-1515's second sentence, verbatim. ---
		{
			i, err := v.armedInterceptor()
			if err != nil {
				return err
			}
			req, err := autoRequest(true, false) // no observer wired
			if err != nil {
				return err
			}
			out, err := i.Do(context.Background(), req)
			if err != nil {
				return err
			}
			if out.Executed || v.act.execs != 0 {
				return fmt.Errorf("an `auto` execution whose post-state cannot be verified must NOT be authorized: %+v execs=%d", out, v.act.execs)
			}
			if got, ok := v.trail.verdictOf("verifiability"); !ok || got != "refuse" {
				return fmt.Errorf("the unverifiable refusal must come from the verifiability gate (got %q, ok=%v), trail: %s", got, ok, v.trail.gates())
			}
		}
		return nil
	})
}

// gates renders the ordered trail for failure messages — a refusal that names the wrong gate is a
// different defect from a refusal that never happened, and the message must distinguish them.
func (g *gateTrail) gates() string {
	out := ""
	for _, r := range g.rows {
		if out != "" {
			out += " → "
		}
		out += r.Gate + ":" + r.Verdict
	}
	if out == "" {
		return "(empty)"
	}
	return out
}
