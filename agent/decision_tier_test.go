package agent

import (
	"context"
	"testing"
)

// TG-198 — WHICH MODEL DECIDED, not which one read.
//
// The loop investigates on ModelName ("fast") and, when the TG-60 poll-limit nudge fires, runs the ONE
// forced-decision cycle on DecisionModelName ("primary"). Result carried only the investigation tier, so the
// runner stamped session_triage.model_tier = "fast" for every session — including the ones where the
// reasoning tier authored the proposal. Across the 537-incident corpus that makes "did the expensive tier
// decide better?" unanswerable: 100% of decisions are attributed to a model that made some of them.
//
// KILLING MUTATION (executed): restore the hardcode by making the loop stamp the investigation tier —
// in agent/loop.go replace `res.DecisionTier = modelName` with `res.DecisionTier = a.ModelName`. Both
// sub-tests then fail with:
//
//	the terminal proposal was produced on tier "primary" but the session recorded "fast" — the corpus
//	attributes this decision to the wrong model (TG-198)
//
// Restored ⇒ green.
func TestDecisionTierIsTheTierThatDecided(t *testing.T) {
	// A GROUNDED proposal — it cites tr-1, the id readTool captures, so the post-observation citation gate
	// passes and the loop reaches a real terminal proposal rather than bouncing.
	proposeGrounded := `{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-1","target":"web01","op_class":"restart-service","op":"restart","reversible":true,"evidence_ids":["tr-1"]}}`
	stopGrounded := `{"action":"stop","confidence":0.9,"reason":"logrotate reclaimed the disk","evidence_ids":["tr-1"]}`

	// Five distinct tool calls reach the poll limit (HandoffPoll=5) without tripping the repeated-step
	// trajectory veto; the sixth response is emitted on the NUDGED decision cycle, which runs on "primary".
	toPollLimit := []string{
		distinctToolCall("h1"), distinctToolCall("h2"), distinctToolCall("h3"), distinctToolCall("h4"), distinctToolCall("h5"),
	}

	for _, tc := range []struct {
		name     string
		terminal string
		outcome  Outcome
	}{
		{"proposal on the decision cycle", proposeGrounded, OutcomeProposed},
		{"grounded stop on the decision cycle", stopGrounded, OutcomeStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &modelRecorder{responses: append(append([]string{}, toPollLimit...), tc.terminal)}
			ts := NewReadOnlyToolSet()
			if err := ts.Register(readTool{}); err != nil {
				t.Fatal(err)
			}
			ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "fast", DecisionModelName: "primary", User: "t"}
			res, err := ag.Run(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != tc.outcome {
				t.Fatalf("setup: expected the nudged cycle to reach %s, got %s (%s) — the scenario no longer exercises a terminal decision",
					tc.outcome, res.Outcome, res.Reason)
			}
			// Anchor on what the model layer ACTUALLY saw, not on a constant: the tier of the LAST completion
			// is by definition the tier that produced the terminal directive. A vacuous run (no calls) fails
			// here rather than silently passing an assertion over an empty recorder.
			if len(m.models) == 0 {
				t.Fatal("the recorder captured no model calls — the assertion below would be vacuous")
			}
			deciding := m.models[len(m.models)-1]
			if deciding != "primary" {
				t.Fatalf("setup: the terminal cycle must run on the DecisionModelName tier, got %q (calls: %v)", deciding, m.models)
			}
			if res.DecisionTier != deciding {
				t.Fatalf("the terminal proposal was produced on tier %q but the session recorded %q — the corpus attributes this decision to the wrong model (TG-198)",
					deciding, res.DecisionTier)
			}
			// The investigation tier is a SEPARATE fact and must survive unchanged: a session that read on
			// "fast" and decided on "primary" is two data points, and collapsing them into one column is the
			// defect in the other direction.
			if ag.ModelName != "fast" {
				t.Fatalf("the investigation tier must remain %q, got %q", "fast", ag.ModelName)
			}
			if !res.DecideNudgeFired {
				t.Fatal("the TG-60 decide-nudge fired but the session did not record it — 'converged on its own' and 'was told to decide' become indistinguishable")
			}
		})
	}
}

// A session that decides WITHOUT ever being nudged records the tier it actually ran on, and records that no
// nudge fired. This is the control for the test above: it proves DecisionTier tracks the real terminal call
// rather than being a constant "primary" that happens to match the nudged case.
func TestDecisionTierWithoutNudgeIsTheInvestigationTier(t *testing.T) {
	proposeGrounded := `{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-1","target":"web01","op_class":"restart-service","op":"restart","reversible":true}}`
	m := &modelRecorder{responses: []string{proposeGrounded}}
	ts := NewReadOnlyToolSet()
	if err := ts.Register(readTool{}); err != nil {
		t.Fatal(err)
	}
	ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "fast", DecisionModelName: "primary", User: "t"}
	res, err := ag.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeProposed {
		t.Fatalf("setup: expected an immediate proposal, got %s (%s)", res.Outcome, res.Reason)
	}
	if res.DecisionTier != "fast" {
		t.Fatalf("an un-nudged session decided on the investigation tier %q but recorded %q", "fast", res.DecisionTier)
	}
	if res.DecideNudgeFired {
		t.Fatal("no nudge fired, but the session recorded one — a forced decision and a self-converged one must stay distinguishable")
	}
}
