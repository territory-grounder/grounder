package acceptance

// Step bindings for the two spec/028 scenarios whose claims are CROSS-PACKAGE (T-028-10).
//
//	REQ-2805  "An absent class seals to nothing executable" — rung 0 of the earned ladder is registry
//	          ABSENCE (ADR-0016). The oracle proves it the only way that means anything: it loads a REAL
//	          overlay (so the composed registry is genuinely populated and genuinely consulted), then
//	          force-routes an action for a slug in NEITHER surface through the REAL gate seal, the REAL
//	          interceptor with mutation ON and a permissive policy, and the REAL effect leaf. Every layer
//	          that could refuse for some other reason is deliberately satisfied, so the refusal that lands
//	          can only be the empty-argv contract.
//
//	REQ-2810  "A forecast-lane verdict never feeds graduation" — the C4 category error. core/falsify and
//	          core/policy do not import each other, and that is exactly why the claim needs an oracle
//	          holding both at once: from inside either package, "these are unconnected" is invisible. The
//	          oracle authors a real forecast DEVIATION — the one outcome that would demote a class — over a
//	          never-executed prediction, with a real Ladder seeded at auto_notice mid-second-climb, and
//	          asserts the ladder did not move and its store was never written.
//
// RED MUTATION CONTROLS EXECUTED (2026-07-31; each mutation applied, `go test ./...` run in this package,
// the failure recorded VERBATIM, the mutation reverted, the suite restored green):
//
//	M1  core/actuate/opschema/opschema.go — Lookup() falls back to an embedded spec on a miss instead of
//	    returning ok=false.
//	    → RED: `"rotate-flux-capacitor" must be absent from EVERY registry surface`
//
//	M2  temporal/runner/activities.go — sealEffect() returns []string{"/bin/true"} instead of (nil, nil)
//	    when opschema.Lookup misses.
//	    → RED: `refused, but NOT by the empty-argv leaf (got "execute failed: actuation: mutating actuation
//	      is disabled (Phase 0/1 is read-only)") — the refusal must come from the argv contract, not an
//	      earlier gate`
//	    NOTE: the first version of this control mutated sealedArgv() and came back GREEN — sealEffect()
//	    returns (nil, nil) on a Lookup miss BEFORE sealedArgv() is ever reached, so the layer above masked
//	    it. Re-aimed rather than banked; a control that cannot reach the code it claims to test is not a
//	    control.
//
//	M3  core/falsify/scorer.go — a Graduation seam added to Scorer and called with
//	    policy.OutcomeFromVerdict(v, true) for each forecast verdict (the C4 wiring this requirement
//	    forbids), PLUS the harness wiring that hands it this oracle's ladder.
//	    → RED: `the forecast lane moved the ladder: level auto_notice -> approve, clean streak 5 -> 0,
//	      notice streak 3 -> 0`
//	    THE TWO-PART SHAPE IS A REAL LIMIT AND IS STATED RATHER THAN HIDDEN: the forecast lane cannot reach
//	    THIS oracle's ladder unless something hands it that ladder, so a production-only mutation is inert
//	    here. What M3 proves is that the immobility assertions are not VACUOUS. What protects production is
//	    a different fact: core/falsify does not import core/policy at all, and this mutation has to add that
//	    import to exist.
//
//	M4  core/falsify/scorer.go — the `d.Executed` guard dropped, so executed predictions also receive a
//	    forecast verdict.
//	    → RED: `the forecast lane must have authored exactly one DEVIATION for this oracle to mean anything,
//	      got {Scored:2 ... Executed:1 ... Deviations:2 ...}` — the non-vacuity guard fires first, and the
//	      executed-prediction assertion behind it is what the guard is protecting.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/falsify"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/verify"
	runner "github.com/territory-grounder/grounder/temporal/runner"
)

func init() { stepRegistrars = append(stepRegistrars, registerSteps) }

// absentClass is a slug in NO registry surface: not embedded (the binary has never heard of it) and not
// ratified into the overlay this oracle loads. Its shape is deliberately plausible — an operator reading it
// would believe it exists — because a slug that looks obviously fake proves nothing about the fail-closed
// path a real typo would take.
const absentClass = "rotate-flux-capacitor"

// grantedClass is what the oracle DOES ratify into the overlay. It exists so the composed registry is
// genuinely non-empty when the absent slug is looked up: an empty overlay would let "absent" be satisfied by
// "the overlay was never consulted", which is a different (and much weaker) fact.
const grantedClass = "reload-haproxy"

// ---------------------------------------------------------------------------
// doubles — the minimal collaborators these two scenarios need. Everything that
// could refuse for a reason OTHER than the one under test answers yes.
// ---------------------------------------------------------------------------

// allowDecider is the permissive policy decider: the chain oracle must reach the EFFECT LEAF, so policy
// answers yes and the refusal asserted below can only come from the leaf itself.
type allowDecider struct{}

func (allowDecider) Decide(_ context.Context, in policy.EvalInput) (policy.PolicyDecision, error) {
	return policy.NewPolicyDecision(policy.VerdictAuto, "acceptance-permissive", in.Band, nil, in.Mode,
		"spec/028 chain oracle: policy must not be the refusing layer", policy.DecisionAudit{}), nil
}

type stopCompleter struct{}

func (stopCompleter) Complete(_ context.Context, _, _ string, _ []model.Message) (string, error) {
	return `{"action":"stop","confidence":0.9}`, nil
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

func (m *memManifests) Get(_ context.Context, actionID string) (*manifest.ActionManifest, bool, error) {
	mf, ok := m.byID[actionID]
	return mf, ok, nil
}

// countingGradStore wraps the ladder's store so the oracle can assert the stronger claim: not merely that
// the level is unchanged, but that the forecast lane never caused a WRITE. A lane that recorded a no-op
// outcome would leave the level identical while still having reached into the ladder, and "no transition
// occurred" would then be true by accident rather than by construction.
type countingGradStore struct {
	inner  policy.GraduationStore
	writes int
}

func (s *countingGradStore) Load(ctx context.Context, opClass string) (policy.ClassState, error) {
	return s.inner.Load(ctx, opClass)
}

func (s *countingGradStore) Save(ctx context.Context, st policy.ClassState) error {
	s.writes++
	return s.inner.Save(ctx, st)
}

// ---------------------------------------------------------------------------
// scenario state
// ---------------------------------------------------------------------------

type specState struct {
	// REQ-2805
	ledger     *audit.Ledger
	ledgerLen  int
	manifests  *memManifests
	sealedID   string
	execResult runner.ExecuteResult
	execErr    error

	// REQ-2810
	store    *falsify.MemStore
	ladder   *policy.Ladder
	gradSt   *countingGradStore
	before   policy.ClassState
	scoreRes falsify.Result
	scoreErr error
}

const fixedNowRFC = "2026-07-18T12:00:00Z"

func fixedNow() time.Time {
	t, _ := time.Parse(time.RFC3339, fixedNowRFC)
	return t
}

// ratifyGrantedClass loads a REAL overlay through the REAL admission path (SetOverlay verifies the entry
// hash, then runs ValidateSpec) so the composed registry the absent-class lookup consults is one an operator
// actually granted — not a stub the oracle waved into existence.
func ratifyGrantedClass() error {
	spec := opschema.OpClassSpec{
		OpClass:          grantedClass,
		Op:               "reload",
		Family:           opschema.FamilyServiceLifecycle,
		SafetyTier:       opschema.TierLowReversible,
		Params:           []opschema.ParamSpec{{Name: "unit", Type: "string", Required: true}},
		ArgvTemplate:     []string{"systemctl", "reload", "${unit}"},
		RollbackTemplate: []string{"systemctl", "status", "${unit}"},
	}
	hash, err := opschema.CanonicalHash(spec)
	if err != nil {
		return fmt.Errorf("canonicalize the granted class: %w", err)
	}
	accepted, rejected := opschema.SetOverlay([]opschema.OverlayEntry{{Spec: spec, Hash: hash}})
	if accepted != 1 || len(rejected) != 0 {
		return fmt.Errorf("the granted class must be admitted into the overlay: accepted=%d rejected=%v", accepted, rejected)
	}
	return nil
}

func registerSteps(sc *godog.ScenarioContext) {
	st := &specState{}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		*st = specState{}
		opschema.ClearOverlay()
		return ctx, nil
	})
	// The overlay is PROCESS-GLOBAL. Leaving a scenario's grant loaded would make the next scenario's
	// registry depend on execution order, which is the failure mode a shared mutable registry always has.
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		opschema.ClearOverlay()
		return ctx, nil
	})

	// ---- REQ-2805: an absent class seals to nothing executable ---------------

	sc.Step(`^an op-class present in no registry surface$`, func() error {
		if err := ratifyGrantedClass(); err != nil {
			return err
		}
		// The composed registry is now genuinely populated, and the granted class resolves through it —
		// so a miss on the absent slug is a real miss, not an unconsulted overlay.
		if _, ok := opschema.Lookup(grantedClass); !ok {
			return fmt.Errorf("the ratified class %q must resolve through the composed registry", grantedClass)
		}
		if _, ok := opschema.Lookup(absentClass); ok {
			return fmt.Errorf("%q must be absent from EVERY registry surface", absentClass)
		}
		if opschema.IsEmbedded(absentClass) {
			return fmt.Errorf("%q must not be in the embedded registry", absentClass)
		}
		for _, s := range opschema.Specs() {
			if s.OpClass == absentClass {
				return fmt.Errorf("%q appears in the composed spec list — it is not absent", absentClass)
			}
		}
		// Rung 0 stated as the argv contract itself: absence is not "no permission", it is no vector.
		if argv, err := opschema.Argv(absentClass, map[string]string{"unit": "haproxy"}); err == nil {
			return fmt.Errorf("an absent class resolved to an argv — opschema.Argv returned %v for a slug in no registry surface", argv)
		}
		return nil
	})

	sc.Step(`^an action for it is force-routed toward execution$`, func() error {
		st.ledger = audit.NewLedger()
		st.manifests = &memManifests{}
		// REAL interceptor over the REAL local effect leaf; mutation ON via a fixed authority and a
		// permissive policy — so the ONLY thing left to refuse is the leaf's argv contract.
		cp := safety.NewChokepoint(safety.NewFixedModeAuthority(true))
		interceptor := actuate.NewInterceptor(
			cp,
			actuation.LocalReadOnly{Cap: "spec028.chain"},
			st.ledger,
		).WithPolicyDecider(allowDecider{}, func() policy.Mode { return policy.ModeFullAuto })
		// Prove the boot preflight over the wired chain, so mode + preflight both permit actuation — the
		// same green a production worker requires. Only the argv contract is left to refuse.
		if err := cp.ProvePreflight(interceptor); err != nil {
			return fmt.Errorf("preflight must prove over a wired chain: %v", err)
		}

		deps := chainDeps()
		deps.Interceptor = interceptor
		deps.ManifestSink = st.manifests
		deps.Manifests = st.manifests
		deps.Mutation = cp
		// The verdict-adjudication baseline gates answer honestly-empty with ok=true, so every named
		// pre-leaf gate is deliberately green.
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
				ExternalRef: "TG-028-acc-1",
				Action: manifest.Action{Target: "svc01", OpClass: absentClass, Op: "rotate",
					Reversible: true, Params: map[string]string{"unit": "haproxy"}},
				Rationale:   "force-routed action for an absent class (REQ-2805 oracle)",
				EvidenceIDs: []string{"tr-1"},
			},
			Band: safety.BandAuto, PlanHash: "acc-028-chain", Site: "dc1",
		})
		if err != nil {
			return fmt.Errorf("gate seal failed before the chain could be exercised: %v", err)
		}
		st.sealedID = gateOut.ActionID
		st.ledgerLen = st.ledger.Len()
		st.execResult, st.execErr = acts.ExecuteActivity(context.Background(), runner.ExecuteInput{
			ActionID: gateOut.ActionID, ExternalRef: "TG-028-acc-1", PlanHash: "acc-028-chain",
			Site: "dc1", TargetHost: "svc01", Approved: true, Band: safety.BandAuto,
			EvidenceIDs: []string{"tr-1"},
			ToolResults: []agent.ToolResult{{ID: "tr-1", Tool: "get-logs", Target: "svc01",
				Output: "svc01 flux capacitor drifting", Success: true}},
		})
		return nil
	})

	sc.Step(`^sealing yields no argv and the effect leaf refuses on empty argv$`, func() error {
		// SEALING YIELDS NO ARGV. The manifest was sealed (the action has an identity and a ledgered
		// decision) and carries no executable vector — absence at rung 0 is not a permission answer, it is
		// the absence of anything to permit.
		if st.sealedID == "" {
			return fmt.Errorf("no manifest was sealed — the chain was never entered")
		}
		if _, ok, err := st.manifests.Get(context.Background(), st.sealedID); err != nil || !ok {
			return fmt.Errorf("the sealed manifest must be retrievable: ok=%v err=%v", ok, err)
		}
		// THE LEAF REFUSES. Not "an earlier gate refused" — the refusal signature must name the argv
		// contract, because every other layer was deliberately made to say yes.
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

	// ---- REQ-2810: a forecast-lane verdict never feeds graduation ------------

	sc.Step(`^a forecast-lane prediction verdict for a ratified class$`, func() error {
		if err := ratifyGrantedClass(); err != nil {
			return err
		}
		// The class is mid-SECOND climb at auto_notice. That is the state with the most to lose: a
		// deviation credited here would demote it to approve AND zero both streaks, so if the forecast lane
		// could reach the ladder at all, this scenario would show it in three fields at once.
		st.gradSt = &countingGradStore{inner: policy.NewMemGraduationStore().Seed(policy.ClassState{
			OpClass:        grantedClass,
			Level:          policy.LevelAutoNotice,
			CleanRunCount:  5,
			NoticeRunCount: 3,
			LastOutcome:    policy.OutcomeVerifiedClean,
		})}
		st.ladder = policy.NewLadder(5, st.gradSt, nil)
		st.before = st.ladder.State(context.Background(), grantedClass)
		if st.before.Level != policy.LevelAutoNotice || st.before.NoticeRunCount != 3 {
			return fmt.Errorf("the ladder must start at auto_notice mid-second-climb, got %+v", st.before)
		}

		// A NEVER-EXECUTED prediction — the forecast lane's whole domain. Its post-state holds a surprise
		// host the prediction never named, which is what makes the authored verdict a DEVIATION: the one
		// outcome that demotes at any level.
		st.store = falsify.NewMemStore()
		st.store.Seed(predict.PredictionRecord{
			Prediction: verify.Prediction{
				ActionID: "act-forecast-1", PlanHash: "plan-forecast-1", TargetHost: "pve01", Site: "nl",
				PredictedHosts: map[string]struct{}{"n8n01": {}},
				PredictedRules: map[string]struct{}{verify.RuleKey("n8n01", "HostDown"): {}},
			},
			ControlHosts:   map[string]struct{}{"web09": {}},
			SchemaVersion:  schema.Version(1),
			PredictionHash: "hash-plan-forecast-1",
		}, fixedNow().Add(-time.Hour))
		// A SECOND prediction for the same class that DID execute. Its adjudication belongs to the action
		// lane, so it must receive no forecast verdict at all — the category boundary in the other
		// direction, asserted in the same pass so neither half can drift alone.
		st.store.SeedExecuted(predict.PredictionRecord{
			Prediction: verify.Prediction{
				ActionID: "act-executed-1", PlanHash: "plan-executed-1", TargetHost: "pve02", Site: "nl",
				PredictedHosts: map[string]struct{}{"n8n02": {}},
				PredictedRules: map[string]struct{}{verify.RuleKey("n8n02", "HostDown"): {}},
			},
			ControlHosts:   map[string]struct{}{"web08": {}},
			SchemaVersion:  schema.Version(1),
			PredictionHash: "hash-plan-executed-1",
		}, fixedNow().Add(-time.Hour))
		return nil
	})

	sc.Step(`^the verdict is processed$`, func() error {
		scorer := &falsify.Scorer{
			Unscored: st.store, Scores: st.store, ForecastVerdicts: st.store, CascadeStats: st.store,
			Observe: func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
				// A surprise host: predicted nowhere, alerting now ⇒ the verdict is a deviation.
				return []verify.ObservedAlert{{Host: "web09", Rule: "HostDown", Site: "nl"}}, true
			},
			Baseline: func(context.Context, time.Time) ([]verify.ObservedAlert, map[string]bool, bool) {
				return nil, nil, true // established and empty: the verdict is licensed, nothing is excluded
			},
			WindowFloor: falsify.DefaultWindowFloor,
			Now:         fixedNow,
		}
		st.scoreRes, st.scoreErr = scorer.ScoreDue(context.Background())
		return nil
	})

	sc.Step(`^no ladder transition occurs$`, func() error {
		if st.scoreErr != nil {
			return fmt.Errorf("the forecast lane failed before it could be observed: %v", st.scoreErr)
		}
		// FIRST: the lane really ran and really authored the demoting-shaped verdict. Without this the
		// scenario would pass trivially whenever the forecast lane did nothing at all, which is the most
		// likely way an oracle like this rots into a tautology.
		if st.scoreRes.Deviations != 1 {
			return fmt.Errorf("the forecast lane must have authored exactly one DEVIATION for this oracle to mean anything, got %+v", st.scoreRes)
		}
		if v, ok := st.store.VerdictOf("act-forecast-1"); !ok || v != safety.VerdictDeviation {
			return fmt.Errorf("expected a persisted forecast deviation verdict, got %q ok=%v", v, ok)
		}
		// THE CATEGORY BOUNDARY, OTHER DIRECTION: an executed prediction gets no forecast verdict.
		if v, ok := st.store.VerdictOf("act-executed-1"); ok {
			return fmt.Errorf("an EXECUTED prediction received a forecast verdict (%q) — the action lane owns its adjudication", v)
		}
		// THEN: the ladder did not move, and was never even written to.
		after := st.ladder.State(context.Background(), grantedClass)
		if after != st.before {
			return fmt.Errorf("the forecast lane moved the ladder: level %s -> %s, clean streak %d -> %d, notice streak %d -> %d",
				st.before.Level, after.Level, st.before.CleanRunCount, after.CleanRunCount,
				st.before.NoticeRunCount, after.NoticeRunCount)
		}
		if st.gradSt.writes != 0 {
			return fmt.Errorf("the forecast lane wrote to the graduation store %d time(s) — it must not reach the ladder at all", st.gradSt.writes)
		}
		return nil
	})
}

// chainDeps is the minimal runner.Deps for the REQ-2805 chain: a model that only stops (the workflow is not
// driven here — the gate and execute activities are called directly), one read-only tool, and a prediction
// gate whose graph knows the target. Nothing here can authorize anything.
func chainDeps() runner.Deps {
	tools := agent.NewReadOnlyToolSet()
	graph := predict.NewDependencyGraph(map[string][]string{"svc01": {"db01"}})
	gate := &predict.PredictionGate{
		Store: predict.NewMemPredictionStore(),
		Model: &predict.InfragraphModel{Graph: graph, DefaultRules: []string{"FluxDrift"}, MaxDepth: 3},
		Mode:  predict.ModeEnforce,
	}
	return runner.Deps{
		Model:              stopCompleter{},
		Tools:              tools,
		Limits:             agent.DefaultLimits(),
		PredictionEligible: func(string) bool { return true },
		Gate:               gate,
		Ledger:             audit.NewLedger(),
		Mutation:           safety.NewReadOnlyChokepoint(),
	}
}
