package acceptance

import (
	"fmt"
	"testing"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/safety"
)

// world is the per-scenario state for the spec/002 GREEN oracles.
type world struct {
	action    manifest.Action
	actionID  string
	changedID string
	m         *manifest.ActionManifest

	// ParseProposal (REQ-102 one-grammar) oracle state.
	parseInputs   []string
	parseAccepted int
	parseRejected int
}

const validToolCall = `{"external_ref":"TG-1","target":"web01","op_class":"restart-service","op":"restart","params":{"unit":"nginx"},"reversible":true,"approval_choice":"AUTO-RESOLVE","confidence":0.8}`

func canonicalAction() manifest.Action {
	return manifest.Action{
		Target:     "web01",
		OpClass:    "restart-service",
		Op:         "restart",
		Params:     map[string]string{"unit": "nginx"},
		Reversible: true,
	}
}

// TestPredictionGateAcceptance runs the spec/002 acceptance feature. @pending scenarios (the
// not-yet-built prediction gate / verifier) are excluded; the executed set drives the real
// core/manifest ActionManifest binding (REQ-102b) and must pass strictly.
func TestPredictionGateAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/002 prediction-gate",
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
		t.Fatal("spec/002 acceptance scenarios failed")
	}
}

func initializeScenario(sc *godog.ScenarioContext) {
	w := &world{}

	// REQ-101/102/103/104/105/102b — the fail-closed prediction gate + deterministic verifier
	// (core/predict, core/verify), bound in a companion file.
	registerPredictSteps(sc)

	sc.Step(`^a canonical action$`, func() error {
		w.action = canonicalAction()
		id, err := w.action.ID()
		w.actionID = id
		return err
	})
	sc.Step(`^a field of the action is changed$`, func() error {
		changed := w.action
		changed.Target = "web02"
		id, err := changed.ID()
		w.changedID = id
		return err
	})
	sc.Step(`^the action_id of the changed action differs from the original$`, func() error {
		if w.actionID == "" || w.changedID == "" {
			return fmt.Errorf("action ids not computed: original=%q changed=%q", w.actionID, w.changedID)
		}
		if w.actionID == w.changedID {
			return fmt.Errorf("action_id did not change on a field change (%s)", w.actionID)
		}
		return nil
	})

	sc.Step(`^a sealed ActionManifest built from a canonical action$`, func() error {
		m, err := manifest.New(canonicalAction(), safety.BandPollPause, "plan#1", "pred#1")
		w.m = m
		return err
	})
	sc.Step(`^asserting the manifest against its own action_id passes$`, func() error {
		if err := w.m.Assert(w.m.ActionID); err != nil {
			return fmt.Errorf("assert against own id must pass, got %v", err)
		}
		return nil
	})
	sc.Step(`^asserting the manifest against a foreign action_id fails$`, func() error {
		if err := w.m.Assert("0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
			return fmt.Errorf("assert against a foreign action_id must fail, got nil")
		}
		return nil
	})

	// REQ-102 — the sole ParseProposal entry point (one grammar; no second grammar accepts). Closes the
	// predecessor H-02 bypass (a "Which plan? - Plan X:" grammar that ran after the gate).
	sc.Step(`^a battery of model responses that are markdown, a sentinel marker, an alternate grammar, or malformed JSON$`, func() error {
		w.parseInputs = []string{
			"[AUTO-RESOLVE] restart nginx",                   // sentinel marker in markdown
			"[POLL] awaiting approval",                       // second marker
			"Which plan? - Plan A: restart - Plan B: reboot", // the predecessor second grammar
			"Which approach do you prefer?",                  // alternate grammar
			"Plan A",                                         // bare choice
			`{"external_ref":"TG-1","target":"h","op_class":"c","op":"o","evil":"x"}`,             // unknown field
			validToolCall + "\n" + `{"external_ref":"TG-2","target":"h","op_class":"c","op":"o"}`, // trailing 2nd object
			`not json at all`,
			`{"target":"h","op_class":"c","op":"o"}`, // missing external_ref
		}
		return nil
	})
	sc.Step(`^ParseProposal parses each response$`, func() error {
		w.parseAccepted, w.parseRejected = 0, 0
		// the one valid grammar must be accepted
		if _, err := proposal.ParseProposal([]byte(validToolCall)); err == nil {
			w.parseAccepted++
		} else {
			return fmt.Errorf("the one valid tool-call must parse, got %v", err)
		}
		// every non-grammar input must fail closed
		for _, in := range w.parseInputs {
			if _, err := proposal.ParseProposal([]byte(in)); err == nil {
				return fmt.Errorf("a second/looser grammar was ACCEPTED: %q", in)
			}
			w.parseRejected++
		}
		return nil
	})
	sc.Step(`^only a schema-valid tool-call is accepted and every other response fails closed$`, func() error {
		if w.parseAccepted != 1 {
			return fmt.Errorf("expected exactly one accepted grammar, got %d", w.parseAccepted)
		}
		if w.parseRejected != len(w.parseInputs) {
			return fmt.Errorf("expected all %d non-grammar inputs rejected, got %d", len(w.parseInputs), w.parseRejected)
		}
		return nil
	})
}
