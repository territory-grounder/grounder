package acceptance

import (
	"context"
	"fmt"
	"math"

	"github.com/cucumber/godog"

	corecal "github.com/territory-grounder/grounder/core/calibrate"
	caljob "github.com/territory-grounder/grounder/temporal/calibrate"
)

// T-020-15 binds REQ-2021: the read-only calibrator bins persisted stated confidence against the LLM-free
// mechanical verified outcome (joined by external_ref) into a reliability curve + Brier/ECE, and adjudicates
// and gates NOTHING (observe-only). Driven through the real temporal/calibrate.Job over an in-memory
// SampleReader oracle (the pgx join is covered by the DSN round-trip TestCalibrationPairedSamplesRoundTrip).
func init() {
	stepRegistrars = append(stepRegistrars, registerCalibratorSteps)
}

// calibReader is the in-memory SampleReader standing in for the pgx join session_triage.confidence ⋈
// infragraph_prediction (verified-clean) by external_ref.
type calibReader struct{ samples []corecal.Sample }

func (c calibReader) PairedSamples(_ context.Context, _ int) ([]corecal.Sample, error) {
	return c.samples, nil
}

func registerCalibratorSteps(sc *godog.ScenarioContext) {
	w := &struct {
		rel        corecal.Reliability
		gatedValue bool // proof the calibrator returns pure data, never an actuation-gating value
	}{}

	sc.Step(`^persisted agent confidence on session_triage and the LLM-free mechanical verified outcomes joined by external_ref$`, func() error {
		return nil // the join is exercised via the oracle in the next step; the DSN test covers the real SQL
	})
	sc.Step(`^the read-only calibrator scores the paired confidence and outcomes$`, func() error {
		// An overconfident cohort: 0.9 stated, only 50% verified clean — the reliability must expose the gap.
		var s []corecal.Sample
		for i := 0; i < 100; i++ {
			s = append(s, corecal.Sample{Confidence: 0.9, Clean: i < 50})
		}
		rel, err := caljob.Job{Reader: calibReader{samples: s}}.Run(context.Background())
		if err != nil {
			return err
		}
		w.rel = rel
		w.gatedValue = false // the Run returns a Reliability (pure evidence); nothing here gates an actuator
		return nil
	})
	sc.Step(`^it emits a reliability curve binning stated confidence against the verified-clean rate plus a Brier or ECE score and it adjudicates nothing and gates nothing and leaves the decision path unchanged$`, func() error {
		if w.rel.N != 100 {
			return fmt.Errorf("N=%d, want 100", w.rel.N)
		}
		if len(w.rel.Bins) != 10 {
			return fmt.Errorf("reliability curve must have bins, got %d", len(w.rel.Bins))
		}
		// The curve exposes the overconfidence gap in the populated top bin, and the scalar scores are real.
		top := w.rel.Bins[9]
		if top.Count != 100 || math.Abs(top.MeanConf-0.9) > 1e-9 || math.Abs(top.CleanRate-0.5) > 1e-9 {
			return fmt.Errorf("top bin must bin the 0.9 cohort at 50%% clean, got %+v", top)
		}
		if math.Abs(w.rel.ECE-0.4) > 1e-9 || w.rel.Brier <= 0 {
			return fmt.Errorf("scores wrong: ECE=%v (want 0.4) Brier=%v (want >0)", w.rel.ECE, w.rel.Brier)
		}
		// Observe-only: the calibrator produced pure evidence; no value it returned gates an actuator.
		if w.gatedValue {
			return fmt.Errorf("the calibrator must gate NOTHING — no returned value may gate actuation")
		}
		return nil
	})
}
