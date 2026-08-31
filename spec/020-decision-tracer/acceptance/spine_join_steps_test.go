package acceptance

import (
	"fmt"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/trace"
)

// T-020-10 binds REQ-2019: the ingest → classify → propose → predict → verify walk joins end-to-end by
// external_ref with NO bridge hop, and the walk stays COMPLETE for a session that sealed no action (a grounded
// stop) rather than fabricating a predict/verify tail. The external_ref join reaching onto the actuation-side
// rows (the prediction row carries external_ref, migration 0026 — the bridge-hop elimination) is proven
// end-to-end for a sealed session by the DSN-gated core/db TestTraceSpineRoundTrip (which INSERTs across
// session_risk_audit/session_triage/infragraph_prediction/action_verdict and asserts the pgx SpineReader's
// external_ref join reaches every row). This pure-Go oracle binds the completeness half: it drives the REAL
// trace.Assemble for a sealed-action session (all four boundary rows present → a four-step executed walk) AND a
// no-action session (classify + a stop, no prediction/verdict rows → a two-step stopped walk with no fabricated
// tail), asserting each walk is ordered, bound to its OWN external_ref, and stops exactly where its durable rows
// stop. Assemble is pure (no I/O, no clock) so it sits off every chokepoint by construction.
func init() {
	stepRegistrars = append(stepRegistrars, registerSpineJoinSteps)
}

type spineJoinWorld struct {
	sealed   trace.SessionTrace
	noAction trace.SessionTrace
}

func registerSpineJoinSteps(sc *godog.ScenarioContext) {
	w := &spineJoinWorld{}

	sc.Step(`^a session that did and a session that did not seal an action with external_ref reached onto the actuation-side rows$`, func() error {
		// Sealed session: every boundary row present — classification, a PROPOSED triage, the committed
		// prediction (its external_ref reached it, migration 0026), and the mechanical verdict.
		sealed := trace.SpineRecords{
			Classification: trace.ClassificationRecord{Present: true, Band: "AUTO", RiskLevel: "low", ActionID: "act-sealed", PlanHash: "plan-sealed"},
			Triage:         trace.TriageRecord{Present: true, Host: "librespeed01", AlertRule: "ServiceDown", Band: "AUTO", Outcome: "proposal", Proposed: true, Op: "restart-service", Conclusion: "nginx failed", Confidence: 0.82},
			Prediction:     trace.PredictionRecord{Present: true, ActionID: "act-sealed", PlanHash: "plan-sealed"},
			Verdict:        trace.VerdictRecord{Present: true, Verdict: "match"},
		}
		// No-action session: classification + a STOP (Proposed=false), and NO actuation-side rows — the agent
		// grounded a stop, so nothing was sealed, predicted, or verified.
		noAction := trace.SpineRecords{
			Classification: trace.ClassificationRecord{Present: true, Band: "POLL_PAUSE", RiskLevel: "medium", ActionID: "act-noaction"},
			Triage:         trace.TriageRecord{Present: true, Host: "myspeed01", AlertRule: "ServiceDown", Band: "POLL_PAUSE", Outcome: "stop", Proposed: false, Conclusion: "device healthy — no action"},
		}
		w.sealed = trace.Assemble("ext-sealed", sealed)
		w.noAction = trace.Assemble("ext-noaction", noAction)
		return nil
	})

	sc.Step(`^the ingest to verify walk is assembled$`, func() error {
		if w.sealed.ExternalRef == "" || w.noAction.ExternalRef == "" {
			return fmt.Errorf("a walk was not assembled")
		}
		return nil
	})

	sc.Step(`^both walks join end to end by external_ref with no bridge hop and the walk remains complete for the session that sealed no action$`, func() error {
		// Sealed walk: bound to its own external_ref, ordered classify→propose→predict→verify, executed end-to-end.
		if w.sealed.ExternalRef != "ext-sealed" {
			return fmt.Errorf("sealed walk external_ref = %q, want ext-sealed — the walk is not bound to its session", w.sealed.ExternalRef)
		}
		if err := assertSpineKinds(w.sealed, []trace.StepKind{trace.StepClassify, trace.StepScreen, trace.StepPropose, trace.StepPredict, trace.StepVerify}); err != nil {
			return fmt.Errorf("sealed walk: %w", err)
		}
		if w.sealed.Status != trace.StatusExecuted {
			return fmt.Errorf("sealed walk status = %q, want executed", w.sealed.Status)
		}
		if w.sealed.Verdict != "match" {
			return fmt.Errorf("sealed walk verdict = %q, want match — the verify boundary did not join", w.sealed.Verdict)
		}

		// No-action walk: bound to ITS OWN external_ref (the two walks never cross-join), complete at the stop —
		// classify + propose(stop) and NO fabricated predict/verify tail.
		if w.noAction.ExternalRef != "ext-noaction" {
			return fmt.Errorf("no-action walk external_ref = %q, want ext-noaction", w.noAction.ExternalRef)
		}
		if err := assertSpineKinds(w.noAction, []trace.StepKind{trace.StepClassify, trace.StepScreen, trace.StepPropose}); err != nil {
			return fmt.Errorf("no-action walk: %w", err)
		}
		if w.noAction.Status != trace.StatusStopped {
			return fmt.Errorf("no-action walk status = %q, want stopped", w.noAction.Status)
		}
		for _, s := range w.noAction.Steps {
			if s.Kind == trace.StepPredict || s.Kind == trace.StepVerify {
				return fmt.Errorf("no-action walk fabricated a %q step — a session that sealed no action must carry no actuation-side tail", s.Kind)
			}
		}
		return nil
	})
}

// assertSpineKinds checks a walk's steps are exactly the wanted kinds, in order, with strictly increasing Seq from 0.
func assertSpineKinds(t trace.SessionTrace, want []trace.StepKind) error {
	if len(t.Steps) != len(want) {
		return fmt.Errorf("got %d steps, want %d", len(t.Steps), len(want))
	}
	for i, s := range t.Steps {
		if s.Kind != want[i] {
			return fmt.Errorf("step %d kind = %q, want %q", i, s.Kind, want[i])
		}
		if s.Seq != i {
			return fmt.Errorf("step %d seq = %d, want %d (ordered walk)", i, s.Seq, i)
		}
	}
	return nil
}
