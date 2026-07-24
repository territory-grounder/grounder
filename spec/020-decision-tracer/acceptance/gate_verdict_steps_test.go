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
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/trace"
	"github.com/territory-grounder/grounder/core/verify"
)

// T-020-7 binds the two REQ-2007 scenarios: the per-interceptor-gate verdict trail leaves one ordered row per
// gate for a passing action, and the refusing gate's row with NO phantom pass rows past it for a refused action.
// Both drive the REAL interceptor.Do with a db.MemGateVerdictSink — a passing (actuating chokepoint) request and
// a request refused at the evidence gate — asserting the emitted ordered rows. Observe-only: the emit is a pure
// side effect (the existing core/actuate + spec/013 tests, which wire no sink, pass unchanged).
// REQ-2001 (every boundary emits) and REQ-2016 (both agent_step AND gate_verdict deny UPDATE/DELETE) span sites/
// tables T-020-8 owns, so they stay @pending until that lands.
func init() {
	stepRegistrars = append(stepRegistrars, registerGateVerdictSteps)
}

// gvFakeActuator is a minimal successful mutating actuator (mirrors the spec/013 fake) — used ONLY to exercise
// the interceptor's pass path in-test; it never touches the estate.
type gvFakeActuator struct{}

func (gvFakeActuator) Capability() string { return "test" }
func (gvFakeActuator) ReadOnly() bool     { return false }
func (gvFakeActuator) Exec(context.Context, []string, []byte) (actuation.Result, error) {
	return actuation.Result{ExitCode: 0}, nil
}
func (gvFakeActuator) ExecLog(_ string, command []string) (forward, rollback []string, err error) {
	return command, command, nil
}

func gvObserved(context.Context) []verify.ObservedAlert { return []verify.ObservedAlert{} }

func gvReversibleManifest() *manifest.ActionManifest {
	m, _ := manifest.New(manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Reversible: true}, safety.BandAuto, "plan#gv", "pred#gv")
	return m
}

func gvBoundEvidence() []actuate.Evidence {
	return []actuate.Evidence{{ToolResultID: "tr-1", Captured: true, Successful: true, Recent: true, Relevant: true}}
}

type gateVerdictWorld struct {
	sink     *db.MemGateVerdictStore
	actionID string
	out      actuate.Outcome
	refused  bool
}

func registerGateVerdictSteps(sc *godog.ScenarioContext) {
	w := &gateVerdictWorld{}

	build := func(withEvidence bool) {
		w.sink = db.NewMemGateVerdictStore()
		// Actuating chokepoint so the mode gate passes in-test (does NOT touch the live Shadow mode); a successful
		// fake actuator; a fresh ledger. No decider/breaker wired ⇒ those gates are skipped (pass-through).
		i := actuate.NewInterceptor(safety.NewActuatingChokepoint(), gvFakeActuator{}, audit.NewLedger()).
			WithGateVerdictSink(w.sink)
		m := gvReversibleManifest()
		w.actionID = m.ActionID
		req := actuate.Request{
			Manifest:    m,
			ExternalRef: "ext-req2007",
			Gated:       true,
			Argv:        []string{"systemctl", "restart", "nginx"}, // non-empty ⇒ structure-schema gate skipped
			Observe:     gvObserved,
			Band:        safety.BandAuto,
		}
		if withEvidence {
			req.Evidence = gvBoundEvidence()
		} // else: unbound evidence ⇒ refused at the evidence gate (after admission/never-auto/structure pass)
		out, err := i.Do(context.Background(), req)
		if err != nil {
			panic("interceptor Do returned an error (unwired chain): " + err.Error())
		}
		w.out = out
		w.refused = out.Refused
	}

	sc.Step(`^an action that passes every interceptor gate$`, func() error { build(true); return nil })
	sc.Step(`^an action that is refused at one interceptor gate$`, func() error { build(false); return nil })
	sc.Step(`^the interceptor chain runs$`, func() error {
		if w.refused {
			return fmt.Errorf("passing action was refused: %s", w.out.Reason)
		}
		if !w.out.Executed {
			return fmt.Errorf("passing action did not execute (outcome %+v)", w.out)
		}
		return nil
	})
	sc.Step(`^the interceptor chain runs and returns at the refusal$`, func() error {
		if !w.refused {
			return fmt.Errorf("expected a refusal, action executed: %+v", w.out)
		}
		return nil
	})

	// REQ-2007 (passing): one ordered row per gate, keyed by both correlation keys, in gate order.
	sc.Step(`^the tracer persists one ordered interceptor_gate_verdict row per gate keyed by both action_id and external_ref in gate order$`, func() error {
		rows := w.sink.GateRows(w.actionID)
		want := []string{"admission", "never-auto-floor", "structure", "evidence", "territory", "verifiability", "mode-chokepoint", "execute", "verify"}
		if len(rows) != len(want) {
			return fmt.Errorf("gate rows: got %d, want %d (%v)", len(rows), len(want), gateNames(rows))
		}
		for idx, r := range rows {
			if r.Ordinal != idx+1 {
				return fmt.Errorf("row %d ordinal = %d, want %d (ordinals must be strictly increasing in gate order)", idx, r.Ordinal, idx+1)
			}
			if r.Gate != want[idx] {
				return fmt.Errorf("row %d gate = %q, want %q (gate order)", idx, r.Gate, want[idx])
			}
			if r.ActionID != w.actionID || r.ExternalRef != "ext-req2007" {
				return fmt.Errorf("row %d keys = (%q,%q), want (%q,ext-req2007) — must key by BOTH correlation keys", idx, r.ActionID, r.ExternalRef, w.actionID)
			}
			// every gate up to execute is a pass; verify carries the mechanical verdict.
			if r.Gate == "verify" {
				if r.Verdict == "" {
					return fmt.Errorf("verify row carries an empty verdict")
				}
			} else if r.Verdict != "pass" {
				return fmt.Errorf("gate %q verdict = %q, want pass", r.Gate, r.Verdict)
			}
		}
		return nil
	})

	// REQ-2007 (refusing): the refusing gate's row present, NO phantom pass rows for gates past it.
	sc.Step(`^the tracer persists the refusing gate's row and no phantom pass rows for gates past the refusal$`, func() error {
		rows := w.sink.GateRows(w.actionID)
		// The refusal is at the evidence gate: admission/never-auto-floor/structure pass, then evidence refuses.
		wantPrefix := []string{"admission", "never-auto-floor", "structure", "evidence"}
		if len(rows) != len(wantPrefix) {
			return fmt.Errorf("refusing trail: got %d rows (%v), want exactly %d ending at the refusing gate — extra rows would be phantom", len(rows), gateNames(rows), len(wantPrefix))
		}
		for idx, r := range rows {
			if r.Gate != wantPrefix[idx] || r.Ordinal != idx+1 {
				return fmt.Errorf("row %d = (%q,ord %d), want (%q,ord %d)", idx, r.Gate, r.Ordinal, wantPrefix[idx], idx+1)
			}
		}
		last := rows[len(rows)-1]
		if last.Gate != "evidence" || last.Verdict != "refuse" {
			return fmt.Errorf("last row = (%q,%q), want (evidence,refuse) — the refusing gate's row", last.Gate, last.Verdict)
		}
		// No row for any gate past the refusal.
		for _, r := range rows {
			for _, past := range []string{"territory", "verifiability", "policy", "breaker", "mode-chokepoint", "execute", "verify"} {
				if r.Gate == past {
					return fmt.Errorf("phantom row for gate %q past the refusal", past)
				}
			}
		}
		return nil
	})
}

func gateNames(rows []trace.GateVerdict) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Gate
	}
	return out
}
