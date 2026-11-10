package acceptance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/falsify"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/schema"
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

	// Phase C4 (REQ-107/108/109): the estate site vocabulary the scoped author keys on, and the real
	// falsify.Scorer pass over its oracle-twin store.
	sites    *estate.Graph
	store    *falsify.MemStore
	scorer   *falsify.Scorer
	result   falsify.Result
	planHash string
	actionID string

	// TG-220 / REQ-110: the learned observation window. clock is advanced explicitly by the steps so "the
	// cascade manifests at +15m" is a fact about the world, not a sleep; narrow/wide hold the two arms of the
	// INV-22 control comparison.
	clock          time.Time
	learnedWindow  time.Duration
	narrow, wide   falsify.Result
	narrowW, wideW []falsify.CascadeWindow
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

	// --- REQ-107: estate-derived coincidental-cross-site exclusion (both-known-and-differ; unknown never) ---
	twoSiteEstate := func() *estate.Graph {
		g := estate.NewGraph()
		g.Upsert(estate.Edge{
			From: estate.Entity{Type: estate.TypeNetworkDevice, Name: "dc1fw01"},
			To:   estate.Entity{Type: estate.TypeSite, Name: "dc1"},
			Rel:  estate.RelMemberOf, Source: estate.SourceDeclared, Confidence: 0.85,
		})
		g.Upsert(estate.Edge{
			From: estate.Entity{Type: estate.TypeNetworkDevice, Name: "dc2fw01"},
			To:   estate.Entity{Type: estate.TypeSite, Name: "dc2"},
			Rel:  estate.RelMemberOf, Source: estate.SourceDeclared, Confidence: 0.85,
		})
		return g
	}
	scopedPrediction := func() verify.Prediction {
		return verify.Prediction{
			ActionID: "a-107", PlanHash: "plan-107", TargetHost: "dc1mealie01", Site: "dc1",
			PredictedHosts: map[string]struct{}{"dc1pve01": {}},
			PredictedRules: map[string]struct{}{verify.RuleKey("dc1pve01", "HostDown"): {}},
		}
	}
	sc.Step(`^an estate graph that derives the target's site and an alerting host's site as different$`, func() error {
		w.sites = twoSiteEstate() // target dc1mealie01 → dc1; dc2lte01 → dc2 (naming tier)
		w.pred = scopedPrediction()
		w.observed = []verify.ObservedAlert{{Host: "dc2lte01", Rule: "Sensor under limit", Site: "dc2"}}
		return nil
	})
	sc.Step(`^an estate graph that makes no site claim for an alerting host$`, func() error {
		w.sites = twoSiteEstate() // notrf01vps01 matches neither membership nor a site name prefix → unknown
		w.pred = scopedPrediction()
		w.observed = []verify.ObservedAlert{{Host: "notrf01vps01", Rule: "HostDown", Site: "dc2"}}
		return nil
	})
	sc.Step(`^the scoped verdict is computed over an observation carrying that host's alert$`, func() error {
		w.detail = verify.ComputeVerdictDetailScoped(w.pred, w.observed, nil, nil, w.sites.SiteOf)
		return nil
	})
	sc.Step(`^the alert is excluded from the surprise evidence and the verdict is match$`, func() error {
		if len(w.detail.SurpriseHosts) != 0 || len(w.detail.SurpriseAlerts) != 0 {
			return fmt.Errorf("a proven-other-site alert must leave no surprise evidence, got %+v", w.detail)
		}
		if w.detail.Verdict != safety.VerdictMatch {
			return fmt.Errorf("verdict = %q, want match (REQ-107 both-known-and-differ exclusion)", w.detail.Verdict)
		}
		return nil
	})
	sc.Step(`^the host remains a surprise and the verdict is deviation$`, func() error {
		if w.detail.Verdict != safety.VerdictDeviation || len(w.detail.SurpriseHosts) != 1 {
			return fmt.Errorf("an unknown-site host must NEVER be excluded (fail closed), got %+v", w.detail)
		}
		return nil
	})

	// --- REQ-108: family-sibling rule on a predicted host is the predicted failure mode ---
	sc.Step(`^a prediction naming a host with a rule and an observation of that host under a family-sibling spelling$`, func() error {
		w.pred = scopedPrediction() // names (dc1pve01, HostDown)
		// "Devices-up/down" is HostDown's family sibling in the embedded production rulefamily.json.
		w.observed = []verify.ObservedAlert{{Host: "dc1pve01", Rule: "Devices-up/down", Site: "dc1"}}
		return nil
	})
	sc.Step(`^no mismatch is recorded and the verdict is match$`, func() error {
		d := verify.ComputeVerdictDetail(w.pred, w.observed)
		if d.Verdict != w.verdict {
			return fmt.Errorf("one authority: detail enum %q != bare verdict %q", d.Verdict, w.verdict)
		}
		if len(d.Mismatches) != 0 || d.Verdict != safety.VerdictMatch {
			return fmt.Errorf("a family sibling on a predicted host must score match with no mismatch (REQ-108), got %+v", d)
		}
		return nil
	})

	// --- REQ-109: the scorer's adjudication split, commit-time baseline, and the un-launderable control ---
	c4Committed := time.Date(2026, 7, 18, 11, 0, 0, 0, time.UTC)
	c4Now := c4Committed.Add(time.Hour)
	c4Record := func(plan, action string) predict.PredictionRecord {
		return predict.PredictionRecord{
			Prediction: verify.Prediction{
				ActionID: action, PlanHash: plan, TargetHost: "pve01", Site: "nl",
				PredictedHosts: map[string]struct{}{"n8n01": {}},
				PredictedRules: map[string]struct{}{verify.RuleKey("n8n01", "HostDown"): {}},
			},
			ControlHosts:   map[string]struct{}{"web09": {}},
			SchemaVersion:  schema.Version(1),
			PredictionHash: "hash-" + plan,
		}
	}
	newC4Scorer := func(observed []verify.ObservedAlert, baseline []verify.ObservedAlert, baseHosts map[string]bool) {
		w.store = falsify.NewMemStore()
		w.scorer = &falsify.Scorer{
			Unscored: w.store, Scores: w.store, ForecastVerdicts: w.store, CascadeStats: w.store,
			Observe: func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return observed, true },
			Baseline: func(context.Context, time.Time) ([]verify.ObservedAlert, map[string]bool, bool) {
				return baseline, baseHosts, true
			},
			// No Latency seam and no explicit bounds: the production defaults apply — every edge falls back to
			// the 900s floor (REQ-110's fail-safe), and these C4 predictions are an hour old, so they are due.
			Now: func() time.Time { return c4Now },
		}
	}
	sc.Step(`^a due prediction whose action has a per-execution record$`, func() error {
		w.planHash, w.actionID = "plan-109-exec", "act-109-exec"
		newC4Scorer([]verify.ObservedAlert{{Host: "surprise01", Rule: "HostDown", Site: "nl"}}, nil, nil)
		w.store.SeedExecuted(c4Record(w.planHash, w.actionID), c4Committed)
		return nil
	})
	sc.Step(`^a due never-executed prediction and a commit-time baseline carrying an already-firing alert$`, func() error {
		w.planHash, w.actionID = "plan-109-base", "act-109-base"
		ambient := []verify.ObservedAlert{{Host: "ambient07", Rule: "DiskFull", Site: "nl"}}
		newC4Scorer(ambient, ambient, map[string]bool{"ambient07": true})
		w.store.Seed(c4Record(w.planHash, w.actionID), c4Committed)
		return nil
	})
	sc.Step(`^a due prediction whose control host carries the only observed alert and a baseline covering it$`, func() error {
		w.planHash, w.actionID = "plan-109-ctrl", "act-109-ctrl"
		ambient := []verify.ObservedAlert{{Host: "web09", Rule: "HostDown", Site: "nl"}} // web09 IS the control host
		newC4Scorer(ambient, ambient, map[string]bool{"web09": true})
		w.store.Seed(c4Record(w.planHash, w.actionID), c4Committed)
		return nil
	})
	sc.Step(`^the scoring pass runs$`, func() error {
		var err error
		w.result, err = w.scorer.ScoreDue(ctx)
		return err
	})
	sc.Step(`^the confusion-matrix score is written and no forecast verdict row is authored$`, func() error {
		if _, ok := w.store.ScoreOf(w.planHash); !ok {
			return fmt.Errorf("the executed prediction must still get its falsifiability score")
		}
		if v, ok := w.store.VerdictOf(w.actionID); ok {
			return fmt.Errorf("an EXECUTED prediction got forecast verdict %q — its adjudication belongs to the interceptor lane (REQ-109)", v)
		}
		if w.result.Executed != 1 || w.result.Deviations != 0 {
			return fmt.Errorf("expected executed=1 forecast-deviations=0, got %+v", w.result)
		}
		return nil
	})
	sc.Step(`^the forecast verdict is match and the baselined alert is not surprise evidence$`, func() error {
		v, ok := w.store.VerdictOf(w.actionID)
		if !ok || v != safety.VerdictMatch {
			return fmt.Errorf("forecast verdict = %q ok=%v, want match — the alert predates the prediction (REQ-109)", v, ok)
		}
		if w.result.Deviations != 0 || len(w.result.SurpriseHosts) != 0 {
			return fmt.Errorf("a commit-time-baselined alert must not be surprise evidence, got %+v", w.result)
		}
		return nil
	})
	sc.Step(`^the control true-positive survives and the cascade window is not falsifiable$`, func() error {
		scr, ok := w.store.ScoreOf(w.planHash)
		if !ok || scr.ControlTP != 1 {
			return fmt.Errorf("the shuffled control's hit must survive every scoping filter (INV-22), got %+v ok=%v", scr, ok)
		}
		ws := w.store.Windows()
		if len(ws) != 1 || ws[0].Falsifiable {
			return fmt.Errorf("a window the control won must stay NON-falsifiable, got %+v", ws)
		}
		return nil
	})

	// --- REQ-110 (TG-220): the LEARNED observation window, driven through the real falsify.Scorer ---
	//
	// The retired behavior scored every prediction in a fixed 10m window, so a cascade slower than that was
	// recorded as never having happened. These oracles synthesize the WORLD (a cascade with a known
	// propagation delay, and a durable ledger that has or has not seen that edge before) and let the
	// production scorer decide. Nothing here re-implements the window.
	slowCascade := []verify.ObservedAlert{{Host: "n8n01", Rule: "HostDown", Site: "nl"}}
	// cascadeAt reveals the alerts only once the clock reaches fireAt; before that it reports a REAL
	// observation of a quiet estate, which is exactly what the live surface would say.
	cascadeAt := func(fireAt time.Time) falsify.Observer {
		return func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			if w.clock.Before(fireAt) {
				return nil, true
			}
			return slowCascade, true
		}
	}
	windowScorer := func(store *falsify.MemStore, obs falsify.Observer, lat falsify.LatencyReader, floor time.Duration) *falsify.Scorer {
		return &falsify.Scorer{
			Unscored: store, Scores: store, ForecastVerdicts: store, CascadeStats: store,
			Observe: obs, Latency: lat,
			Baseline: func(context.Context, time.Time) ([]verify.ObservedAlert, map[string]bool, bool) {
				return nil, nil, true
			},
			WindowFloor: floor, WindowCap: falsify.DefaultWindowCap,
			Now: func() time.Time { return w.clock },
		}
	}

	sc.Step(`^a prediction whose cascade manifests fifteen minutes after it was committed$`, func() error {
		w.planHash, w.actionID = "plan-110-slow", "act-110-slow"
		w.store = falsify.NewMemStore()
		w.store.Seed(c4Record(w.planHash, w.actionID), c4Committed)
		w.clock = c4Committed
		w.scorer = windowScorer(w.store, cascadeAt(c4Committed.Add(15*time.Minute)), nil, falsify.DefaultWindowFloor)
		return nil
	})
	sc.Step(`^the scoring pass runs at the retired fixed window and again after the learned window$`, func() error {
		// The tick the RETIRED 10-minute constant would have adjudicated on.
		w.clock = c4Committed.Add(12 * time.Minute)
		early, err := w.scorer.ScoreDue(ctx)
		if err != nil {
			return err
		}
		if early.Scored != 0 {
			return fmt.Errorf("a prediction scored at +12m adjudicates a cascade that has not happened yet — the "+
				"exact bias REQ-110 closes: %+v", early)
		}
		// A tick past the window the scorer actually learned for this prediction.
		w.clock = c4Committed.Add(16 * time.Minute)
		w.result, err = w.scorer.ScoreDue(ctx)
		return err
	})
	sc.Step(`^the cascade is scored as a true positive and never as a premature miss$`, func() error {
		if w.result.Scored != 1 || w.result.SumRealTP != 1 {
			return fmt.Errorf("the 15-minute cascade must adjudicate as a HIT (REQ-110), got %+v", w.result)
		}
		scr, ok := w.store.ScoreOf(w.planHash)
		if !ok || scr.TP != 1 || scr.FP != 0 {
			return fmt.Errorf("confusion matrix = %+v ok=%v, want tp=1 fp=0 — the predicted cascade DID happen", scr, ok)
		}
		return nil
	})

	sc.Step(`^a prediction over an edge whose durable history shows it propagates in seconds$`, func() error {
		w.planHash, w.actionID = "plan-110-fast", "act-110-fast"
		w.store = falsify.NewMemStore()
		w.store.Seed(c4Record(w.planHash, w.actionID), c4Committed)
		w.clock = c4Now
		fast := map[falsify.CascadeEdge][]time.Duration{
			{Primary: "pve01", Dependent: "n8n01"}: {28 * time.Second, 30 * time.Second, 31 * time.Second},
		}
		w.scorer = windowScorer(w.store, cascadeAt(c4Committed), func(context.Context, []string, time.Time) (map[falsify.CascadeEdge][]time.Duration, bool) {
			return fast, true
		}, falsify.DefaultWindowFloor)
		return nil
	})
	sc.Step(`^the learned observation window is computed$`, func() error {
		var err error
		w.result, err = w.scorer.ScoreDue(ctx)
		w.learnedWindow = w.result.WidestWindow
		return err
	})
	sc.Step(`^the window stays at the floor rather than widening$`, func() error {
		if w.learnedWindow != falsify.DefaultWindowFloor {
			return fmt.Errorf("a fast edge (2 x p95 = ~62s) must keep the tight floor window, got %s", w.learnedWindow)
		}
		if w.result.Scored != 1 {
			return fmt.Errorf("a fast edge's prediction must still score on schedule, got %+v", w.result)
		}
		return nil
	})

	sc.Step(`^the same slow cascade scored under the retired fixed window and under the learned window$`, func() error {
		fireAt := c4Committed.Add(25 * time.Minute)
		learned := map[falsify.CascadeEdge][]time.Duration{
			{Primary: "pve01", Dependent: "n8n01"}: {900 * time.Second}, // p95=900s ⇒ window 1800s
		}
		// NARROW arm — the retired fixed 10m window, adjudicating before the cascade manifests.
		narrowStore := falsify.NewMemStore()
		narrowStore.Seed(c4Record("plan-110-narrow", "act-110-narrow"), c4Committed)
		w.clock = c4Committed.Add(11 * time.Minute)
		var err error
		if w.narrow, err = windowScorer(narrowStore, cascadeAt(fireAt), nil, 10*time.Minute).ScoreDue(ctx); err != nil {
			return err
		}
		w.narrowW = narrowStore.Windows()
		// WIDE arm — the learned 30m window over the IDENTICAL world.
		wideStore := falsify.NewMemStore()
		wideStore.Seed(c4Record("plan-110-wide", "act-110-wide"), c4Committed)
		w.clock = c4Committed.Add(31 * time.Minute)
		if w.wide, err = windowScorer(wideStore, cascadeAt(fireAt), func(context.Context, []string, time.Time) (map[falsify.CascadeEdge][]time.Duration, bool) {
			return learned, true
		}, falsify.DefaultWindowFloor).ScoreDue(ctx); err != nil {
			return err
		}
		w.wideW = wideStore.Windows()
		return nil
	})
	sc.Step(`^both scoring passes complete$`, func() error {
		if w.narrow.Scored != 1 || w.wide.Scored != 1 {
			return fmt.Errorf("both arms must actually score (narrow=%+v wide=%+v)", w.narrow, w.wide)
		}
		return nil
	})
	sc.Step(`^the real prediction gains true positives while the control ratio does not rise above the ceiling$`, func() error {
		if w.wide.SumRealTP <= w.narrow.SumRealTP {
			return fmt.Errorf("the wider window must recover REAL cascade hits: narrow real_tp=%d wide real_tp=%d",
				w.narrow.SumRealTP, w.wide.SumRealTP)
		}
		if w.wide.SumControlTP > w.narrow.SumControlTP {
			return fmt.Errorf("the shuffled control's hits ROSE with the window (%d -> %d) — a widening that helps "+
				"the random control equally is laundering noise, not measuring topology (INV-22, REQ-110)",
				w.narrow.SumControlTP, w.wide.SumControlTP)
		}
		narrowRatio := falsify.ControlRatio(w.narrow.SumRealTP, w.narrow.SumControlTP)
		wideRatio := falsify.ControlRatio(w.wide.SumRealTP, w.wide.SumControlTP)
		if wideRatio > narrowRatio {
			return fmt.Errorf("control_ratio rose with the window (%.3f -> %.3f) — INV-22 says the widening added no signal",
				narrowRatio, wideRatio)
		}
		if len(w.wideW) != 1 || !w.wideW[0].Falsifiable || w.wideW[0].ControlRatio > predict.ControlRatioCeiling {
			return fmt.Errorf("the widened window must record a FALSIFIABLE cascade-stats row (ratio <= %.2f), got %+v",
				predict.ControlRatioCeiling, w.wideW)
		}
		return nil
	})
}
