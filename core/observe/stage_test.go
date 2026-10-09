package observe

import (
	"testing"

	"github.com/territory-grounder/grounder/core/metrics"
)

// TG-380: the decision-stage tally. offered ≥ eligible ≥ acted by construction; a nil tally is a no-op; the
// samples render all three families per stage (denominator beside numerator) and nothing for an idle stage.

func TestStageTallySubsetInvariant(t *testing.T) {
	tl := NewStageTally()
	// three offered: one acted (implies eligible), one eligible-not-acted, one force-escalated (neither).
	tl.Record("suppress", true, true)   // acted
	tl.Record("suppress", true, false)  // eligible, not acted
	tl.Record("suppress", false, false) // severity-escalated: not eligible
	off, elig, acted := tl.Snapshot("suppress")
	if off != 3 || elig != 2 || acted != 1 {
		t.Fatalf("offered/eligible/acted = %d/%d/%d, want 3/2/1", off, elig, acted)
	}
	if !(off >= elig && elig >= acted) {
		t.Fatalf("subset invariant offered>=eligible>=acted violated: %d/%d/%d", off, elig, acted)
	}
}

// TestActedPromotesEligible: a caller that records acted=true but eligible=false must NOT publish the
// impossible offered>=acted>eligible — Record promotes eligible. (Guards a wiring bug at the call site.)
func TestActedPromotesEligible(t *testing.T) {
	tl := NewStageTally()
	tl.Record("suppress", false, true) // acted but caller passed eligible=false
	off, elig, acted := tl.Snapshot("suppress")
	if off != 1 || elig != 1 || acted != 1 {
		t.Fatalf("acted must promote eligible: got %d/%d/%d, want 1/1/1", off, elig, acted)
	}
}

func TestNilStageTallyIsNoOp(t *testing.T) {
	var tl *StageTally
	tl.Record("suppress", true, true) // must not panic
	if s := tl.Samples(); s != nil {
		t.Fatalf("nil tally must render nothing, got %v", s)
	}
	if o, e, a := tl.Snapshot("suppress"); o != 0 || e != 0 || a != 0 {
		t.Fatalf("nil tally snapshot must be zero, got %d/%d/%d", o, e, a)
	}
}

// TestSamplesRenderAllThreePerStage: a stage that recorded traffic emits offered AND eligible AND acted —
// never acted alone (the denominator discipline). An idle stage emits nothing.
func TestSamplesRenderAllThreePerStage(t *testing.T) {
	tl := NewStageTally()
	if len(tl.Samples()) != 0 {
		t.Fatal("an idle tally must emit nothing")
	}
	tl.Record("suppress", true, false)
	names := map[string]bool{}
	for _, s := range tl.Samples() {
		names[s.Name] = true
	}
	for _, want := range []string{metrics.MetricStageOffered, metrics.MetricStageEligible, metrics.MetricStageActed} {
		if !names[want] {
			t.Fatalf("a stage with traffic must emit %s; got %v", want, names)
		}
	}
}
