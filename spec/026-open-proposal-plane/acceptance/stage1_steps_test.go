package acceptance

// Stage-1 step bindings (T-026-7). Four scenarios drive REAL code end-to-end:
//
//   REQ-2601  the full runner workflow (Temporal test env) with a scripted model whose op_class matches no
//             registry entry — the day-zero shape — asserting the terminal outcome and the recorded row.
//             ("Empty catalog" is realized as catalog-ABSENCE for this class: the registry is compiled-in,
//             and an unmatched slug takes the identical divert path an empty registry takes — the predicate
//             is opschema.Lookup failure, spec REQ-2603.)
//   REQ-2602  the ONE proposal grammar (core/proposal.Parse) + the structural manifest.Action exclusion.
//   REQ-2608  a sealed free-form action FORCE-ROUTED past the divert into the real interceptor and the real
//             empty-argv effect leaf (adapters/actuation.LocalReadOnly) — refusal + ledger, no new machinery.
//   REQ-2609  the same real workflow path with authored-stop actor evidence present — the proposal records
//             its op_class regardless: nothing in the plane gates or drops on actor evidence.
//
// RED mutation controls executed for these bindings (2026-07-31, restored green):
//   - grammar: UndoSketch copy dropped in core/proposal/parse.go → REQ-2602 step failed ("undo sketch
//     dropped at parse"). (Same control the backend task ran; re-executed against the godog binding.)
//   - workflow: the scripted model STOPS instead of proposing → REQ-2601 failed at outcome
//     ("proposed:shadow", got "stop:confident").
//   - chain: the same steps fed a REGISTERED restart-service action → the step failed on the refusal
//     SIGNATURE ("mutating actuation is disabled", not the argv contract) — proving the oracle demands
//     the empty-argv leaf specifically and can go red. (The leaf checks argv BEFORE the mutation gate,
//     so only a nil-sealed free-form action carries the argv signature.)

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/cucumber/godog"
	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
	runner "github.com/territory-grounder/grounder/temporal/runner"
)

func init() { stepRegistrars = append(stepRegistrars, registerStage1Steps) }

// ---------------------------------------------------------------------------
// shared fakes — the minimal exported-surface equivalents of the runner
// package's test doubles (scripted LLM, one read-only tool, manifest sink).
// ---------------------------------------------------------------------------

type scriptedCompleter struct {
	responses []string
	i         int
}

func (m *scriptedCompleter) Complete(_ context.Context, _, _ string, _ []model.Message) (string, error) {
	if m.i >= len(m.responses) {
		return `{"action":"stop","confidence":0.9}`, nil
	}
	r := m.responses[m.i]
	m.i++
	return r, nil
}

type evidenceTool struct{}

func (evidenceTool) Name() string   { return "get-logs" }
func (evidenceTool) ReadOnly() bool { return true }
func (evidenceTool) Invoke(_ context.Context, _ map[string]string) (agent.ToolResult, error) {
	return agent.ToolResult{ID: "tr-1", Tool: "get-logs", Target: "svc01", Output: "svc01 flux capacitor drifting; vzstop by root@pam 12:01Z", Success: true}, nil
}

type memManifests struct {
	byID map[string]*manifest.ActionManifest
}

func (m *memManifests) Seal(_ context.Context, mf *manifest.ActionManifest) error {
	if m.byID == nil {
		m.byID = map[string]*manifest.ActionManifest{}
	}
	m.byID[mf.ActionID] = mf
	return nil
}
func (m *memManifests) Get(_ context.Context, id string) (*manifest.ActionManifest, bool, error) {
	mf, ok := m.byID[id]
	return mf, ok, nil
}

// allowDecider is the permissive policy decider: the chain oracle must reach the EFFECT LEAF, so policy
// answers yes and the refusal we assert can only come from the leaf itself.
type allowDecider struct{}

func (allowDecider) Decide(_ context.Context, in policy.EvalInput) (policy.PolicyDecision, error) {
	return policy.NewPolicyDecision(policy.VerdictAuto, "acceptance-permissive", in.Band, nil, in.Mode,
		"acceptance chain oracle: policy must not be the refusing layer", policy.DecisionAudit{}), nil
}

func acceptanceDeps(responses ...string) runner.Deps {
	tools := agent.NewReadOnlyToolSet()
	_ = tools.Register(evidenceTool{})
	graph := predict.NewDependencyGraph(map[string][]string{"svc01": {"db01"}})
	gate := &predict.PredictionGate{
		Store: predict.NewMemPredictionStore(),
		Model: &predict.InfragraphModel{Graph: graph, DefaultRules: []string{"FluxDrift"}, MaxDepth: 3},
		Mode:  predict.ModeEnforce,
	}
	return runner.Deps{
		Model:              &scriptedCompleter{responses: responses},
		Tools:              tools,
		Limits:             agent.DefaultLimits(),
		PredictionEligible: func(string) bool { return true },
		Gate:               gate,
		Ledger:             audit.NewLedger(),
		Mutation:           safety.NewReadOnlyChokepoint(),
	}
}

func registerRunnerActivities(env interface{ RegisterActivity(interface{}) }, acts *runner.Activities) {
	env.RegisterActivity(acts.SuppressActivity)
	env.RegisterActivity(acts.CorrelateActivity) // the TG-169 pre-context correlation stage (workflow step 0.75)
	env.RegisterActivity(acts.InvestigateActivity)
	env.RegisterActivity(acts.AttributeActivity)
	env.RegisterActivity(acts.ClassifyActivity)
	env.RegisterActivity(acts.GateActivity)
	env.RegisterActivity(acts.NotifyActivity)
	env.RegisterActivity(acts.RecordVoteActivity)
	env.RegisterActivity(acts.ExecuteActivity)
	env.RegisterActivity(acts.VerifyActivity)
	env.RegisterActivity(acts.RecordTriageActivity)
	env.RegisterActivity(acts.ShadowProposalActivity)
	env.RegisterActivity(acts.MarkTriageClearedActivity)
}

// ---------------------------------------------------------------------------
// scenario state
// ---------------------------------------------------------------------------

type stage1State struct {
	// REQ-2601/2609 workflow run
	scripted []string
	result   runner.RunnerResult
	rows     []judge.TriageRow
	// REQ-2602 grammar
	parsed   proposal.Proposal
	parseErr error
	// REQ-2608 chain
	execResult runner.ExecuteResult
	execErr    error
	ledger     *audit.Ledger
	ledgerLen  int
}

func registerStage1Steps(sc *godog.ScenarioContext) {
	st := &stage1State{}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		*st = stage1State{}
		return ctx, nil
	})

	// ---- REQ-2601 -----------------------------------------------------------
	sc.Step(`^a real-path session against an empty op-class catalog$`, func() error {
		// Catalog absence for the proposed class: the slug matches no registry entry, which is the same
		// opschema.Lookup-miss predicate an entirely empty catalog exercises (REQ-2603's divert condition).
		st.scripted = []string{
			`{"action":"tool","tool":"get-logs","args":{"host":"svc01"},"confidence":0.7}`,
			`{"action":"propose","confidence":0.82,"proposal":{"external_ref":"TG-acc-1","target":"svc01",` +
				`"op_class":"rotate-flux-capacitor","op":"rotate","reversible":true,` +
				`"rationale":"observed drift on svc01 per OBSERVATION[tr-1]","undo_sketch":"rotate back one notch",` +
				`"evidence_ids":["tr-1"]}}`,
		}
		return nil
	})
	sc.Step(`^observations confirm an action-warranted fault$`, func() error { return nil }) // the scripted tool observation above
	sc.Step(`^the agent completes its triage$`, func() error {
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestWorkflowEnvironment()
		deps := acceptanceDeps(st.scripted...)
		deps.TriageRecord = func(_ context.Context, row judge.TriageRow) error {
			st.rows = append(st.rows, row)
			return nil
		}
		registerRunnerActivities(env, runner.NewActivities(deps))
		env.ExecuteWorkflow(runner.RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-acc-1",
			SourceID: "prometheus-dc1", AlertRule: "FluxDrift", Host: "svc01",
			Severity: ingest.SeverityWarning, Site: "dc1"})
		if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
			return fmt.Errorf("workflow did not complete cleanly: %v", env.GetWorkflowError())
		}
		return env.GetWorkflowResult(&st.result)
	})
	sc.Step(`^the session terminates with outcome "([^"]*)"$`, func(want string) error {
		if st.result.Outcome != want {
			return fmt.Errorf("outcome = %q, want %q", st.result.Outcome, want)
		}
		return nil
	})
	sc.Step(`^the triage row carries a free-form op_class naming the addressing action$`, func() error {
		if len(st.rows) == 0 {
			return fmt.Errorf("no triage row recorded")
		}
		row := st.rows[len(st.rows)-1]
		if row.OpClass != "rotate-flux-capacitor" {
			return fmt.Errorf("row op_class = %q, want the free-form slug rotate-flux-capacitor", row.OpClass)
		}
		return nil
	})

	// ---- REQ-2602 -----------------------------------------------------------
	sc.Step(`^a proposal whose op_class matches no registry entry$`, func() error {
		raw := `{"external_ref":"TG-acc-2","target":"svc01","op_class":"rotate-flux-capacitor","op":"rotate",` +
			`"reversible":true,"rationale":"drift","undo_sketch":"rotate back one notch","evidence_ids":["tr-1"]}`
		p, err := proposal.ParseProposal([]byte(raw))
		st.parsed, st.parseErr = p, err
		return nil
	})
	sc.Step(`^the proposal is parsed$`, func() error { return nil })
	sc.Step(`^parsing succeeds through the single proposal grammar$`, func() error {
		if st.parseErr != nil {
			return fmt.Errorf("the one grammar must accept a free-form op_class: %v", st.parseErr)
		}
		if st.parsed.Action.OpClass != "rotate-flux-capacitor" {
			return fmt.Errorf("parsed proposal lost the free-form op_class")
		}
		return nil
	})
	sc.Step(`^the undo sketch is available on the proposal record and absent from the manifest action$`, func() error {
		if st.parsed.UndoSketch != "rotate back one notch" {
			return fmt.Errorf("undo_sketch dropped at parse: %q", st.parsed.UndoSketch)
		}
		// Structural INV-07 assertion: manifest.Action has NO UndoSketch field — the sketch can never enter
		// action identity (a field addition would change every action id).
		if _, has := reflect.TypeOf(manifest.Action{}).FieldByName("UndoSketch"); has {
			return fmt.Errorf("manifest.Action gained an UndoSketch field — the sketch must never enter action identity")
		}
		return nil
	})

	// ---- REQ-2608 -----------------------------------------------------------
	sc.Step(`^a sealed action for an unregistered op_class routed to execution against the safety chain$`, func() error {
		st.ledger = audit.NewLedger()
		manifests := &memManifests{}
		// REAL interceptor over the REAL local effect leaf; mutation ON via a fixed authority and a
		// permissive policy — so the ONLY thing left to refuse is the leaf itself.
		cp := safety.NewChokepoint(safety.NewFixedModeAuthority(true))
		interceptor := actuate.NewInterceptor(
			cp,
			actuation.LocalReadOnly{Cap: "acceptance.chain"},
			st.ledger,
		).WithPolicyDecider(allowDecider{}, func() policy.Mode { return policy.ModeFullAuto })
		// prove the boot preflight over the wired chain so mode+preflight both permit actuation — the
		// SAME green a production Semi-auto worker requires; only the argv contract is left to refuse.
		if err := cp.ProvePreflight(interceptor); err != nil {
			return fmt.Errorf("preflight must prove over a wired chain: %v", err)
		}
		deps := acceptanceDeps()
		deps.Interceptor = interceptor
		deps.ManifestSink = manifests
		deps.Manifests = manifests
		deps.Mutation = cp
		// Satisfy the verdict-adjudication baseline + pre-anomaly gates (fakes that answer honestly-empty,
		// ok=true) — every named pre-leaf gate is now deliberately green; the leaf refusal stands alone.
		deps.PostStateObserve = func(_ context.Context, _, _ string) ([]verify.ObservedAlert, bool) {
			return nil, true
		}
		deps.OpenIncidents = func(_ context.Context, _ time.Time) (map[string]bool, bool) {
			return map[string]bool{}, true
		}
		// TG-166b: the interceptor's necessity gate re-checks at execute time that the fault is still there,
		// and refuses when the seam is unwired. This oracle needs EVERY pre-leaf gate green so the argv
		// contract is the only refusing layer left, so the probe reports the target still alerting.
		deps.ClearObserve = func(_ context.Context, _, _ string) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{{Host: "svc01", Rule: "FluxDrift", Site: "dc1"}}, true
		}
		acts := runner.NewActivities(deps)

		gateOut, err := acts.GateActivity(context.Background(), runner.GateInput{
			Proposal: proposal.Proposal{
				ExternalRef: "TG-acc-3",
				Action: manifest.Action{Target: "svc01", OpClass: "rotate-flux-capacitor", Op: "rotate",
					Reversible: true},
				Rationale:   "force-routed free-form action (REQ-2608 oracle)",
				EvidenceIDs: []string{"tr-1"},
			},
			Band: safety.BandAuto, PlanHash: "acc-chain", Site: "dc1",
		})
		if err != nil {
			return fmt.Errorf("gate seal failed before the chain could be exercised: %v", err)
		}
		st.ledgerLen = st.ledger.Len()
		// Every pre-leaf gate is deliberately satisfied — approval granted, auto band, cited captured
		// evidence — so the ONLY refusing layer left is the effect leaf's argv contract (REQ-2608's claim).
		st.execResult, st.execErr = acts.ExecuteActivity(context.Background(), runner.ExecuteInput{
			ActionID: gateOut.ActionID, ExternalRef: "TG-acc-3", PlanHash: "acc-chain",
			Site: "dc1", TargetHost: "svc01", Approved: true, Band: safety.BandAuto,
			EvidenceIDs: []string{"tr-1"},
			ToolResults: []agent.ToolResult{{ID: "tr-1", Tool: "get-logs", Target: "svc01",
				Output: "svc01 flux capacitor drifting", Success: true}},
		})
		return nil
	})
	sc.Step(`^the effect leaf receives the action$`, func() error { return nil })
	sc.Step(`^execution is refused on empty argv and the refusal is ledgered$`, func() error {
		if st.execResult.Executed {
			return fmt.Errorf("a free-form action EXECUTED — the never-executable chain is broken: %+v", st.execResult)
		}
		sig := st.execResult.Note
		if st.execErr != nil {
			sig = st.execErr.Error()
		}
		if !strings.Contains(sig, "argv") && !strings.Contains(sig, "no program") {
			return fmt.Errorf("refused, but NOT by the empty-argv leaf (got %q) — the refusal must come from the argv contract, not an earlier gate", sig)
		}
		if st.ledger.Len() <= st.ledgerLen {
			return fmt.Errorf("the refusal left no ledger entry (len %d -> %d)", st.ledgerLen, st.ledger.Len())
		}
		if err := st.ledger.Verify(); err != nil {
			return fmt.Errorf("ledger chain must verify after the refusal: %v", err)
		}
		return nil
	})

	// ---- REQ-2609 -----------------------------------------------------------
	sc.Step(`^observations confirming an action-warranted fault$`, func() error {
		return nil // the evidenceTool observation (which also carries the authored-stop trail) is scripted below
	})
	sc.Step(`^actor evidence showing an authored stop by a named actor$`, func() error {
		st.scripted = []string{
			`{"action":"tool","tool":"get-logs","args":{"host":"svc01"},"confidence":0.7}`,
			`{"action":"propose","confidence":0.82,"proposal":{"external_ref":"TG-acc-4","target":"svc01",` +
				`"op_class":"rotate-flux-capacitor","op":"rotate","reversible":true,` +
				`"rationale":"authored stop by root@pam per OBSERVATION[tr-1]; proposing the addressing fix regardless",` +
				`"undo_sketch":"rotate back one notch","evidence_ids":["tr-1"]}}`,
		}
		return nil
	})
	// "When the agent completes its triage" is shared with REQ-2601 above.
	sc.Step(`^the proposal names the addressing op-class$`, func() error {
		if len(st.rows) == 0 {
			return fmt.Errorf("no triage row recorded")
		}
		row := st.rows[len(st.rows)-1]
		if row.OpClass == "" {
			return fmt.Errorf("actor evidence suppressed the proposal: op_class empty on the recorded row")
		}
		if !strings.Contains(row.Conclusion, "root@pam") {
			return fmt.Errorf("the authored-stop evidence must be cited in the recorded rationale/conclusion, got %q", row.Conclusion)
		}
		return nil
	})
}
