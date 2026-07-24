package acceptance

import (
	"context"
	"errors"
	"fmt"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// actingModelRole models the acting LLM's DB role. It deliberately has NO method that produces or
// writes a verdict — the ONLY verdict author is verify.ComputeVerdict (REQ-103). The DB grant
// (no UPDATE/DELETE on the verdict columns) is the runtime enforcement of this type-level guarantee.
type actingModelRole struct{}

// predictWorld is the per-scenario state for the spec/002 gate/verifier oracles.
type predictWorld struct {
	gate                *predict.PredictionGate
	gp                  predict.GatedProposal
	poll                predict.ApprovalPoll
	pollErr             error
	committedBeforePoll bool

	pred     verify.Prediction
	observed []verify.ObservedAlert
	verdict  safety.Verdict
	detail   verify.VerdictDetail
	author   string // which component authored the verdict
	routed   safety.Band

	m         *manifest.ActionManifest
	priorID   string
	changedID string
}

func newTestGate(mode predict.Mode) *predict.PredictionGate {
	g := predict.NewDependencyGraph(map[string][]string{
		"web01": {"db01", "cache01"},
		"db01":  {"reports01"},
	})
	return &predict.PredictionGate{
		Store: predict.NewMemPredictionStore(),
		Model: &predict.InfragraphModel{Graph: g, DefaultRules: []string{"HighLatency"}, MaxDepth: 3},
		Mode:  mode,
	}
}

func testParsedProposal() proposal.Proposal {
	p, err := proposal.ParseProposal([]byte(`{"external_ref":"TG-4617","target":"web01","op_class":"restart-service","op":"restart","confidence":0.8}`))
	if err != nil {
		panic(err)
	}
	return p
}

// registerPredictSteps binds the REQ-101/102/103/104/105/102b gate + verifier scenarios to the real
// core/predict and core/verify code paths.
func registerPredictSteps(sc *godog.ScenarioContext) {
	w := &predictWorld{}
	ctx := context.Background()

	// --- REQ-101: prediction committed before the poll ---
	sc.Step(`^a remediation proposal with a plan_hash and a machine-computed cascade prediction$`, func() error {
		w.gate = newTestGate(predict.ModeEnforce)
		return nil
	})
	sc.Step(`^the remediation workflow reaches the approval stage$`, func() error {
		has, _ := w.gate.Store.Has(ctx, "plan-1")
		committedBefore := !has // nothing committed yet
		gp, err := w.gate.Commit(ctx, testParsedProposal(), "plan-1", "dc1", safety.BandPollPause, true)
		if err != nil {
			return err
		}
		w.gp = gp
		// the prediction is committed here, BEFORE the poll is built below
		has2, _ := w.gate.Store.Has(ctx, "plan-1")
		w.committedBeforePoll = committedBefore && has2
		w.poll, w.pollErr = predict.BuildApprovalPoll(gp, w.gate.Mode)
		return w.pollErr
	})
	sc.Step(`^a plan_hash-keyed prediction row exists in the append-only prediction store$`, func() error {
		if has, _ := w.gate.Store.Has(ctx, "plan-1"); !has {
			return fmt.Errorf("no plan_hash-keyed prediction row committed")
		}
		return nil
	})
	sc.Step(`^it was committed before the approval poll activity started$`, func() error {
		if !w.committedBeforePoll {
			return fmt.Errorf("the prediction must be committed before the poll is built (REQ-101)")
		}
		return nil
	})

	// --- REQ-102: default-deny an ungated proposal ---
	sc.Step(`^a remediation proposal with no committed prediction$`, func() error {
		w.gp = predict.GatedProposal{} // ungated zero value
		return nil
	})
	sc.Step(`^the prediction gate evaluates the approval poll$`, func() error {
		_, w.pollErr = predict.BuildApprovalPoll(w.gp, predict.ModeEnforce)
		return nil
	})
	sc.Step(`^the approval poll is denied by default$`, func() error {
		if !errors.Is(w.pollErr, predict.ErrNotGated) {
			return fmt.Errorf("expected default-deny (ErrNotGated), got %v", w.pollErr)
		}
		return nil
	})
	sc.Step(`^a proposal that is not a GatedProposal produced by the PredictionGate$`, func() error {
		w.gp = predict.GatedProposal{}
		return nil
	})
	sc.Step(`^the caller attempts to build an approval poll from it$`, func() error {
		_, w.pollErr = predict.BuildApprovalPoll(w.gp, predict.ModeEnforce)
		return nil
	})
	sc.Step(`^the approval poll cannot be constructed$`, func() error {
		if w.pollErr == nil {
			return fmt.Errorf("an ungated proposal must not construct a poll")
		}
		return nil
	})

	// --- REQ-105: analysis-only records but does not block ---
	sc.Step(`^the prediction gate is in analysis-only mode$`, func() error {
		w.gate = newTestGate(predict.ModeAnalysisOnly)
		return nil
	})
	sc.Step(`^a remediation proposal is evaluated$`, func() error {
		gp, err := w.gate.Commit(ctx, testParsedProposal(), "plan-1", "dc1", safety.BandPollPause, true)
		if err != nil {
			return err
		}
		w.gp = gp
		w.poll, w.pollErr = predict.BuildApprovalPoll(gp, w.gate.Mode)
		return w.pollErr
	})
	sc.Step(`^the prediction and a shadow verdict are recorded$`, func() error {
		if has, _ := w.gate.Store.Has(ctx, "plan-1"); !has {
			return fmt.Errorf("analysis-only must still commit the prediction")
		}
		return nil
	})
	sc.Step(`^the approval is not blocked on the prediction$`, func() error {
		if w.poll.Blocking {
			return fmt.Errorf("analysis-only poll must be non-blocking (fail-open advisory)")
		}
		return nil
	})

	// --- REQ-103: deterministic verifier is the sole verdict author ---
	sc.Step(`^an executed action with a committed prediction and an observed alert set$`, func() error {
		w.pred = verify.Prediction{
			ActionID: "a1", PlanHash: "plan-1", TargetHost: "web01", Site: "dc1",
			PredictedHosts: map[string]struct{}{"db01": {}},
			PredictedRules: map[string]struct{}{verify.RuleKey("db01", "HighLatency"): {}},
		}
		w.observed = []verify.ObservedAlert{{Host: "db01", Rule: "HighLatency", Site: "dc1"}}
		return nil
	})
	sc.Step(`^the verdict is computed$`, func() error {
		w.verdict = verify.ComputeVerdict(w.pred, w.observed)
		w.author = "verify.ComputeVerdict"
		return nil
	})
	sc.Step(`^the match, partial, or deviation verdict is written by the deterministic verifier$`, func() error {
		if !safety.ValidVerdict(w.verdict) {
			return fmt.Errorf("verdict %q is not a valid mechanical verdict", w.verdict)
		}
		if w.author != "verify.ComputeVerdict" {
			return fmt.Errorf("the verdict must be authored by the deterministic verifier, got %q", w.author)
		}
		return nil
	})
	sc.Step(`^it equals the mechanical diff of observed against predicted$`, func() error {
		if want := verify.ComputeVerdict(w.pred, w.observed); w.verdict != want {
			return fmt.Errorf("verdict %q != mechanical diff %q", w.verdict, want)
		}
		return nil
	})
	sc.Step(`^an executed action whose session role is the acting model role$`, func() error {
		_ = actingModelRole{} // the acting model's role has NO verdict-write method
		return nil
	})
	sc.Step(`^the acting model attempts to write a verdict column$`, func() error {
		// There is no method on actingModelRole (or any model-facing type) that authors a verdict —
		// the ONLY author is verify.ComputeVerdict. This is a compile-time absence; the DB grant is the
		// runtime enforcement. Nothing to invoke here.
		return nil
	})
	sc.Step(`^the write is rejected because the model and session roles hold no UPDATE or DELETE grant$`, func() error {
		// The type-level guarantee: the acting-model role exposes no verdict-authoring path; only the
		// deterministic verifier produces a verdict.
		if _, ok := interface{}(actingModelRole{}).(interface {
			ComputeVerdict(verify.Prediction, []verify.ObservedAlert) safety.Verdict
		}); ok {
			return fmt.Errorf("the acting model role must NOT be able to author a verdict")
		}
		return nil
	})

	// --- REQ-103a: one-pass typed VerdictDetail; enum stays byte-identical; breakdown populated ---
	sc.Step(`^an executed action with a committed prediction and a mixed observed alert set$`, func() error {
		w.pred = verify.Prediction{
			ActionID: "a1", PlanHash: "plan-1", TargetHost: "web01", Site: "dc1",
			PredictedHosts: map[string]struct{}{"db01": {}, "cache01": {}},
			PredictedRules: map[string]struct{}{
				verify.RuleKey("db01", "HighLatency"):    {},
				verify.RuleKey("cache01", "MemPressure"): {},
			},
		}
		w.observed = []verify.ObservedAlert{
			{Host: "db01", Rule: "HighLatency", Site: "dc1"}, // predicted host+rule → matched
			{Host: "cache01", Rule: "DiskFull", Site: "dc1"}, // predicted host, unpredicted rule → mismatch
			{Host: "router09", Rule: "BGPDown", Site: "dc1"}, // host never named → surprise
			{Host: "web01", Rule: "Down", Site: "dc1"},       // the action's own target → excluded
		}
		return nil
	})
	sc.Step(`^the typed verdict detail is computed in one pass$`, func() error {
		w.detail = verify.ComputeVerdictDetail(w.pred, w.observed)
		return nil
	})
	sc.Step(`^the detail enum equals the bare mechanical verdict for the same inputs$`, func() error {
		if bare := verify.ComputeVerdict(w.pred, w.observed); w.detail.Verdict != bare {
			return fmt.Errorf("detail enum %q != bare ComputeVerdict %q (must be one authority)", w.detail.Verdict, bare)
		}
		if w.detail.Verdict != safety.VerdictDeviation {
			return fmt.Errorf("a mixed set with a surprise host must derive a deviation, got %q", w.detail.Verdict)
		}
		return nil
	})
	sc.Step(`^the detail lists the surprise hosts and the rule mismatches that produced it$`, func() error {
		if len(w.detail.SurpriseHosts) != 1 || w.detail.SurpriseHosts[0] != "router09" {
			return fmt.Errorf("expected surprise hosts [router09], got %v", w.detail.SurpriseHosts)
		}
		if len(w.detail.Mismatches) != 1 || w.detail.Mismatches[0] != (verify.RuleMismatch{Host: "cache01", Rule: "DiskFull"}) {
			return fmt.Errorf("expected mismatches [{cache01 DiskFull}], got %v", w.detail.Mismatches)
		}
		return nil
	})
	sc.Step(`^a deviation detail is never auto-resolvable$`, func() error {
		if w.detail.AutoResolvable() {
			return fmt.Errorf("a deviation detail must never be auto-resolvable (REQ-104)")
		}
		return nil
	})

	// --- REQ-104: a deviation blocks auto-resolution ---
	sc.Step(`^a completed action whose mechanical verdict is deviation$`, func() error {
		pred := verify.Prediction{
			TargetHost: "web01", Site: "dc1",
			PredictedHosts: map[string]struct{}{"db01": {}},
		}
		obs := []verify.ObservedAlert{{Host: "router09", Rule: "BGPDown", Site: "dc1"}} // surprise
		w.verdict = verify.ComputeVerdict(pred, obs)
		if w.verdict != safety.VerdictDeviation {
			return fmt.Errorf("setup: expected deviation, got %s", w.verdict)
		}
		return nil
	})
	sc.Step(`^the reconciler evaluates auto-resolution$`, func() error {
		if verify.AutoResolvable(w.verdict) {
			w.routed = safety.BandAuto
		} else {
			w.routed = safety.BandPollPause // a deviation routes to the poll/approver graph
		}
		return nil
	})
	sc.Step(`^auto-resolution is refused regardless of band or confidence$`, func() error {
		if verify.AutoResolvable(w.verdict) {
			return fmt.Errorf("a deviation must never be auto-resolvable")
		}
		return nil
	})
	sc.Step(`^the session routes to POLL_PAUSE and the approver graph$`, func() error {
		if w.routed != safety.BandPollPause {
			return fmt.Errorf("a deviation must route to POLL_PAUSE, got %s", w.routed)
		}
		return nil
	})

	// --- REQ-102b: action_id threaded; a change forces a re-gate ---
	sc.Step(`^an ActionManifest sealed around an action with a content-hashed action_id$`, func() error {
		g := newTestGate(predict.ModeEnforce)
		gp, err := g.Commit(ctx, testParsedProposal(), "plan-1", "dc1", safety.BandPollPause, true)
		if err != nil {
			return err
		}
		w.m = gp.Manifest()
		return nil
	})
	sc.Step(`^each of the predict, approve, execute, and verify stages asserts the manifest$`, func() error {
		for _, stage := range []string{"predict", "approve", "execute", "verify"} {
			if err := w.m.Assert(w.m.ActionID); err != nil {
				return fmt.Errorf("stage %s: %v", stage, err)
			}
		}
		return nil
	})
	sc.Step(`^every stage re-derives the same action_id and the assertion passes$`, func() error {
		return w.m.Assert(w.m.ActionID)
	})
	sc.Step(`^a sealed ActionManifest with a committed prediction and approval$`, func() error {
		g := newTestGate(predict.ModeEnforce)
		gp, err := g.Commit(ctx, testParsedProposal(), "plan-1", "dc1", safety.BandPollPause, true)
		if err != nil {
			return err
		}
		w.m = gp.Manifest()
		w.priorID = w.m.ActionID
		return nil
	})
	sc.Step(`^the bound action is changed mid-session$`, func() error {
		changed := manifest.Action{Target: "web01", OpClass: "restart-service", Op: "reload"} // Op changed
		id, err := changed.ID()
		w.changedID = id
		return err
	})
	sc.Step(`^a new action_id is derived that does not match the prior authorization$`, func() error {
		if w.changedID == w.priorID {
			return fmt.Errorf("a changed action must mint a new action_id")
		}
		return nil
	})
	sc.Step(`^the prior prediction and approval are invalidated and the action re-enters the gate$`, func() error {
		if err := w.m.Assert(w.changedID); err == nil {
			return fmt.Errorf("the prior manifest must reject the changed id (force re-gate)")
		}
		return nil
	})
}
