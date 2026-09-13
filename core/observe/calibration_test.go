package observe

import (
	"testing"

	"github.com/territory-grounder/grounder/core/metrics"
)

// THE CONFIDENCE CURVE HAD NOWHERE TO GO BUT A LOG LINE.
//
// The calibrator has been running every 15 minutes and computing a real answer — measured live when this
// shipped: N=64, Brier 0.4633, ECE 0.5114, MCE 0.9000. A Brier of 0.25 is what always guessing 0.5 scores, so
// the agent's stated confidence was WORSE THAN A COIN, and the only place that appeared was worker stdout.
// Nobody greps a worker log to find out whether a number means anything.

func sampleByName(t *testing.T, out []metrics.Sample, name string) (metrics.Sample, bool) {
	t.Helper()
	for _, s := range out {
		if s.Name == name {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

func TestCalibrationCurveReachesTheMetricsSurface(t *testing.T) {
	r := NewRegistry()
	RecordCalibration(r, CalibrationReading{N: 64, Brier: 0.4633, ECE: 0.5114, MCE: 0.9000, BaseRate: 0.2656, Skill: -1.3752, SkillDefined: true})
	out := r.Collect()

	for name, want := range map[string]float64{
		metrics.MetricConfidenceSamples: 64,
		metrics.MetricConfidenceBrier:   0.4633,
		metrics.MetricConfidenceECE:     0.5114,
		metrics.MetricConfidenceMCE:     0.9000,
	} {
		s, ok := sampleByName(t, out, name)
		if !ok {
			t.Errorf("%s is absent from /metrics — the calibration answer is invisible again", name)
			continue
		}
		if s.Value != want {
			t.Errorf("%s = %v, want %v", name, s.Value, want)
		}
		if s.Kind != metrics.Gauge {
			t.Errorf("%s must be a GAUGE — a reliability score is the state of a curve, and rendering it as a "+
				"counter would show a meaningless monotonic climb", name)
		}
	}
}

// THE LOAD-BEARING HONESTY PROPERTY. At N=0 the three scores are a flat zero, which on any dashboard is
// indistinguishable from PERFECT calibration. The denominator must be published and the scores withheld.
func TestAnEmptySampleSetPublishesTheDenominatorAndNoScores(t *testing.T) {
	r := NewRegistry()
	RecordCalibration(r, CalibrationReading{})
	out := r.Collect()

	n, ok := sampleByName(t, out, metrics.MetricConfidenceSamples)
	if !ok || n.Value != 0 {
		t.Fatal("the sample COUNT must be published even at zero — it is the only thing that distinguishes " +
			"'no evidence yet' from 'perfectly calibrated'")
	}
	for _, name := range []string{
		metrics.MetricConfidenceBrier, metrics.MetricConfidenceECE, metrics.MetricConfidenceMCE,
	} {
		if _, present := sampleByName(t, out, name); present {
			t.Errorf("%s was published over an EMPTY sample set — a zeroed score renders as flawless "+
				"calibration for a system that has not been measured at all", name)
		}
	}
}

// Before the calibrator has ever run, nothing is published — not even the zero. An unmeasured system must not
// appear on the dashboard at all, rather than appear as measured-and-empty.
func TestNothingIsPublishedBeforeTheFirstPass(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{
		metrics.MetricConfidenceSamples, metrics.MetricConfidenceBrier,
		metrics.MetricConfidenceECE, metrics.MetricConfidenceMCE,
	} {
		if _, present := sampleByName(t, r.Collect(), name); present {
			t.Errorf("%s appeared before any calibration pass ran", name)
		}
	}
}

// Each pass REPLACES the reading. Accumulating would be meaningless — the calibrator recomputes the whole
// curve every time, so an added-up Brier score is not a score of anything.
func TestEachPassReplacesTheReadingRatherThanAccumulating(t *testing.T) {
	r := NewRegistry()
	RecordCalibration(r, CalibrationReading{N: 10, Brier: 0.9, ECE: 0.9, MCE: 0.9})
	RecordCalibration(r, CalibrationReading{N: 64, Brier: 0.4633, ECE: 0.5114, MCE: 0.9})

	n, _ := sampleByName(t, r.Collect(), metrics.MetricConfidenceSamples)
	if n.Value != 64 {
		t.Fatalf("want the LATEST reading (64), got %v — the curve is being accumulated, not replaced", n.Value)
	}
	b, _ := sampleByName(t, r.Collect(), metrics.MetricConfidenceBrier)
	if b.Value != 0.4633 {
		t.Fatalf("want the latest Brier 0.4633, got %v", b.Value)
	}
}

// A nil Emitter stays a no-op, so the calibrator can forward unconditionally whether or not metrics are wired.
func TestNilEmitterIsStillSilent(t *testing.T) {
	RecordCalibration(nil, CalibrationReading{N: 64, Brier: 0.4, ECE: 0.5, MCE: 0.9})
	var r *Registry
	r.Calibration(CalibrationReading{N: 64, Brier: 0.4, ECE: 0.5, MCE: 0.9})
	if got := r.Collect(); got != nil {
		t.Fatalf("a nil Registry must collect nothing, got %v", got)
	}
}
