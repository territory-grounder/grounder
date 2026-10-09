package eval

import (
	"bytes"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/eval/gate"
)

// TestAggregate_ReportsResolutionRecall proves TG-507's REPORTED retrieval-recall axis rides onto the
// scorecard: Aggregate measures TG-491's leave-one-out resolution-recall over the SHIPPED seed corpus
// (deploy/knowledge/corpus.seed.json) and carries recall/ceiling/of-findable — all > 0 — with no gateway, no
// judge, no operator label. It is a REPORTED field only; the load-bearing proof that the gate never bars on
// it lives in eval/gate (TestResolutionRecall_ReportedNotGated).
func TestAggregate_ReportsResolutionRecall(t *testing.T) {
	// The retrieval metric is independent of these sessions — it is measured over the shipped seed corpus, not
	// the passed-in sessions/scores — so a single trivial session suffices to build a scorecard.
	sc := Aggregate(
		[]Session{{Ref: "a", Proposed: true, Predicted: true}},
		[]Score{{Ref: "a", Scores: map[string]int{"sensible_proposal": 4}}},
	)
	if sc.ResolutionRecallOfFindable <= 0 {
		t.Fatalf("resolution_recall_of_findable = %.3f, want > 0 (the shipped seed corpus must be measured onto the scorecard)", sc.ResolutionRecallOfFindable)
	}
	if sc.ResolutionRecall <= 0 {
		t.Fatalf("resolution_recall = %.3f, want > 0", sc.ResolutionRecall)
	}
	// Structural honesty (mirrors resolution_recall_test.go): recall can never exceed ceiling; of-findable in [0,1].
	if sc.ResolutionRecall > sc.ResolutionRecallCeiling+1e-9 {
		t.Fatalf("resolution_recall %.3f exceeds ceiling %.3f — impossible, the metric is broken", sc.ResolutionRecall, sc.ResolutionRecallCeiling)
	}
	if sc.ResolutionRecallOfFindable > 1+1e-9 {
		t.Fatalf("of-findable %.3f out of [0,1]", sc.ResolutionRecallOfFindable)
	}
	// It must ride onto the SERIALIZED scorecard — scorecard.json is what the change-gate diff and the
	// committed baseline actually read, so a field the struct carries but ScorecardJSON drops is invisible.
	if !bytes.Contains(ScorecardJSON(sc), []byte(`"resolution_recall_of_findable"`)) {
		t.Fatal("resolution_recall_of_findable missing from the serialized scorecard JSON — the change-gate diff would never see it")
	}
}

func TestRetryComplete(t *testing.T) {
	// transient error (transport) twice, then success → 3 attempts, success returned, backoff called twice.
	calls, backoffs := 0, 0
	raw, err := retryComplete(3, func(int) { backoffs++ }, func() (string, error) {
		calls++
		if calls < 3 {
			return "", &model.ModelError{Class: "transport"}
		}
		return "ok", nil
	})
	if err != nil || raw != "ok" || calls != 3 || backoffs != 2 {
		t.Fatalf("transient-then-success: raw=%q err=%v calls=%d backoffs=%d (want ok/nil/3/2)", raw, err, calls, backoffs)
	}
	// permanent error (auth) → NO retry: exactly 1 attempt, error returned, no backoff.
	calls, backoffs = 0, 0
	_, err = retryComplete(3, func(int) { backoffs++ }, func() (string, error) {
		calls++
		return "", &model.ModelError{Class: "auth"}
	})
	if err == nil || calls != 1 || backoffs != 0 {
		t.Fatalf("permanent auth: err=%v calls=%d backoffs=%d (want err/1/0)", err, calls, backoffs)
	}
	// all transient → exhaust maxTries, return the last error (a nil backoff is tolerated).
	calls = 0
	_, err = retryComplete(3, nil, func() (string, error) {
		calls++
		return "", &model.ModelError{Class: "timeout"}
	})
	if err == nil || calls != 3 {
		t.Fatalf("exhaust: err=%v calls=%d (want err/3)", err, calls)
	}
}

func TestParseScore(t *testing.T) {
	// clean JSON
	s, err := ParseScore("eval-01", `{"correct_diagnosis":4,"evidence_grounded":3,"sensible_proposal":5,"appropriate_band":5,"falsifiable_prediction":2,"comment":"ok"}`)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if s.Scores["appropriate_band"] != 5 || s.Comment != "ok" {
		t.Fatalf("bad parse: %+v", s)
	}
	// wrapped in prose + fences + out-of-range clamps
	s2, err := ParseScore("eval-02", "Here is my verdict:\n```json\n{\"correct_diagnosis\":9,\"appropriate_band\":0,\"comment\":\"x\"}\n```\nthanks")
	if err != nil {
		t.Fatalf("wrapped: %v", err)
	}
	if s2.Scores["correct_diagnosis"] != 5 || s2.Scores["appropriate_band"] != 1 {
		t.Fatalf("clamp failed: %+v", s2.Scores)
	}
	// no json → error
	if _, err := ParseScore("eval-03", "I cannot score this"); err == nil {
		t.Fatal("want error on no-json reply")
	}
}

func TestAggregate(t *testing.T) {
	sessions := []Session{
		{Ref: "eval-01", Band: "POLL_PAUSE", Proposed: true, Predicted: true, OpClass: "restart-service", StepCount: 2},
		{Ref: "eval-02", Band: "AUTO_NOTICE", Proposed: true, Predicted: true, OpClass: "start-guest", StepCount: 4},
		{Ref: "eval-03", Band: "", Proposed: false, Predicted: false, StepCount: 1}, // stop: no op-class
		{Ref: "eval-04", Band: "POLL_PAUSE", Proposed: true, Predicted: false, Mutated: true, OpClass: "restart-service", StepCount: 5},
	}
	scores := []Score{
		{Ref: "eval-01", Scores: map[string]int{"correct_diagnosis": 4, "appropriate_band": 5}},
		{Ref: "eval-02", Scores: map[string]int{"correct_diagnosis": 2, "appropriate_band": 3}},
	}
	sc := Aggregate(sessions, scores)
	if sc.N != 4 {
		t.Fatalf("N=%d", sc.N)
	}
	if sc.Bands["POLL_PAUSE"] != 2 || sc.Bands["none"] != 1 {
		t.Fatalf("bands=%v", sc.Bands)
	}
	if sc.ProposalRate != 0.75 {
		t.Fatalf("proposal rate=%v", sc.ProposalRate)
	}
	if sc.PredictionRate != 0.5 {
		t.Fatalf("prediction rate=%v", sc.PredictionRate)
	}
	// A4 autonomy rate: only the auto-actuating bands (here AUTO_NOTICE, 1 of 4) count — POLL_PAUSE needs a
	// human and a stand-down takes no action; neither is autonomous.
	if sc.AutonomyRate != 0.25 {
		t.Fatalf("autonomy rate=%v (want 0.25 = 1 AUTO_NOTICE / 4)", sc.AutonomyRate)
	}
	// A5 fault-class breadth: distinct proposed op-classes (restart-service x2 + start-guest = 2 distinct);
	// the stand-down contributes none.
	if sc.FaultClassBreadth != 2 || len(sc.FaultClasses) != 2 || sc.FaultClasses[0] != "restart-service" || sc.FaultClasses[1] != "start-guest" {
		t.Fatalf("fault-class breadth=%d classes=%v (want 2 [restart-service start-guest])", sc.FaultClassBreadth, sc.FaultClasses)
	}
	// A6 decision efficiency: mean cycles per session = (2+4+1+5)/4 = 3.0.
	if sc.MeanDecisionSteps != 3.0 {
		t.Fatalf("mean decision steps=%v (want 3.0 = (2+4+1+5)/4)", sc.MeanDecisionSteps)
	}
	if sc.MutationCount != 1 { // captured but flagged — mutation MUST be 0 in a healthy read-only run
		t.Fatalf("mutation count=%d", sc.MutationCount)
	}
	if sc.DimMeans["correct_diagnosis"] != 3.0 || sc.DimMeans["appropriate_band"] != 4.0 {
		t.Fatalf("dim means=%v", sc.DimMeans)
	}
	// Fixed denominator (v2): the three unmeasured dimensions are floored at AbstentionFloor, never
	// dropped, so Overall = (3+4+1+1+1)/5 = 2.0 — NOT (3+4)/2 = 3.5, which is what the pre-v2 formula
	// produced and what let an abstaining run outscore a producing one.
	if sc.Overall != 2.0 {
		t.Fatalf("overall=%v (want 2.0 under fixed-denominator-v2)", sc.Overall)
	}
	if sc.OverallFormula != OverallFormulaV2 {
		t.Fatalf("overall_formula=%q (want %q)", sc.OverallFormula, OverallFormulaV2)
	}
	if sc.DimSamples["correct_diagnosis"] != 2 || sc.DimSamples["falsifiable_prediction"] != 0 {
		t.Fatalf("dim samples=%v (measured vs imputed must be distinguishable)", sc.DimSamples)
	}
}

// TG-61 seq C: falsifiable_prediction is N/A for a grounded stand-down (no action ⇒ no prediction), so a
// stand-down's floor must NOT drag the dimension mean — it is EXCLUDED from that dimension only, while
// every other dimension still counts every session.
func TestAggregateFalsifiablePredictionExcludesStandDowns(t *testing.T) {
	sessions := []Session{
		{Ref: "p1", Band: "POLL_PAUSE", Proposed: true, Predicted: true},
		{Ref: "p2", Band: "POLL_PAUSE", Proposed: true, Predicted: true},
		{Ref: "stop", Band: "", Proposed: false, Predicted: false}, // grounded stand-down
	}
	scores := []Score{
		{Ref: "p1", Scores: map[string]int{"falsifiable_prediction": 4, "correct_diagnosis": 5}},
		{Ref: "p2", Scores: map[string]int{"falsifiable_prediction": 4, "correct_diagnosis": 3}},
		{Ref: "stop", Scores: map[string]int{"falsifiable_prediction": 1, "correct_diagnosis": 4}},
	}
	sc := Aggregate(sessions, scores)
	// falsifiable_prediction is the PROPOSER-ONLY mean (4+4)/2 = 4.0 — the stand-down's 1 is excluded.
	if got := sc.DimMeans["falsifiable_prediction"]; got != 4.0 {
		t.Fatalf("falsifiable_prediction must exclude the stand-down (want 4.0), got %v", got)
	}
	// Every other dimension still counts all three sessions: correct_diagnosis (5+3+4)/3 = 4.0.
	if got := sc.DimMeans["correct_diagnosis"]; got != 4.0 {
		t.Fatalf("correct_diagnosis must count all sessions (want 4.0), got %v", got)
	}
	// If EVERY session were a stand-down, falsifiable_prediction has zero scoreable samples ⇒ it is
	// FLOORED at AbstentionFloor with DimSamples 0 (v2), never silently dropped from the denominator —
	// dropping it is exactly how the 2026-07-25 collapse read as an improvement.
	only := Aggregate([]Session{{Ref: "s", Proposed: false}}, []Score{{Ref: "s", Scores: map[string]int{"falsifiable_prediction": 1}}})
	if got := only.DimMeans["falsifiable_prediction"]; got != AbstentionFloor {
		t.Fatalf("with no applicable session, falsifiable_prediction must be floored at %v, got %v", AbstentionFloor, got)
	}
	if only.DimSamples["falsifiable_prediction"] != 0 {
		t.Fatalf("a floored dimension must publish DimSamples=0, got %v", only.DimSamples)
	}
}

// The regression that motivated fixed-denominator-v2, pinned exactly: two runs with IDENTICAL judged
// quality on every measured dimension — one proposes (its prediction quality is measured), one produces
// nothing (the dimension has no samples). Under the pre-v2 formula the abstainer scored HIGHER (the worst
// dimension left its denominator); under v2 abstention scores strictly lower.
func TestAggregateFixedDenominatorAbstentionIsFailure(t *testing.T) {
	producing := Aggregate(
		[]Session{{Ref: "a", Proposed: true, Predicted: true}},
		[]Score{{Ref: "a", Scores: map[string]int{
			"correct_diagnosis": 4, "evidence_grounded": 4, "sensible_proposal": 4, "appropriate_band": 4,
			"falsifiable_prediction": 2,
		}}},
	)
	abstaining := Aggregate(
		[]Session{{Ref: "b", Proposed: false, Predicted: false}},
		[]Score{{Ref: "b", Scores: map[string]int{
			"correct_diagnosis": 4, "evidence_grounded": 4, "sensible_proposal": 4, "appropriate_band": 4,
		}}},
	)
	// producing: (4+4+4+4+2)/5 = 3.6 · abstaining: (4+4+4+4+1)/5 = 3.4 — lower, as it must be.
	if producing.Overall != 3.6 || abstaining.Overall != 3.4 {
		t.Fatalf("want producing=3.6 abstaining=3.4, got %v / %v", producing.Overall, abstaining.Overall)
	}
	if abstaining.Overall >= producing.Overall {
		t.Fatalf("abstention must score strictly lower than producing the same measured quality")
	}
}

// Label-aware rates: proposal_recall counts proposals only on expected-propose incidents; a raw
// proposal_rate cannot distinguish a correctly-stood-down stale corpus from a collapse.
func TestAggregateLabeledRecall(t *testing.T) {
	sessions := []Session{
		{Ref: "p1", Expected: "propose", Proposed: true},
		{Ref: "p2", Expected: "propose", Proposed: true},
		{Ref: "p3", Expected: "propose", Proposed: false}, // a miss
		{Ref: "s1", Expected: "stand-down", Proposed: false},
		{Ref: "s2", Expected: "stand-down", Proposed: false},
		{Ref: "e1", Expected: "escalate", Proposed: false},
		{Ref: "u1", Proposed: false}, // unlabeled — enters neither metric
	}
	sc := Aggregate(sessions, nil)
	if sc.ExpectedProposeN != 3 {
		t.Fatalf("expected_propose_n=%d (want 3)", sc.ExpectedProposeN)
	}
	if sc.ProposalRecall != 0.67 {
		t.Fatalf("proposal_recall=%v (want 0.67 = 2/3)", sc.ProposalRecall)
	}
	// Labeled stand-downs: p3 (label propose — WRONG to stand down), s1, s2, e1 = 4; correct = 3.
	if sc.LabeledStanddownN != 4 || sc.StanddownPrecision != 0.75 {
		t.Fatalf("standdown n=%d precision=%v (want 4 / 0.75)", sc.LabeledStanddownN, sc.StanddownPrecision)
	}
	// An unlabeled corpus publishes zero denominators — the gate must key its floors on these, so they
	// must be exactly zero, not defaulted.
	bare := Aggregate([]Session{{Ref: "x", Proposed: true}}, nil)
	if bare.ExpectedProposeN != 0 || bare.LabeledStanddownN != 0 {
		t.Fatalf("unlabeled corpus must publish zero denominators, got %d/%d", bare.ExpectedProposeN, bare.LabeledStanddownN)
	}
}

func TestLoadCorpus(t *testing.T) {
	c, err := LoadCorpus("corpus.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c) < 15 {
		t.Fatalf("want >=15 corpus incidents, got %d", len(c))
	}
	for _, x := range c {
		if x.ExternalRef == "" || x.AlertRule == "" || x.Severity == "" {
			t.Fatalf("incomplete corpus entry: %+v", x)
		}
	}
}

// The gate package keeps literal copies of AbstentionFloor / OverallFormulaV2 (to stay dependency-light);
// this pins the two pairs equal so they can never drift apart silently.
func TestGateConstantsMatch(t *testing.T) {
	if AbstentionFloor != gate.AbstentionFloor || OverallFormulaV2 != gate.OverallFormulaV2 {
		t.Fatalf("eval and eval/gate constants drifted: %v/%v vs %v/%v",
			AbstentionFloor, OverallFormulaV2, gate.AbstentionFloor, gate.OverallFormulaV2)
	}
}

// Pins the corpus's label composition so a regeneration cannot silently strip or skew the labels (the
// recall floor auto-disarms at ExpectedProposeN==0 by design — this test is what makes that safe), and
// pins the TG_EVAL_LIMIT=8 fast-gate prefix carrying enough expected-propose incidents to measure.
func TestCorpusLabelComposition(t *testing.T) {
	c, err := LoadCorpus("corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	prefixPropose := 0
	for i, inc := range c {
		if inc.Expected == "" {
			t.Fatalf("%s is unlabeled — every corpus incident must carry an expected label", inc.ExternalRef)
		}
		counts[inc.Expected]++
		if i < 8 && inc.Expected == "propose" {
			prefixPropose++
		}
	}
	if counts["propose"] != 8 || counts["stand-down"] != 12 || counts["escalate"] != 16 {
		t.Fatalf("label composition drifted: %v (want propose=8 stand-down=12 escalate=16 — the TG-78 storage-appliance row joined (+1) and tg78-pve-guest-02 left (−1, REMOVED-AS-UNEVALUABLE: Azure's prompt-shield flags its composed prompt on both deployments as of 08-30 — TG-556; restore with the shield drift or a funded rescue rung). The storage row (sensor-health on dc1syno01: no storage op-class exists, the named-member escalation IS the competence; the capacity row and a syno02 appliance-down row were deliberately NOT added — one is trajectory-dependent so no fixed label is honest, the other is live-unroutable, and grading either would grade a divergence); stand-down 9→12 the linux trio; a deliberate relabel updates this pin)", counts)
	}
	if prefixPropose < 2 {
		t.Fatalf("the TG_EVAL_LIMIT=8 prefix has %d expected-propose incidents (want >=2) — the fast gate would under-measure recall", prefixPropose)
	}
}

// The label validator fails closed on unknown labels and orphan op-class declarations.
func TestLoadCorpusRejectsBadLabels(t *testing.T) {
	dir := t.TempDir()
	bad := dir + "/bad.json"
	os.WriteFile(bad, []byte(`[{"external_ref":"x","expected":"Propose"}]`), 0o644)
	if _, err := LoadCorpus(bad); err == nil {
		t.Fatal("a typo'd label must fail closed, not silently disarm the recall floor")
	}
	os.WriteFile(bad, []byte(`[{"external_ref":"x","expected":"stand-down","expected_op_class":"start-guest"}]`), 0o644)
	if _, err := LoadCorpus(bad); err == nil {
		t.Fatal("expected_op_class without expected=propose must fail closed")
	}
}

// A stale-vs-live labeled session leaves BOTH label denominators (counted in StaleExcluded, never silent):
// scoring corpus drift as an agent miss is how a stale corpus once read as a capability collapse.
func TestAggregateStaleExclusion(t *testing.T) {
	sc := Aggregate([]Session{
		{Ref: "p1", Expected: "propose", Proposed: true},
		{Ref: "p2", Expected: "propose", Proposed: false, StaleVsLive: true}, // stood down on a stale incident — correct, excluded
		{Ref: "s1", Expected: "stand-down", Proposed: false},
	}, nil)
	if sc.ExpectedProposeN != 1 || sc.ProposalRecall != 1.0 {
		t.Fatalf("stale expected-propose must leave the recall denominator: n=%d recall=%v", sc.ExpectedProposeN, sc.ProposalRecall)
	}
	if sc.StaleExcluded != 1 {
		t.Fatalf("the exclusion must be published, got %d", sc.StaleExcluded)
	}
	if sc.LabeledStanddownN != 1 || sc.StanddownPrecision != 1.0 {
		t.Fatalf("stale sessions must leave the precision denominator too: n=%d p=%v", sc.LabeledStanddownN, sc.StanddownPrecision)
	}
}
