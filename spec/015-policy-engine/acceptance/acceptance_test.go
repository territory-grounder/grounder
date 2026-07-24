package acceptance

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
)

// modeAuthz / modePreflight are the injected seams for the mode-transition scenarios (T-015-4). They are
// trivial always-admit / always-green fakes — the real RBAC (spec/006) and boot preflight (spec/013) are
// separate leaves; the mode state machine is driven standalone here.
type modeAuthz struct{}

func (modeAuthz) HasModeChangeAuthority(context.Context, string) error { return nil }

type modePreflight struct{}

func (modePreflight) PreflightGreen(context.Context) error { return nil }

// modeRedPreflight is the not-green boot preflight for the WIRED-RBAC escalation-refusal scenario (REQ-1502).
type modeRedPreflight struct{}

func (modeRedPreflight) PreflightGreen(context.Context) error {
	return errors.New("boot preflight not green")
}

// world is the per-scenario state driving the REAL core/policy engine + rule model. Only the T-015-1 and
// T-015-2 scenarios are bound here (the fixed-Rego evaluator, deny-overrides, the verdict trinary, and param
// inheritance); every other scenario in the feature stays @pending until its owning leaf lands.
type world struct {
	engine  *policy.Engine
	in      policy.EvalInput
	dec     policy.PolicyDecision
	verdict policy.Verdict

	ruleSet   policy.RuleSet
	effective policy.Params

	// T-015-3 confidence clamp + rate governor.
	refineGov    *policy.RateGovernor
	refineParams policy.Params
	refineRec    policy.RefineRecord

	// T-015-4 mode state machine.
	modeCtl    *policy.ModeController
	modeLedger *audit.Ledger
	activeMode policy.Mode

	// T-015-4 WIRED mode-transition RBAC (REQ-1502): the production ModeTransitionAuthority + audited transition.
	modeActor      string
	modeResultMode policy.Mode
	modeResultErr  error

	// T-015-6 band composition.
	bandPolicyVerdict policy.Verdict
	bandInput         safety.Band
	bandMode          policy.BandMode
	bandComposed      policy.Verdict
	bandRecord        policy.ComposeRecord

	// T-015-8 graduation ladder.
	ladder    *policy.Ladder
	gradClass string
	gradV     policy.Verdict

	// T-015-7 execution deny-floor + templates.
	floor          *policy.Floor
	floorProposal  policy.PolicyDecision
	floorExec      policy.Verdict
	floorRec       policy.FloorRecord
	tmplRuleSet    policy.RuleSet
	floorChange    policy.FloorChangeRecord
	floorChangeErr error

	// T-015-9 approve_by enforcement on /v1/vote.
	voteResolver  policy.PrincipalResolver
	voteActor     string
	voteApproveBy []string
	voteAllowed   bool
	voteDec       policy.ApproveDecision

	// T-015-11 warn-don't-block guard + audit-every-decision + admin engine toggle.
	allowAllFloor *policy.Floor
	allowAllAck   policy.Warning
	allowAllRS    policy.RuleSet
	allowAllErr   error
	allowAllWarns []policy.PolicyWarning
	auditLedger   *audit.Ledger
	auditedEng    *policy.AuditedEngine
	auditDec      policy.PolicyDecision
	toggle        *policy.EngineToggle
	toggleLedger  *audit.Ledger
	forceOnRec    policy.ToggleRecord
	disableRec    policy.ToggleRecord

	// T-015-12 persistence: the policy_decision append-only migration DDL.
	migrationDDL string

	// T-015-5 identical-pipeline invariant (mode governs ONLY the actuation branch).
	pipelineFactory policy.DeciderFactory
	pipelineIn      policy.EvalInput
	pipelineErr     error

	// REQ-1522 decision-surface hardening: bundle version on every decision + the full matched-rule list.
	hardenEng   *policy.Engine
	hardenAllow policy.PolicyDecision
	hardenDeny  policy.PolicyDecision
}

func hostSel(host string) *credential.Selector {
	s := credential.Selector{Kind: credential.KindHost, Pattern: host}
	return &s
}

func f64(v float64) *float64 { return &v }

// TestPolicyEngineAcceptance runs the spec/015 acceptance feature. @pending scenarios (the not-yet-built
// mode/band/graduation/identity/persistence behavior) are excluded; the executed set drives real
// core/policy code and must pass strictly.
func TestPolicyEngineAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/015 policy-engine",
		ScenarioInitializer: initializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"."},
			Tags:     "~@pending",
			Strict:   true,
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("spec/015 acceptance scenarios failed")
	}
}

func initializeScenario(sc *godog.ScenarioContext) {
	w := &world{}

	// --- REQ-1503: operator rules enter the FIXED Rego module as data only ---
	sc.Step(`^the fixed audited Rego evaluator module and an operator rule list$`, func() error {
		// The operator supplies a rule as DATA (a verdict + a shared-object-model selector) — no Rego.
		r, err := policy.NewRule(policy.Rule{ID: "op-1", Match: policy.Match{Selector: hostSel("h1")}, Verdict: policy.VerdictAuto})
		if err != nil {
			return err
		}
		w.engine, err = policy.NewEngine(context.Background(), policy.RuleSet{Rules: []policy.Rule{r}})
		if err != nil {
			return err
		}
		// A reversible, high-confidence, auto-banded, non-floor action so the composed Decide carries the data
		// rule's `auto` verdict all the way through — proving the fixed module consumed the operator DATA.
		w.in = policy.EvalInput{Host: "h1", Reversible: true, Confidence: 1.0, Band: safety.BandAuto}
		return nil
	})

	// --- REQ-1506: a rule resolves an action to one of auto/approve/deny (shares the "evaluates an action" verb) ---
	sc.Step(`^an action matched by a single rule$`, func() error {
		r, err := policy.NewRule(policy.Rule{ID: "single", Match: policy.Match{Selector: hostSel("h1")}, Verdict: policy.VerdictApprove})
		if err != nil {
			return err
		}
		w.engine, err = policy.NewEngine(context.Background(), policy.RuleSet{Rules: []policy.Rule{r}})
		if err != nil {
			return err
		}
		w.in = policy.EvalInput{Host: "h1"}
		return nil
	})

	evaluate := func() error {
		// The COMPOSED Engine.Decide runs the full most-restrictive-first chain (Rego → confidence/rate → band →
		// graduation → floor) and returns the required-field PolicyDecision. The scenarios read the composed
		// verdict and, for the confidence scenario, the refine sub-record from the decision's audit projection.
		d, err := w.engine.Decide(context.Background(), w.in)
		if err != nil {
			return err
		}
		w.dec = d
		w.verdict = d.Verdict()
		w.refineRec = d.Audit().Refine
		return nil
	}
	// Two scenarios phrase the same verb slightly differently ("an action" / "the action"); one grammar.
	sc.Step(`^the engine evaluates an action$`, evaluate)
	sc.Step(`^the engine evaluates the action$`, evaluate)

	sc.Step(`^the operator rules were supplied as input data and no operator-authored Rego was compiled$`, func() error {
		// The decision was produced from a data rule ...
		if w.verdict != policy.VerdictAuto {
			return fmt.Errorf("data rule did not drive the fixed module: got %q", w.verdict)
		}
		// ... and the ONLY compiled module is the embedded fixed tg.policy module (there is no exported API
		// on core/policy that accepts operator-authored Rego — rules enter only as data).
		if !strings.Contains(policy.RegoModule(), "package tg.policy") {
			return fmt.Errorf("compiled module is not the fixed embedded tg.policy module")
		}
		return nil
	})

	sc.Step(`^the verdict is one of "auto" "approve" or "deny"$`, func() error {
		switch w.verdict {
		case policy.VerdictAuto, policy.VerdictApprove, policy.VerdictDeny:
			return nil
		default:
			return fmt.Errorf("verdict %q is not one of the closed trinary", w.verdict)
		}
	})

	// --- REQ-1504: a matching deny wins regardless of rule order ---
	sc.Step(`^a rule list where a deny rule and a permissive auto rule both match an action$`, func() error {
		w.in = policy.EvalInput{Host: "h1"}
		auto, err := policy.NewRule(policy.Rule{ID: "auto", Match: policy.Match{Selector: hostSel("h1")}, Verdict: policy.VerdictAuto})
		if err != nil {
			return err
		}
		deny, err := policy.NewRule(policy.Rule{ID: "deny", Match: policy.Match{Selector: hostSel("h1")}, Verdict: policy.VerdictDeny})
		if err != nil {
			return err
		}
		w.ruleSet = policy.RuleSet{Rules: []policy.Rule{auto, deny}}
		return nil
	})

	sc.Step(`^the engine evaluates the action under any permutation of the rule order$`, func() error {
		// Assert the property under BOTH orderings (deny-overrides is order-independent by construction).
		orders := [][]policy.Rule{
			{w.ruleSet.Rules[0], w.ruleSet.Rules[1]},
			{w.ruleSet.Rules[1], w.ruleSet.Rules[0]},
		}
		for _, ord := range orders {
			e, err := policy.NewEngine(context.Background(), policy.RuleSet{Rules: ord})
			if err != nil {
				return err
			}
			d, err := e.Decide(context.Background(), w.in)
			if err != nil {
				return err
			}
			if d.Verdict() != policy.VerdictDeny {
				return fmt.Errorf("permutation %v resolved %q, want deny", ruleIDs(ord), d.Verdict())
			}
			w.verdict = d.Verdict()
		}
		return nil
	})

	sc.Step(`^the resolved verdict is "deny"$`, func() error {
		if w.verdict != policy.VerdictDeny {
			return fmt.Errorf("resolved verdict = %q, want deny", w.verdict)
		}
		return nil
	})

	// --- REQ-1507: an unset rule param inherits from the global-default rule ---
	sc.Step(`^a rule whose min_confidence is unset and a global-default rule that sets it$`, func() error {
		leaf, err := policy.NewRule(policy.Rule{ID: "leaf", Match: policy.Match{Selector: hostSel("h1")}, Verdict: policy.VerdictAuto})
		if err != nil {
			return err
		}
		w.ruleSet = policy.RuleSet{
			Default: policy.Params{MinConfidence: f64(0.82)}, // the global-default sets min_confidence
			Rules:   []policy.Rule{leaf},                     // the leaf leaves it unset
		}
		return nil
	})

	sc.Step(`^the engine resolves the rule params$`, func() error {
		w.effective = w.ruleSet.EffectiveParams(w.ruleSet.Rules[0])
		return nil
	})

	sc.Step(`^the rule uses the global-default min_confidence$`, func() error {
		if w.effective.MinConfidence == nil || *w.effective.MinConfidence != 0.82 {
			return fmt.Errorf("resolved min_confidence = %v, want the global-default 0.82", w.effective.MinConfidence)
		}
		return nil
	})

	// --- REQ-1500: exactly one of the four modes is active (T-015-4) ---
	sc.Step(`^the engine in any of the four global modes$`, func() error {
		w.modeLedger = audit.NewLedger()
		w.modeCtl = policy.NewModeController(context.Background(), policy.NewMemModeStore(), w.modeLedger, modeAuthz{}, modePreflight{}, nil)
		return nil
	})
	sc.Step(`^the active mode is queried$`, func() error {
		w.activeMode = w.modeCtl.Current()
		return nil
	})
	sc.Step(`^exactly one of "Shadow" "HITL" "Semi-auto" or "Full-auto" is active$`, func() error {
		switch w.activeMode {
		case policy.ModeShadow, policy.ModeHITL, policy.ModeSemiAuto, policy.ModeFullAuto:
			// A fresh controller over an absent store is exactly-one-active and fail-closed to Shadow.
			if w.activeMode != policy.ModeShadow {
				return fmt.Errorf("fresh controller active mode = %v, want the fail-closed Shadow", w.activeMode)
			}
			return nil
		default:
			return fmt.Errorf("active mode %v is not one of the four closed-enum modes", w.activeMode)
		}
	})

	// --- REQ-1502: a mode transition is gated and audited to the ledger (T-015-4) ---
	sc.Step(`^an authenticated operator with mode-change authority$`, func() error {
		w.modeLedger = audit.NewLedger()
		w.modeCtl = policy.NewModeController(context.Background(), policy.NewMemModeStore(), w.modeLedger, modeAuthz{}, modePreflight{}, nil)
		return nil
	})
	sc.Step(`^the operator changes the active mode$`, func() error {
		return w.modeCtl.Transition(context.Background(), policy.ModeShadow, policy.ModeSemiAuto, "operator:alice", "canary window")
	})
	sc.Step(`^the transition is gated and a mode-transition record is appended to the governance ledger before the new mode takes effect$`, func() error {
		if w.modeCtl.Current() != policy.ModeSemiAuto {
			return fmt.Errorf("mode did not take effect: active = %v", w.modeCtl.Current())
		}
		entries := w.modeLedger.Entries()
		if len(entries) != 1 {
			return fmt.Errorf("ledger has %d records, want exactly one mode-transition record", len(entries))
		}
		if entries[0].Decision != "mode-transition:Shadow->Semi-auto" {
			return fmt.Errorf("ledger decision = %q, want the mode-transition record", entries[0].Decision)
		}
		// The record is hash-chained into the tamper-evident ledger and re-walks clean.
		return w.modeLedger.Verify()
	})

	// --- REQ-1502: the WIRED production RBAC (core/policy.ModeTransitionAuthority) gates + audits an
	//     owner-invoked flip. Unlike the modeAuthz always-admit fake above, these scenarios drive the REAL
	//     authority the worker binds: a flip-authorized operator is admitted, an unauthorized operator is
	//     denied + audited, and a red preflight refuses the escalation. ---
	setupWiredAuthority := func(pf policy.PreflightChecker, actor string) {
		w.modeLedger = audit.NewLedger()
		w.modeCtl = policy.NewModeController(context.Background(), policy.NewMemModeStore(), w.modeLedger,
			policy.NewModeTransitionAuthority([]string{"kyriakosp"}), pf, nil)
		w.modeActor = actor
	}
	sc.Step(`^the production mode-transition authority admits the operator "([^"]*)"$`, func(op string) error {
		setupWiredAuthority(modePreflight{}, op)
		return nil
	})
	sc.Step(`^the production mode-transition authority does not admit the operator "([^"]*)"$`, func(op string) error {
		setupWiredAuthority(modePreflight{}, op)
		return nil
	})
	sc.Step(`^the production mode-transition authority admits the operator "([^"]*)" but the boot preflight is not green$`, func(op string) error {
		setupWiredAuthority(modeRedPreflight{}, op)
		return nil
	})
	wiredFlip := func() error {
		w.modeResultMode, w.modeResultErr = policy.AuditedModeTransition(context.Background(), w.modeCtl,
			policy.ModeShadow, policy.ModeFullAuto, w.modeActor, "owner-present canary GO")
		return nil
	}
	sc.Step(`^that operator transitions the mode from Shadow to Full-auto with a green preflight$`, wiredFlip)
	sc.Step(`^that operator attempts to transition the mode from Shadow to Full-auto$`, wiredFlip)
	sc.Step(`^that operator attempts to escalate the mode into Full-auto$`, wiredFlip)
	sc.Step(`^the mode is Full-auto and the mode-transition record is appended to the governance ledger$`, func() error {
		if w.modeResultErr != nil {
			return fmt.Errorf("authorized flip with a green preflight was refused: %v", w.modeResultErr)
		}
		if w.modeResultMode != policy.ModeFullAuto || w.modeCtl.Current() != policy.ModeFullAuto {
			return fmt.Errorf("mode after an authorized flip = %v, want Full-auto", w.modeCtl.Current())
		}
		e := w.modeLedger.Entries()
		if len(e) != 1 || e[0].Decision != "mode-transition:Shadow->Full-auto" {
			return fmt.Errorf("the flip was not audited exactly once: %+v", e)
		}
		return w.modeLedger.Verify()
	})
	sc.Step(`^the transition is refused and the mode stays Shadow and a mode-transition-refused record is appended to the governance ledger$`, func() error {
		if !errors.Is(w.modeResultErr, policy.ErrUnauthorizedModeChange) {
			return fmt.Errorf("an unauthorized flip err = %v, want ErrUnauthorizedModeChange", w.modeResultErr)
		}
		if w.modeCtl.Current() != policy.ModeShadow {
			return fmt.Errorf("the mode changed on a denied flip: %v", w.modeCtl.Current())
		}
		e := w.modeLedger.Entries()
		if len(e) != 1 || e[0].Decision != "mode-transition-refused:Shadow->Full-auto" {
			return fmt.Errorf("the denial was not audited: %+v", e)
		}
		return w.modeLedger.Verify()
	})
	sc.Step(`^the escalation is refused and the mode stays Shadow$`, func() error {
		if !errors.Is(w.modeResultErr, policy.ErrPreflightNotGreen) {
			return fmt.Errorf("a red-preflight escalation err = %v, want ErrPreflightNotGreen", w.modeResultErr)
		}
		if w.modeCtl.Current() != policy.ModeShadow {
			return fmt.Errorf("the mode escalated despite a red preflight: %v", w.modeCtl.Current())
		}
		return nil
	})

	// --- REQ-1519: an absent persisted mode fails closed to Shadow (T-015-4) ---
	sc.Step(`^the persisted mode is absent or unreadable$`, func() error {
		// An empty MemModeStore has never persisted a mode → Load returns ErrModeAbsent.
		w.modeCtl = policy.NewModeController(context.Background(), policy.NewMemModeStore(), audit.NewLedger(), modeAuthz{}, modePreflight{}, nil)
		return nil
	})
	sc.Step(`^the engine resolves the active mode$`, func() error {
		w.activeMode = w.modeCtl.ResolveMode(context.Background())
		return nil
	})
	sc.Step(`^the active mode is "Shadow"$`, func() error {
		if w.activeMode != policy.ModeShadow {
			return fmt.Errorf("resolved active mode = %v, want the fail-closed Shadow", w.activeMode)
		}
		return nil
	})

	// --- REQ-1520: the mode is the SOLE actuation chokepoint that absorbs the retired gate (T-015-13) ---
	sc.Step(`^no TG_MUTATION_ENABLED knob and no standalone toggle and no separate MutationGate object$`, func() error {
		// Negative control (no live dual-state): the retired gate symbols must not exist as LIVE code anywhere
		// in the runtime tree — only historical comments / ADR text may mention them. A second live gate object
		// would be exactly the synchronization-bug surface REQ-1520 forbids.
		return assertGateFullyRetired()
	})
	sc.Step(`^the mode answers whether an action may actuate$`, func() error {
		// No state to set — the Then drives the safety.MayActuate authority directly across the four modes.
		return nil
	})
	sc.Step(`^actuation is permitted only while the mode is Semi-auto or Full-auto and a breaker trip or halt forces the mode to Shadow$`, func() error {
		// (1) The sole authority: MayActuate is true ONLY for Semi-auto/Full-auto, and ONLY with a green preflight.
		for _, m := range []policy.Mode{policy.ModeShadow, policy.ModeHITL, policy.ModeSemiAuto, policy.ModeFullAuto} {
			want := m == policy.ModeSemiAuto || m == policy.ModeFullAuto
			if got := safety.MayActuate(m, true); got != want {
				return fmt.Errorf("MayActuate(%s, preflight-green) = %v, want %v", m, got, want)
			}
			if safety.MayActuate(m, false) {
				return fmt.Errorf("MayActuate(%s, preflight-RED) must be false (preflight-gated, fail closed)", m)
			}
		}
		// The zero/unknown mode is Shadow ⇒ fail-closed false (the property the disabled gate held).
		if safety.MayActuate(policy.Mode(0), true) || safety.MayActuate(nil, true) {
			return fmt.Errorf("the zero/nil mode must not actuate (fail-closed Shadow zero value)")
		}
		// (2) A breaker trip / halt forces the mode to Shadow via ForceShadow → MayActuate false (absorbed gate.Disable).
		ctl := policy.NewModeController(context.Background(), policy.NewMemModeStore(), audit.NewLedger(), modeAuthz{}, modePreflight{}, nil)
		if err := ctl.Transition(context.Background(), policy.ModeShadow, policy.ModeFullAuto, "operator:alice", "canary"); err != nil {
			return fmt.Errorf("escalate to Full-auto: %w", err)
		}
		cp := safety.NewChokepoint(ctl)
		if err := cp.ProvePreflight(okProver{}); err != nil {
			return fmt.Errorf("prove preflight: %w", err)
		}
		if !cp.MayActuate() {
			return fmt.Errorf("Full-auto + green preflight must actuate")
		}
		cp.ForceShadow("deviation breaker trip / POST /halt")
		if ctl.Current() != policy.ModeShadow || cp.MayActuate() {
			return fmt.Errorf("a breaker trip / halt must force the mode to Shadow (mode=%v mayActuate=%v)", ctl.Current(), cp.MayActuate())
		}
		return nil
	})

	// --- REQ-1521: absorbing the mutation gate is a deliberate, audited safety-core refactor (T-015-13) ---
	sc.Step(`^the mutation-gate obligations of INV-09 and INV-21$`, func() error { return nil })
	sc.Step(`^the gate is absorbed into the mode chokepoint$`, func() error { return nil })
	sc.Step(`^the constitution breaker halt preflight spec/013 and lockstep are re-expressed in mode terms and every fail-closed property the gate guaranteed is preserved$`, func() error {
		// (a) The absorption is DOCUMENTED as a deliberate, audited decision (never a silent deletion): the ADR
		//     and the re-expression across the constitution and spec/013 exist and name the mode chokepoint.
		for _, c := range []struct{ path, needle string }{
			{filepath.Join("..", "..", "..", "docs", "adr", "0013-mode-is-the-actuation-chokepoint.md"), "chokepoint"},
			{filepath.Join("..", "..", "..", "docs", "CONSTITUTION.md"), "sole actuation chokepoint"},
			{filepath.Join("..", "..", "..", "spec", "013-actuation-interceptor", "requirements.md"), "mode chokepoint"},
		} {
			b, err := os.ReadFile(c.path)
			if err != nil {
				return fmt.Errorf("re-expression missing: %s: %w", c.path, err)
			}
			if !strings.Contains(strings.ToLower(string(b)), c.needle) {
				return fmt.Errorf("%s does not re-express the gate in mode-chokepoint terms (missing %q)", c.path, c.needle)
			}
		}
		// (b) No live dual-state remains (the retired gate is fully absorbed).
		if err := assertGateFullyRetired(); err != nil {
			return err
		}
		// (c) Every fail-closed property the gate guaranteed is PRESERVED by the mode chokepoint:
		//     default-off (zero mode Shadow), preflight-gated enable, breaker/halt force-off.
		if safety.MayActuate(policy.Mode(0), true) {
			return fmt.Errorf("zero-value mode must be read-only (default-off preserved)")
		}
		cp := safety.NewChokepoint(safety.NewFixedModeAuthority(true)) // actuating mode, but preflight RED
		if cp.MayActuate() {
			return fmt.Errorf("an actuating mode with a red preflight must NOT actuate (preflight-gated preserved)")
		}
		if err := cp.ProvePreflight(okProver{}); err != nil {
			return err
		}
		if !cp.MayActuate() {
			return fmt.Errorf("actuating mode + green preflight must actuate")
		}
		cp.ForceShadow("halt")
		if cp.MayActuate() {
			return fmt.Errorf("ForceShadow must drop to read-only (breaker/halt force-off preserved)")
		}
		return nil
	})

	// --- REQ-1509: respect band_mode emits the more-restrictive of {verdict, risk band} (T-015-6) ---
	sc.Step(`^a rule with band_mode respect whose verdict is auto and an action whose risk band is POLL_PAUSE$`, func() error {
		w.bandPolicyVerdict = policy.VerdictAuto
		w.bandInput = safety.BandPollPause
		w.bandMode = policy.BandRespect
		return nil
	})

	// --- REQ-1510: force band_mode overrides the risk band and double-warns (T-015-6) ---
	sc.Step(`^a rule with band_mode force whose verdict is auto and an action whose risk band is AUTO_NOTICE$`, func() error {
		w.bandPolicyVerdict = policy.VerdictAuto
		w.bandInput = safety.BandAutoNotice
		w.bandMode = policy.BandForce
		return nil
	})

	sc.Step(`^the engine composes the band$`, func() error {
		// No constitutional never-auto floor applies to either scenario's action (a reversible, non-floor op).
		w.bandComposed, w.bandRecord = policy.ComposeBand(w.bandPolicyVerdict, w.bandInput, w.bandMode, false)
		return nil
	})

	sc.Step(`^the emitted decision is the more-restrictive POLL_PAUSE and not auto$`, func() error {
		// The POLL_PAUSE risk band vetoes the permissive `auto` policy verdict → the more-restrictive approve.
		if w.bandComposed == policy.VerdictAuto {
			return fmt.Errorf("composed verdict is auto — the POLL_PAUSE band did not restrict it")
		}
		if w.bandComposed != policy.VerdictApprove {
			return fmt.Errorf("composed verdict = %q, want the more-restrictive approve (POLL_PAUSE floor)", w.bandComposed)
		}
		if w.bandRecord.SafetyBand != safety.BandPollPause || w.bandRecord.BandMode != policy.BandRespect {
			return fmt.Errorf("record did not capture the POLL_PAUSE/respect composition: %+v", w.bandRecord)
		}
		return nil
	})

	sc.Step(`^the policy verdict is applied and a double-confirmation warning is recorded$`, func() error {
		if w.bandComposed != w.bandPolicyVerdict {
			return fmt.Errorf("force did not apply the policy verdict: composed %q, policy %q", w.bandComposed, w.bandPolicyVerdict)
		}
		if !w.bandRecord.DoubleWarn {
			return fmt.Errorf("force did not record the double-confirmation warning: %+v", w.bandRecord)
		}
		if w.bandRecord.BandMode != policy.BandForce {
			return fmt.Errorf("record band_mode = %q, want force", w.bandRecord.BandMode)
		}
		return nil
	})

	// --- REQ-1514: an op-class promotes to auto after N clean verified runs (T-015-8) ---
	sc.Step(`^an op-class in graduation state approve with N consecutive verified match runs and no deviation$`, func() error {
		const n = 4
		w.ladder = policy.NewLadder(n, policy.NewMemGraduationStore(), nil)
		w.gradClass = "service.restart"
		// Feed N verified-clean (match) runs, the boundary mapping of a spec/002 verifier `match` under
		// verify-on-auto (REQ-1515). The class must still read `approve` until the Nth run lands.
		for i := 0; i < n; i++ {
			if got := w.ladder.LevelOf(context.Background(), w.gradClass); i < n && i > 0 && got != policy.LevelApprove {
				return fmt.Errorf("class promoted early after %d clean runs, want approve until N=%d", i, n)
			}
			if _, err := w.ladder.Record(context.Background(), w.gradClass, policy.OutcomeFromVerdict(safety.VerdictMatch, true)); err != nil {
				return err
			}
		}
		return nil
	})
	sc.Step(`^the engine re-evaluates that op-class$`, func() error {
		// The graduation→verdict hook: an `auto` rule verdict is honored only for a graduated class.
		w.gradV = w.ladder.GraduatedVerdict(context.Background(), w.gradClass, policy.VerdictAuto)
		return nil
	})
	sc.Step(`^the op-class graduates to verdict "auto"$`, func() error {
		if w.ladder.LevelOf(context.Background(), w.gradClass) != policy.LevelAuto {
			return fmt.Errorf("class did not graduate to auto after N clean verified runs")
		}
		if w.gradV != policy.VerdictAuto {
			return fmt.Errorf("graduated hook resolved %q, want auto", w.gradV)
		}
		return nil
	})

	// --- REQ-1514: an op-class demotes to approve on the first deviation (T-015-8) ---
	sc.Step(`^an op-class graduated to auto$`, func() error {
		const n = 2
		w.ladder = policy.NewLadder(n, policy.NewMemGraduationStore(), nil)
		w.gradClass = "service.restart"
		for i := 0; i < n; i++ {
			if _, err := w.ladder.Record(context.Background(), w.gradClass, policy.OutcomeFromVerdict(safety.VerdictMatch, true)); err != nil {
				return err
			}
		}
		if w.ladder.LevelOf(context.Background(), w.gradClass) != policy.LevelAuto {
			return fmt.Errorf("precondition failed: class is not graduated to auto")
		}
		return nil
	})
	sc.Step(`^a verification returns a deviation verdict$`, func() error {
		// A spec/002 verifier `deviation`, mapped at the boundary (REQ-1514). It must demote the class.
		res, err := w.ladder.Record(context.Background(), w.gradClass, policy.OutcomeFromVerdict(safety.VerdictDeviation, true))
		if err != nil {
			return err
		}
		if !res.Demoted {
			return fmt.Errorf("deviation did not record a demotion: %+v", res)
		}
		return nil
	})
	sc.Step(`^the op-class is demoted to verdict "approve"$`, func() error {
		if w.ladder.LevelOf(context.Background(), w.gradClass) != policy.LevelApprove {
			return fmt.Errorf("class not demoted to approve after a deviation")
		}
		// The graduation→verdict hook now downgrades an `auto` rule verdict back to a human vote.
		if v := w.ladder.GraduatedVerdict(context.Background(), w.gradClass, policy.VerdictAuto); v != policy.VerdictApprove {
			return fmt.Errorf("demoted class still resolves auto → %q, want approve", v)
		}
		return nil
	})

	// --- REQ-1511: a floor-class action is still PROPOSED with rationale in every mode; the deny is an
	//     EXECUTION floor that bites only at auto-execute (T-015-7) ---
	sc.Step(`^a floor-class irreversible action in any mode$`, func() error {
		// The pipeline PROPOSES the action via a permissive operator rule (verdict auto) — the proposal is
		// produced identically regardless of mode; the floor gates only execution.
		r, err := policy.NewRule(policy.Rule{ID: "propose-reboot", Match: policy.Match{OpClass: "reboot"}, Verdict: policy.VerdictAuto})
		if err != nil {
			return err
		}
		w.engine, err = policy.NewEngine(context.Background(), policy.RuleSet{Rules: []policy.Rule{r}})
		if err != nil {
			return err
		}
		w.floor, err = policy.NewFloor(policy.FloorEntry{ID: "floor-reboot", Match: policy.Match{OpClass: "reboot"}})
		if err != nil {
			return err
		}
		w.in = policy.EvalInput{OpClass: "reboot", Reversible: false}
		return nil
	})
	sc.Step(`^the pipeline reasons about the action$`, func() error {
		// Reason → propose: the PROPOSAL is the pre-composition Rego base verdict (auto), produced identically in
		// every mode; the EXECUTION floor is then composed as a separate step. (Decide's own composed verdict is
		// additionally clamped by the constitutional never-auto floor for this irreversible floor-class op.)
		d, err := w.engine.Decide(context.Background(), w.in)
		if err != nil {
			return err
		}
		w.floorProposal = d
		w.floorExec, w.floorRec = w.floor.ApplyFloor(d.Audit().Base.Verdict, w.in)
		return nil
	})
	sc.Step(`^the action is suggested with its rationale and the deny takes effect only at auto-execute$`, func() error {
		// Suggested WITH rationale: the proposal (the Rego base verdict) was produced as `auto` and the decision
		// carries a reason — the pipeline did not suppress the floor-class action.
		if w.floorProposal.Audit().Base.Verdict != policy.VerdictAuto || strings.TrimSpace(w.floorProposal.Reason()) == "" {
			return fmt.Errorf("floor-class action was not proposed with rationale: %+v", w.floorProposal)
		}
		// The floor is an EXECUTION floor: the proposal is PRESERVED, only the execution verdict is denied.
		if w.floorRec.ProposalVerdict != policy.VerdictAuto {
			return fmt.Errorf("the floor mutated the proposal (not an execution floor): %+v", w.floorRec)
		}
		if w.floorExec != policy.VerdictDeny || !w.floorRec.Floored {
			return fmt.Errorf("the deny did not take effect at auto-execute: exec=%q rec=%+v", w.floorExec, w.floorRec)
		}
		return nil
	})

	// --- REQ-1512: the conservative template loads the predecessor deny-patterns + 30/min governor (T-015-7) ---
	sc.Step(`^the conservative policy template$`, func() error { return nil })
	sc.Step(`^the operator loads it as rule data$`, func() error {
		rs, err := policy.ConservativeTemplate()
		if err != nil {
			return err
		}
		w.tmplRuleSet = rs
		return nil
	})
	sc.Step(`^the loaded rules carry the predecessor argv deny-patterns and a 30-per-minute governor$`, func() error {
		if w.tmplRuleSet.Default.RateLimit == nil || *w.tmplRuleSet.Default.RateLimit != 30 {
			return fmt.Errorf("conservative governor = %v, want a 30-per-minute rate limit", w.tmplRuleSet.Default.RateLimit)
		}
		have := map[string]bool{}
		for _, r := range w.tmplRuleSet.Rules {
			if r.Verdict != policy.VerdictDeny {
				return fmt.Errorf("conservative rule %q is %q, want a deny (safe default)", r.ID, r.Verdict)
			}
			have[strings.ToLower(strings.TrimSpace(r.Match.ArgvPattern))] = true
		}
		for _, want := range []string{"rm -rf /", "mkfs", "reboot", "kubectl delete namespace"} {
			if !have[want] {
				return fmt.Errorf("conservative template is missing the predecessor deny-pattern %q", want)
			}
		}
		return nil
	})

	// --- REQ-1513: removing the conservative template warns but is not blocked; the constitutional floor
	//     still clamps floor-class ops beneath the removed policy template (T-015-7) ---
	sc.Step(`^the conservative template is loaded$`, func() error {
		f, err := policy.ConservativeFloor()
		if err != nil {
			return err
		}
		w.floor = f
		return nil
	})
	sc.Step(`^the operator removes it behind a double-confirmation$`, func() error {
		// Warn-don't-block: removal is PERMITTED behind a distinct red double-confirmation, and it is audited.
		ack := policy.Warning{Text: "I accept lowering the conservative deny floor", Acknowledged: true, DoubleConfirm: true}
		w.floorChange, w.floorChangeErr = w.floor.RemoveFloorEntry("deny-reboot", "operator:alice", ack)
		return nil
	})
	sc.Step(`^the removal is permitted and the constitutional floor still clamps floor-class ops$`, func() error {
		// Permitted (not blocked) and audited.
		if w.floorChangeErr != nil {
			return fmt.Errorf("removal behind a double-confirmation was blocked: %v", w.floorChangeErr)
		}
		if w.floorChange.Change != "remove-floor-entry" || !w.floorChange.RedConfirm || !w.floorChange.Lowering ||
			strings.TrimSpace(w.floorChange.WarningText) == "" || w.floorChange.Timestamp.IsZero() {
			return fmt.Errorf("removal did not emit a complete audited warning record: %+v", w.floorChange)
		}
		// The removed POLICY floor entry no longer floors that op via the policy floor ...
		if w.floor.AppliesTo(policy.EvalInput{OpClass: "reboot"}) {
			return fmt.Errorf("the removed policy floor entry still floors — removal did not take effect")
		}
		// ... BUT the constitutional mechanical never-auto floor (core/safety, INV-09) STILL clamps floor-class
		// ops beneath the removed policy template — the policy floor exposes no bypass of it.
		reboot := policy.EvalInput{OpClass: "reboot", Reversible: true}
		if !safety.IsNeverAuto("reboot") || !policy.NeverAutoApplies(reboot) {
			return fmt.Errorf("the constitutional never-auto floor was lifted by removing the policy template — forbidden")
		}
		if v, rec := policy.ComposeBand(policy.VerdictAuto, safety.BandAuto, policy.BandForce, policy.NeverAutoApplies(reboot)); v == policy.VerdictAuto || !rec.FloorClamped {
			return fmt.Errorf("the constitutional floor did not clamp beneath the removed policy template: v=%q rec=%+v", v, rec)
		}
		return nil
	})

	// --- REQ-1507: a below-min-confidence action is clamped to approve (T-015-3), driven THROUGH the composed
	//     Engine.Decide: the engine holds a fixed-clock rate governor and resolves min_confidence (0.60) from the
	//     global-default rule via EffectiveParams; the composed step-3 confidence clamp tightens auto→approve. ---
	sc.Step(`^an auto-verdict rule and an action whose confidence is below the resolved min_confidence$`, func() error {
		r, err := policy.NewRule(policy.Rule{ID: "auto-lowconf", Match: policy.Match{Selector: hostSel("h1")}, Verdict: policy.VerdictAuto})
		if err != nil {
			return err
		}
		// The global-default sets min_confidence 0.60; the leaf leaves it unset and inherits it (REQ-1507).
		rs := policy.RuleSet{Default: policy.Params{MinConfidence: f64(0.60)}, Rules: []policy.Rule{r}}
		e, err := policy.NewEngine(context.Background(), rs)
		if err != nil {
			return err
		}
		fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) // fixed clock — no dependence on wall time.
		w.engine = e.WithRateGovernor(policy.NewRateGovernor(func() time.Time { return fixed }))
		// A reversible, non-floor, auto-banded action whose bound confidence (0.30) is below the resolved 0.60,
		// so the composed Decide clamps auto→approve at the confidence stage (and nowhere loosens it).
		w.in = policy.EvalInput{Host: "h1", Confidence: 0.30, Reversible: true, Band: safety.BandAuto}
		return nil
	})

	// --- REQ-1508: an over-rate-limit action is clamped to approve (T-015-3) ---
	sc.Step(`^a rule with a rate_limit of N per minute that has already auto-executed N times this minute$`, func() error {
		const n = 3
		fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) // injected clock seam — deterministic window.
		w.refineGov = policy.NewRateGovernor(func() time.Time { return fixed })
		w.refineParams = policy.Params{RateLimit: intPtr(n)}
		w.gradClass = "svc.restart" // the rate-governor key (reused world field).
		// Drive N auto-executions this minute; each must be admitted as auto (under the cap).
		for k := 0; k < n; k++ {
			v, _ := w.refineGov.Refine(policy.VerdictAuto, 1.0, w.refineParams, w.gradClass)
			if v != policy.VerdictAuto {
				return fmt.Errorf("run %d was clamped before the cap: %q", k, v)
			}
		}
		return nil
	})
	sc.Step(`^the engine evaluates the next matching action$`, func() error {
		v, rec := w.refineGov.Refine(policy.VerdictAuto, 1.0, w.refineParams, w.gradClass)
		w.verdict = v
		w.refineRec = rec
		return nil
	})

	// --- REQ-1516: a vote is admitted ONLY WHEN the voter is a member of the pending decision's approve_by
	//     set; a non-member's vote is rejected (T-015-9). The approve_by principals resolve through the shared
	//     local-and-human-plane PrincipalResolver — here the TG-local operator/group registry. ---
	sc.Step(`^a pending decision whose approve_by set includes the voting principal$`, func() error {
		// alice is a member of the sre-oncall group named in the decision's approve_by.
		w.voteResolver = policy.NewLocalResolver(nil, policy.LocalPrincipal{ID: "alice", Groups: []string{"sre-oncall"}})
		w.voteActor = "alice"
		w.voteApproveBy = []string{"group:sre-oncall"}
		return nil
	})
	sc.Step(`^a pending decision whose approve_by set excludes the voting principal$`, func() error {
		// mallory is named in no approve_by user entry and belongs to no approve_by group.
		w.voteResolver = policy.NewLocalResolver(nil, policy.LocalPrincipal{ID: "mallory", Groups: []string{"interns"}})
		w.voteActor = "mallory"
		w.voteApproveBy = []string{"group:sre-oncall", "user:alice"}
		return nil
	})
	sc.Step(`^the principal votes on /v1/vote$`, func() error {
		w.voteAllowed, w.voteDec = policy.MayApprove(context.Background(), w.voteResolver, w.voteActor, w.voteApproveBy)
		return nil
	})
	sc.Step(`^the vote is admitted$`, func() error {
		if !w.voteAllowed || !w.voteDec.Allowed {
			return fmt.Errorf("an approve_by member's vote was not admitted: %+v", w.voteDec)
		}
		if strings.TrimSpace(w.voteDec.MatchedEntry) == "" {
			return fmt.Errorf("an admitted vote must name the matching approve_by entry: %+v", w.voteDec)
		}
		return nil
	})
	sc.Step(`^the vote is rejected$`, func() error {
		if w.voteAllowed || w.voteDec.Allowed {
			return fmt.Errorf("a non-member's vote was admitted (must fail closed): %+v", w.voteDec)
		}
		return nil
	})

	// Shared Then for both T-015-3 scenarios: the verdict was clamped auto→approve.
	sc.Step(`^the verdict is clamped to "approve"$`, func() error {
		if w.verdict != policy.VerdictApprove {
			return fmt.Errorf("verdict = %q, want the clamped approve", w.verdict)
		}
		if !w.refineRec.ConfidenceClamped && !w.refineRec.RateClamped {
			return fmt.Errorf("no clamp was recorded in the refine record: %+v", w.refineRec)
		}
		return nil
	})

	// --- REQ-1517: a single allow-all policy is permitted behind a red double-confirmation (T-015-11) ---
	sc.Step(`^an operator applying an allow-all policy$`, func() error {
		fl, err := policy.ConservativeFloor()
		if err != nil {
			return err
		}
		w.allowAllFloor = fl
		// The operator's DISTINCT red double-confirmation — the loud, acknowledged consent to lower the dial.
		w.allowAllAck = policy.Warning{Text: "I own the paranoia dial — apply allow-all", Acknowledged: true, DoubleConfirm: true}
		return nil
	})
	sc.Step(`^the engine receives the change behind a red double-confirmation$`, func() error {
		// SelectTemplate(bare) applies the allow-all posture; WarnFor surfaces the (non-blocking) warning about it.
		w.allowAllRS, _, w.allowAllErr = w.allowAllFloor.SelectTemplate(policy.TemplateBare, "operator:alice", w.allowAllAck)
		w.allowAllWarns = policy.WarnFor(w.allowAllRS, w.allowAllFloor, policy.ModeShadow)
		return nil
	})
	sc.Step(`^the engine warns and applies the policy and never refuses it$`, func() error {
		if w.allowAllErr != nil {
			return fmt.Errorf("the engine REFUSED the operator's allow-all policy (%v) — warn-don't-block forbids a block (REQ-1517)", w.allowAllErr)
		}
		if len(w.allowAllRS.Rules) == 0 {
			return fmt.Errorf("the allow-all policy was not applied (empty rule set)")
		}
		warned := false
		for _, wn := range w.allowAllWarns {
			if wn.Code == policy.WarnAllowAllRule {
				warned = true
			}
		}
		if !warned {
			return fmt.Errorf("the engine did not WARN about the allow-all policy: %+v", w.allowAllWarns)
		}
		return nil
	})

	// --- REQ-1518: every policy decision is appended to the governance ledger (T-015-11) ---
	sc.Step(`^the engine evaluates an action to any verdict$`, func() error {
		r, err := policy.NewRule(policy.Rule{ID: "audit-rule", Match: policy.Match{Selector: hostSel("h1")}, Verdict: policy.VerdictAuto})
		if err != nil {
			return err
		}
		eng, err := policy.NewEngine(context.Background(), policy.RuleSet{Rules: []policy.Rule{r}})
		if err != nil {
			return err
		}
		// Wrap the engine so every Decide emits ONE record to the tamper-evident governance ledger via the adapter.
		w.auditLedger = audit.NewLedger()
		w.auditedEng = policy.NewAuditedEngine(eng, policy.NewLedgerAuditSink(w.auditLedger))
		return nil
	})
	sc.Step(`^the decision is produced$`, func() error {
		var err error
		w.auditDec, err = w.auditedEng.Decide(context.Background(),
			policy.EvalInput{Host: "h1", OpClass: "service-restart", Reversible: true, Confidence: 0.95, Band: safety.BandAuto, Mode: policy.ModeFullAuto})
		return err
	})
	sc.Step(`^one policy_decision record with the matched rule verdict band action_id and actor is appended to the tamper-evident ledger$`, func() error {
		if w.auditLedger.Len() != 1 {
			return fmt.Errorf("ledger has %d rows, want exactly one policy_decision record", w.auditLedger.Len())
		}
		entry := w.auditLedger.Entries()[0]
		if entry.Decision != "policy:"+string(w.auditDec.Verdict()) {
			return fmt.Errorf("ledger decision = %q, want policy:%s", entry.Decision, w.auditDec.Verdict())
		}
		if entry.ActionID == "" {
			return fmt.Errorf("the policy_decision record has no bound action_id")
		}
		// The record is hash-chained into the tamper-evident ledger and re-walks clean.
		return w.auditLedger.Verify()
	})

	// --- REQ-1518: the policy_decision table is append-only with no runtime UPDATE/DELETE (T-015-12) ---
	// CI has no Postgres, so the grant is asserted structurally over the migration DDL (the same compose-only
	// integration model the migration guards use in core/db/migrations_test.go): the runtime DML role is
	// stripped of UPDATE+DELETE on policy_decision, so an attempted rewrite is denied by the grants.
	sc.Step(`^the policy_decision persistence migration$`, func() error {
		path := filepath.Join("..", "..", "..", "core", "db", "migrations", "0019_policy_engine.up.sql")
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read policy_decision migration: %w", err)
		}
		w.migrationDDL = string(b)
		if !strings.Contains(w.migrationDDL, "CREATE TABLE policy_decision") {
			return fmt.Errorf("migration does not create policy_decision")
		}
		return nil
	})
	sc.Step(`^the runtime database role attempts to update or delete a policy_decision row$`, func() error {
		// The attempt is governed structurally by the grants; nothing to execute (CI has no DB). The Then
		// asserts the REVOKE that denies it.
		if w.migrationDDL == "" {
			return fmt.Errorf("no migration DDL loaded")
		}
		return nil
	})
	sc.Step(`^the operation is denied by the grants$`, func() error {
		flat := strings.Join(strings.Fields(w.migrationDDL), " ")
		if !strings.Contains(flat, "REVOKE UPDATE, DELETE ON policy_decision FROM tg_runtime") {
			return fmt.Errorf("policy_decision is NOT append-only: the migration must REVOKE UPDATE,DELETE from tg_runtime")
		}
		return nil
	})

	// --- REQ-1501: the investigation pipeline is IDENTICAL across all four modes; the mode governs ONLY the
	//     actuation branch (T-015-5). The pipeline's policy-evaluate stage is Engine.Decide; the guard proves
	//     Decide resolves the SAME verdict/composed-band/approve_by regardless of the active mode. ---
	sc.Step(`^the same alert evaluated in each of the four modes$`, func() error {
		// A fresh REAL composed engine per mode (verdict trinary as data rules), so each mode is evaluated from
		// identical per-action state — the ONLY variable is in.Mode.
		w.pipelineFactory = func(ctx context.Context) (policy.ModeDecider, error) {
			auto, err := policy.NewRule(policy.Rule{ID: "auto", Match: policy.Match{Selector: hostSel("h1")}, Verdict: policy.VerdictAuto})
			if err != nil {
				return nil, err
			}
			eng, err := policy.NewEngine(ctx, policy.RuleSet{Rules: []policy.Rule{auto}})
			if err != nil {
				return nil, err
			}
			return eng, nil
		}
		// A reversible, high-confidence, auto-banded action so the base `auto` carries through the composition.
		w.pipelineIn = policy.EvalInput{Host: "h1", Reversible: true, Confidence: 1.0, Band: safety.BandAuto}
		return nil
	})
	sc.Step(`^the pipeline runs ingest reason rationale propose and risk-classify$`, func() error {
		w.pipelineErr = policy.AssertModeInvariant(context.Background(), w.pipelineFactory, w.pipelineIn)
		return nil
	})
	sc.Step(`^those stages take the same code path in every mode and only the actuation branch differs$`, func() error {
		// The guard passes: the decision is identical across all four modes (only the recorded Mode differs). And
		// the guard is NOT a no-op — a deliberately mode-dependent decider is DETECTED with the typed error.
		if w.pipelineErr != nil {
			return fmt.Errorf("the pipeline is mode-dependent (REQ-1501 violated): %v", w.pipelineErr)
		}
		modeDependent := func(context.Context) (policy.ModeDecider, error) { return modeDependentDecider{}, nil }
		if got := policy.AssertModeInvariant(context.Background(), modeDependent, w.pipelineIn); got == nil {
			return fmt.Errorf("the guard is a no-op: it failed to detect a deliberately mode-dependent pipeline")
		} else if !errors.Is(got, policy.ErrPipelineModeDependent) {
			return fmt.Errorf("a detected divergence must be ErrPipelineModeDependent, got: %v", got)
		}
		return nil
	})

	// --- REQ-1519: forcing the engine on in Shadow warns; disabling it in Semi-auto double-warns (T-015-11) ---
	sc.Step(`^an administrator overriding the per-mode engine default$`, func() error {
		w.toggleLedger = audit.NewLedger()
		w.toggle = policy.NewEngineToggle(modeAuthz{}, w.toggleLedger)
		return nil
	})
	sc.Step(`^the engine is forced on in Shadow and later disabled in Semi-auto$`, func() error {
		var err error
		// Forcing ON in Shadow (a read-only mode) needs a single acknowledgement and records a warning.
		w.forceOnRec, err = w.toggle.Override(context.Background(), "admin:root", true, policy.ModeShadow,
			policy.Warning{Text: "run the engine in shadow", Acknowledged: true})
		if err != nil {
			return err
		}
		// Disabling in Semi-auto (an actuating mode) needs the DISTINCT red double-confirmation.
		w.disableRec, err = w.toggle.Override(context.Background(), "admin:root", false, policy.ModeSemiAuto,
			policy.Warning{Text: "disable the engine in semi-auto", Acknowledged: true, DoubleConfirm: true})
		return err
	})
	sc.Step(`^the force-on records a warning and the disable records a double-confirmation warning$`, func() error {
		if !w.forceOnRec.ForcedOn || w.forceOnRec.Warning.Code != policy.WarnEngineForcedOn {
			return fmt.Errorf("force-on did not record a warning: %+v", w.forceOnRec)
		}
		if !w.disableRec.Disabled || !w.disableRec.DoubleConfirm || w.disableRec.Warning.Code != policy.WarnEngineDisabled {
			return fmt.Errorf("disable did not record a double-confirmation warning: %+v", w.disableRec)
		}
		// Both overrides were appended to the tamper-evident governance ledger, which re-walks clean.
		if w.toggleLedger.Len() != 2 {
			return fmt.Errorf("toggle ledger has %d records, want 2 (force-on + disable)", w.toggleLedger.Len())
		}
		return w.toggleLedger.Verify()
	})

	// --- REQ-1522: every decision records the bundle version + the full matched-rule list (decision-surface
	//     hardening). The fail-to-deny-on-evaluator-error leg is locked white-box in core/policy/hardening_test.go
	//     (there is no public path from input to an evaluator error); this oracle drives the two auditable
	//     hardenings through the real public Engine.Decide. ---
	sc.Step(`^a rule bundle where an auto rule and a deny rule both match one host and a second auto rule matches another host$`, func() error {
		autoH1, err := policy.NewRule(policy.Rule{ID: "auto-h1", Match: policy.Match{Selector: hostSel("h1")}, Verdict: policy.VerdictAuto})
		if err != nil {
			return err
		}
		denyH1, err := policy.NewRule(policy.Rule{ID: "deny-h1", Match: policy.Match{Selector: hostSel("h1")}, Verdict: policy.VerdictDeny})
		if err != nil {
			return err
		}
		autoH2, err := policy.NewRule(policy.Rule{ID: "auto-h2", Match: policy.Match{Selector: hostSel("h2")}, Verdict: policy.VerdictAuto})
		if err != nil {
			return err
		}
		w.hardenEng, err = policy.NewEngine(context.Background(), policy.RuleSet{Rules: []policy.Rule{autoH1, denyH1, autoH2}})
		return err
	})
	sc.Step(`^the engine resolves an allow decision on the other host and a deny decision on the shared host$`, func() error {
		var err error
		// h2 → the lone auto rule survives to auto (an ALLOW decision).
		w.hardenAllow, err = w.hardenEng.Decide(context.Background(),
			policy.EvalInput{Host: "h2", Reversible: true, Confidence: 1.0, Band: safety.BandAuto})
		if err != nil {
			return err
		}
		// h1 → the auto and deny rules both match; deny-overrides resolves to deny.
		w.hardenDeny, err = w.hardenEng.Decide(context.Background(), policy.EvalInput{Host: "h1"})
		return err
	})
	sc.Step(`^each decision records the loaded bundle version and the deny decision surfaces the full matched-rule list$`, func() error {
		want := w.hardenEng.BundleVersion()
		if want == "" {
			return fmt.Errorf("engine reports an empty bundle version")
		}
		// The outcomes are UNCHANGED by the hardening: h2 → auto (allow), h1 → deny (deny-overrides).
		if w.hardenAllow.Verdict() != policy.VerdictAuto {
			return fmt.Errorf("allow decision verdict = %q, want auto", w.hardenAllow.Verdict())
		}
		if w.hardenDeny.Verdict() != policy.VerdictDeny {
			return fmt.Errorf("deny decision verdict = %q, want deny", w.hardenDeny.Verdict())
		}
		// The bundle version rides BOTH the allow and the deny decision (REQ-1522).
		if w.hardenAllow.BundleVersion() != want || w.hardenDeny.BundleVersion() != want {
			return fmt.Errorf("bundle version missing/mismatched: allow=%q deny=%q want=%q",
				w.hardenAllow.BundleVersion(), w.hardenDeny.BundleVersion(), want)
		}
		// The deny decision surfaces the FULL matched-rule list (both matching rules, each with its verdict).
		got := w.hardenDeny.MatchedRules()
		if len(got) != 2 {
			return fmt.Errorf("matched-rule list has %d entries, want the full 2 (auto-h1 + deny-h1): %+v", len(got), got)
		}
		byID := map[string]policy.Verdict{}
		for _, m := range got {
			byID[m.ID] = m.Verdict
		}
		if byID["auto-h1"] != policy.VerdictAuto || byID["deny-h1"] != policy.VerdictDeny {
			return fmt.Errorf("matched-rule list is not the full typed set {auto-h1:auto, deny-h1:deny}: %+v", got)
		}
		return nil
	})
}

func intPtr(v int) *int { return &v }

// modeDependentDecider is a deliberately BROKEN decider whose verdict depends on the active mode. It is the
// negative control for the REQ-1501 guard scenario: the guard MUST detect it (proving it is not a no-op).
type modeDependentDecider struct{}

func (modeDependentDecider) Decide(_ context.Context, in policy.EvalInput) (policy.PolicyDecision, error) {
	v := policy.VerdictApprove
	if in.Mode.MayAutoActuate() { // BUG: the pipeline verdict is a function of the mode.
		v = policy.VerdictAuto
	}
	return policy.NewPolicyDecision(v, "buggy", in.Band, nil, in.Mode, "mode-dependent stub", policy.DecisionAudit{}), nil
}

func ruleIDs(rules []policy.Rule) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = r.ID
	}
	return out
}

// okProver is a PreflightProver whose SelfTest always passes — a wired interception chain — for the
// mode-chokepoint scenarios (T-015-13). It discharges Chokepoint.ProvePreflight's proof obligation.
type okProver struct{}

func (okProver) SelfTest() error { return nil }

// assertGateFullyRetired is the grep-style negative control for REQ-1520/1521: it walks the runtime Go source
// and fails if any LIVE definition of the retired mutation gate remains — proving the MutationGate object and
// the TG_MUTATION_ENABLED env knob are fully ABSORBED into the mode chokepoint (no second live state to sync).
// It searches code-declaration tokens that cannot appear in prose (a type/func declaration) plus the env knob
// as a Go string literal assembled by concatenation, so this scanner's own source is never a false match.
func assertGateFullyRetired() error {
	root := filepath.Join("..", "..", "..")
	banned := []string{"type MutationGate struct", "func NewMutationGate", "\"TG_" + "MUTATION_ENABLED\""}
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", "node_modules", "frontend", "vendor":
				// Skip VCS/build/vendor dirs and any nested git worktrees under .claude — they hold sibling
				// checkouts (possibly pre-refactor) that are not this tree's live runtime source.
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip this scanner's own package so its needle literals are not self-matched.
		if strings.Contains(path, filepath.Join("spec", "015-policy-engine", "acceptance")) {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		s := string(b)
		for _, tok := range banned {
			if strings.Contains(s, tok) {
				offenders = append(offenders, path+" :: "+tok)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(offenders) > 0 {
		return fmt.Errorf("the retired mutation gate must be fully absorbed (no live dual-state), found: %v", offenders)
	}
	return nil
}
