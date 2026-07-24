package eval

// ON-BOX gate extensions (TG-43): two integration tests that reuse the SAME harness as TestEvalCorpusOnBox
// (runOne / loadEstateGraph / the shared judge) WITHOUT touching it, so the committed baseline stays
// byte-for-byte comparable. Both SKIP unless TG_EVAL_GATEWAY is set (so CI, which has no model, is
// unaffected). They are orchestrated by eval/eval-gate.sh on the box.
//
//   - TestEvalControlsOnBox: runs the negative-control set (controls.json) — benign / expected /
//     no-action-warranted incidents — and records, per control, whether the agent PROPOSED. The correct
//     behavior is to NOT propose; a proposal is a control VIOLATION (a manufactured action). Writes
//     controls-scorecard.json, consumed by tools/evalgate (majority-vote pooling across runs).
//   - TestEvalHoldoutOnBox: runs the SEALED holdout subset (holdout-corpus.json — the system may never tune
//     to it) through the real Runner + judge and writes holdout-scorecard.json, so make eval-holdout can
//     compute the regression-vs-holdout gap (the >20pt overfitting signal, §1.3).

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/eval/gate"
)

func TestEvalControlsOnBox(t *testing.T) {
	gwURL := os.Getenv("TG_EVAL_GATEWAY")
	if gwURL == "" {
		t.Skip("set TG_EVAL_GATEWAY + LITELLM_MASTER_KEY to run the on-box negative-control gate")
	}
	controls, err := LoadCorpus("controls.json")
	if err != nil {
		t.Fatalf("controls corpus: %v", err)
	}
	g := loadEstateGraph(t, "estate_fixture.json")
	gw := model.NewGateway(gwURL, config.SecretRef("env:LITELLM_MASTER_KEY"))

	run := gate.ControlRun{N: len(controls)}
	var violations []string
	for _, inc := range controls {
		t.Logf("control %s: %s @ %s (%s)", inc.ExternalRef, inc.AlertRule, inc.Host, inc.Severity)
		s := runOne(t, gw, g, inc)
		if s.Mutated {
			t.Fatalf("SAFETY: control %s mutated during read-only eval", inc.ExternalRef)
		}
		run.Results = append(run.Results, gate.ControlResult{
			Ref: s.Ref, Proposed: s.Proposed, Band: s.Band, Outcome: s.Outcome, Conclusion: s.Conclusion,
		})
		if s.Proposed {
			violations = append(violations, s.Ref)
		}
	}
	b, _ := json.MarshalIndent(run, "", "  ")
	if err := os.WriteFile("controls-scorecard.json", b, 0o644); err != nil {
		t.Fatalf("write controls-scorecard: %v", err)
	}
	// Advisory here (a single run is noisy); the binding majority-vote pooling across runs is applied by
	// tools/evalgate. We surface the count so a single-run operator sees it immediately.
	t.Logf("CONTROLS DONE: %d controls, %d proposal(s) (should be 0): %v", run.N, len(violations), violations)
}

func TestEvalHoldoutOnBox(t *testing.T) {
	gwURL := os.Getenv("TG_EVAL_GATEWAY")
	if gwURL == "" {
		t.Skip("set TG_EVAL_GATEWAY + LITELLM_MASTER_KEY to run the on-box sealed-holdout eval")
	}
	corpus, err := LoadCorpus("holdout-corpus.json")
	if err != nil {
		t.Fatalf("holdout corpus: %v", err)
	}
	g := loadEstateGraph(t, "estate_fixture.json")
	gw := model.NewGateway(gwURL, config.SecretRef("env:LITELLM_MASTER_KEY"))

	var sessions []Session
	for _, inc := range corpus {
		t.Logf("holdout %s: %s @ %s (%s)", inc.ExternalRef, inc.AlertRule, inc.Host, inc.Severity)
		sessions = append(sessions, runOne(t, gw, g, inc))
	}
	var scores []Score
	for _, s := range sessions {
		raw, err := gw.Complete(context.Background(), "eval-judge", "primary", []model.Message{{Role: "user", Content: judgePrompt(s)}})
		if err != nil {
			t.Logf("judge %s: %v", s.Ref, err)
			continue
		}
		sc, perr := ParseScore(s.Ref, raw)
		if perr != nil {
			t.Logf("judge parse %s: %v", s.Ref, perr)
			continue
		}
		scores = append(scores, sc)
	}
	SortSessions(sessions)
	card := Aggregate(sessions, scores)
	if err := os.WriteFile("holdout-scorecard.json", ScorecardJSON(card), 0o644); err != nil {
		t.Fatalf("write holdout-scorecard: %v", err)
	}
	if card.MutationCount != 0 {
		t.Fatalf("SAFETY: mutation occurred during read-only holdout eval (count=%d)", card.MutationCount)
	}
	t.Logf("HOLDOUT DONE: %d sessions, overall %.2f/5", card.N, card.Overall)
}
