package calibrate

import (
	"testing"

	core "github.com/territory-grounder/grounder/core/calibrate"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/observe"
)

// ★ THE SECOND REFERENCE CLASS (TG-335).
//
// metrics declares a CLOSED set of outcome variables and ClampCalibrationOutcome accepts two of them, but
// the emitter hardcoded blast_radius_exact — so OutcomeDiagnosisCorrect was a constant with no producer and
// the `outcome` label carried exactly one value. The confidence alerts instruct an operator to compare the
// two classes before concluding the agent is overconfident; that comparison could not be made from the
// running system.
// The Emitter interface is wide and this exercises exactly one method, so the interface is EMBEDDED (nil)
// and only Calibration is overridden. Any other method would panic loudly rather than silently no-op — which
// is what you want from a fake: if EmitFor ever starts emitting something else, the test says so.
type recordingEmitter struct {
	observe.Emitter
	got []observe.CalibrationReading
}

func (r *recordingEmitter) Calibration(c observe.CalibrationReading) { r.got = append(r.got, c) }

func TestEmitForCarriesTheOutcomeItWasGiven(t *testing.T) {
	for _, want := range []string{metrics.OutcomeBlastRadiusExact, metrics.OutcomeDiagnosisCorrect} {
		got := outcomeOf(t, want)
		if got != want {
			t.Errorf("EmitFor(%q) published outcome=%q — a curve labelled with the wrong reference class is "+
				"worse than an unlabelled one, because every score beside it silently changes meaning", want, got)
		}
	}
}

func TestEmitForClampsAnUnknownClass(t *testing.T) {
	if got := outcomeOf(t, "something-nobody-declared"); got != metrics.OutcomeOther {
		t.Errorf("an undeclared outcome published as %q, want %q. A label that can take any value is not a "+
			"reference class.", got, metrics.OutcomeOther)
	}
}

// EmitTo must keep meaning exactly what it meant, or every existing dashboard and alert silently repoints.
func TestEmitToStillMeansBlastRadiusExact(t *testing.T) {
	e := &recordingEmitter{}
	EmitTo(e)(core.Reliability{N: 3})
	if len(e.got) != 1 || e.got[0].Outcome != metrics.OutcomeBlastRadiusExact {
		t.Fatalf("EmitTo published %+v, want a single reading with outcome=%q", e.got, metrics.OutcomeBlastRadiusExact)
	}
}

func outcomeOf(t *testing.T, class string) string {
	t.Helper()
	e := &recordingEmitter{}
	EmitFor(class, e)(core.Reliability{N: 5, Brier: 0.1})
	if len(e.got) != 1 {
		t.Fatalf("expected exactly one reading, got %d", len(e.got))
	}
	return e.got[0].Outcome
}
