package acceptance

import (
	"context"
	"fmt"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// T-020-7 binds REQ-2002: the decision tracer is OBSERVE-ONLY — replaying the SAME governed decision with
// tracing ON (a gate-verdict sink attached) and OFF (nil sink) must yield a byte-for-byte-identical interceptor
// Outcome AND reach the effect-leaf actuator the SAME number of times, so no trace emit ever changes a gate
// verdict or the mutation posture. The oracle drives the REAL interceptor.Do — with the policy decider AND mode
// chokepoint wired (so "the policy verdict" and "the mode chokepoint" the scenario names genuinely run) — twice
// per case over a counting fake actuator: once with a db.MemGateVerdictSink and once with none, for a passing
// (policy `auto`) decision AND a refused (policy `deny`) decision. It asserts the Outcomes are identical and the
// actuator exec counts are identical, and that a refused decision reaches the actuator ZERO times either way —
// the trace can never turn a refusal into an actuation. The gate-verdict sink is a pure side write; nothing reads
// its rows back into the decision (the tracing-ON runs record rows, proving tracing was genuinely on).
func init() {
	stepRegistrars = append(stepRegistrars, registerTraceInvarianceSteps)
}

// tiActuator is a minimal successful mutating actuator that COUNTS reaches, so "no trace emit reaches an
// actuator or changes actuation" is provable by comparing the count traced vs untraced. It never touches the estate.
type tiActuator struct{ execs int }

func (a *tiActuator) Capability() string { return "test" }
func (a *tiActuator) ReadOnly() bool     { return false }
func (a *tiActuator) Exec(context.Context, []string, []byte) (actuation.Result, error) {
	a.execs++
	return actuation.Result{ExitCode: 0}, nil
}
func (a *tiActuator) ExecLog(_ string, command []string) (forward, rollback []string, err error) {
	return command, command, nil
}

// tiDecider is a scripted policy decider so the policy-authorize gate genuinely runs in both the traced and the
// untraced interceptor — the scenario names "the policy verdict" as a gate that must behave identically.
type tiDecider struct{ verdict policy.Verdict }

func (d tiDecider) Decide(_ context.Context, in policy.EvalInput) (policy.PolicyDecision, error) {
	return policy.NewPolicyDecision(d.verdict, "rule-req2002", in.Band, nil, in.Mode, "scripted for the trace-invariance oracle", policy.DecisionAudit{}), nil
}

func tiObserved(context.Context) []verify.ObservedAlert { return []verify.ObservedAlert{} }

func tiManifest() *manifest.ActionManifest {
	m, _ := manifest.New(manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Reversible: true}, safety.BandAuto, "plan#ti", "pred#ti")
	return m
}

func tiEvidence() []actuate.Evidence {
	return []actuate.Evidence{{ToolResultID: "tr-1", Captured: true, Successful: true, Recent: true, Relevant: true}}
}

// tiRun drives the REAL interceptor.Do once. withSink toggles tracing on/off; verdict scripts the policy gate.
// Mode is Full-auto and the chokepoint actuates (mutation ON) in-test — this NEVER touches the live Shadow mode;
// it is a local interceptor. Returns the outcome, how many times the actuator was reached, and how many gate
// rows the trace recorded (0 when tracing is off).
func tiRun(withSink bool, verdict policy.Verdict) (out actuate.Outcome, execs, sinkRows int) {
	act := &tiActuator{}
	i := actuate.NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
		WithPolicyDecider(tiDecider{verdict: verdict}, func() policy.Mode { return policy.ModeFullAuto })
	var sink *db.MemGateVerdictStore
	if withSink {
		sink = db.NewMemGateVerdictStore()
		i = i.WithGateVerdictSink(sink)
	}
	m := tiManifest()
	req := actuate.Request{
		Manifest:    m,
		ExternalRef: "ext-req2002",
		Gated:       true,
		Argv:        []string{"systemctl", "restart", "nginx"},
		Evidence:    tiEvidence(),
		Observe:     tiObserved,
		Band:        safety.BandAuto,
	}
	o, err := i.Do(context.Background(), req)
	if err != nil {
		panic("interceptor Do returned an error (unwired chain): " + err.Error())
	}
	if sink != nil {
		sinkRows = len(sink.GateRows(m.ActionID))
	}
	return o, act.execs, sinkRows
}

type traceInvarianceWorld struct {
	execTraced, execUntraced actuate.Outcome
	execTX, execUX, execRows int
	refTraced, refUntraced   actuate.Outcome
	refTX, refUX, refRows    int
}

func registerTraceInvarianceSteps(sc *godog.ScenarioContext) {
	w := &traceInvarianceWorld{}

	sc.Step(`^the same decision replayed with tracing on and with tracing off$`, func() error {
		// A passing decision (policy auto) and a refused decision (policy deny), each replayed traced and untraced.
		w.execTraced, w.execTX, w.execRows = tiRun(true, policy.VerdictAuto)
		w.execUntraced, w.execUX, _ = tiRun(false, policy.VerdictAuto)
		w.refTraced, w.refTX, w.refRows = tiRun(true, policy.VerdictDeny)
		w.refUntraced, w.refUX, _ = tiRun(false, policy.VerdictDeny)
		return nil
	})

	sc.Step(`^the never-auto floor the policy verdict the credential resolution and the mode chokepoint run$`, func() error {
		// The comparison only proves something if the gate path GENUINELY ran: the passing decision must have
		// executed (past every gate) and the refused decision must have been refused at the policy verdict.
		if !w.execTraced.Executed {
			return fmt.Errorf("the passing decision did not execute (%+v) — the gate path did not run, so trace-invariance proves nothing", w.execTraced)
		}
		if !w.refTraced.Refused {
			return fmt.Errorf("the refused decision did not refuse (%+v) — the policy verdict gate did not gate", w.refTraced)
		}
		// Tracing was genuinely ON in the traced runs (they recorded gate rows) — otherwise on-vs-off is vacuous.
		if w.execRows == 0 || w.refRows == 0 {
			return fmt.Errorf("a tracing-ON run recorded no gate rows (exec=%d refuse=%d) — tracing was not actually on", w.execRows, w.refRows)
		}
		return nil
	})

	sc.Step(`^every gate behaves identically whether or not the decision is traced and no tracer emit or read path reaches an actuator or returns a value that gates actuation$`, func() error {
		// Passing decision: identical outcome AND identical actuator reach traced vs untraced.
		if w.execTraced != w.execUntraced {
			return fmt.Errorf("the passing decision differs traced vs untraced:\n  traced=  %+v\n  untraced=%+v\nthe trace changed a gate outcome", w.execTraced, w.execUntraced)
		}
		if w.execTX != w.execUX {
			return fmt.Errorf("the passing decision reached the actuator a different number of times traced (%d) vs untraced (%d) — the trace changed actuation", w.execTX, w.execUX)
		}
		// Refused decision: identical outcome AND identical actuator reach traced vs untraced.
		if w.refTraced != w.refUntraced {
			return fmt.Errorf("the refused decision differs traced vs untraced:\n  traced=  %+v\n  untraced=%+v\nthe trace changed the refusal", w.refTraced, w.refUntraced)
		}
		if w.refTX != w.refUX {
			return fmt.Errorf("the refused decision reached the actuator a different number of times traced (%d) vs untraced (%d)", w.refTX, w.refUX)
		}
		// The mutation-posture guard: a refused decision must reach the actuator ZERO times, traced or not — a
		// trace can never turn a refusal into an actuation.
		if w.refTX != 0 || w.refUX != 0 {
			return fmt.Errorf("a refused decision reached the actuator (traced=%d untraced=%d) — a refusal must never actuate", w.refTX, w.refUX)
		}
		return nil
	})
}
