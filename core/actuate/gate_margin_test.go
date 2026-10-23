package actuate

import (
	"context"
	"math"
	"testing"

	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/trace"
)

// marginCaptureSink is a local observe-only GateVerdictSink that records every emitted row, so the oracle can
// read back the policy gate's margin without a database.
type marginCaptureSink struct{ rows []trace.GateVerdict }

func (c *marginCaptureSink) Emit(_ context.Context, gv trace.GateVerdict) error {
	c.rows = append(c.rows, gv)
	return nil
}

// TestPolicyGateRecordsMinConfidenceMargin is the TG-178 oracle: when the confidence gate is active, the policy
// gate's verdict row carries the signed min_confidence margin (Confidence - min_confidence), so a decision that
// auto-authorized within ε of the auto→approve clamp is a flaggable boundary case, and one that cleared the
// gate comfortably is not.
// Killing mutation: revert the policy emit to plain emitGate (drop the margin) → Margin is nil for the boundary
// case → RED.
func TestPolicyGateRecordsMinConfidenceMargin(t *testing.T) {
	const eps = 0.05
	cases := []struct {
		name       string
		conf       float64
		minConf    float64
		wantMargin float64
		within     bool
	}{
		{"auto-authorized by a hair (boundary case)", 0.59, 0.60, -0.01, true},
		{"cleared the confidence gate comfortably", 0.90, 0.60, 0.30, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			act := &fakeActuator{}
			sink := &marginCaptureSink{}
			dec := &fakeDecider{
				verdict: policy.VerdictAuto,
				audit:   policy.DecisionAudit{Refine: policy.RefineRecord{Confidence: tc.conf, MinConfidence: tc.minConf}},
			}
			i := wired(safety.NewActuatingChokepoint(), act).
				WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto }).
				WithGateVerdictSink(sink)
			out, err := i.Do(context.Background(), goodRequest(t))
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			if out.Refused {
				t.Fatalf("an auto verdict on a good request must not be refused: %+v", out)
			}
			var pol *trace.GateVerdict
			for idx := range sink.rows {
				if sink.rows[idx].Gate == "policy" {
					pol = &sink.rows[idx]
				}
			}
			if pol == nil {
				t.Fatal("no policy gate row was emitted")
			}
			if pol.Margin == nil {
				t.Fatal("the policy gate must record a min_confidence margin when the confidence gate is active (TG-178)")
			}
			if math.Abs(*pol.Margin-tc.wantMargin) > 1e-9 {
				t.Fatalf("policy margin = %v, want %v", *pol.Margin, tc.wantMargin)
			}
			if within := math.Abs(*pol.Margin) <= eps; within != tc.within {
				t.Fatalf("within-ε(%.2f) = %v, want %v (margin %v)", eps, within, tc.within, *pol.Margin)
			}
		})
	}
}

// TestPolicyGateNoMarginWhenConfidenceGateUnset: an UNSET confidence gate (min_confidence 0) has no numeric
// threshold, so the policy gate records a NIL margin — not 0.0, which would masquerade as an exactly-at-threshold
// boundary case and pollute the within-ε review set.
func TestPolicyGateNoMarginWhenConfidenceGateUnset(t *testing.T) {
	act := &fakeActuator{}
	sink := &marginCaptureSink{}
	dec := &fakeDecider{
		verdict: policy.VerdictAuto,
		audit:   policy.DecisionAudit{Refine: policy.RefineRecord{Confidence: 0.42, MinConfidence: 0}},
	}
	i := wired(safety.NewActuatingChokepoint(), act).
		WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto }).
		WithGateVerdictSink(sink)
	if _, err := i.Do(context.Background(), goodRequest(t)); err != nil {
		t.Fatalf("Do: %v", err)
	}
	for _, g := range sink.rows {
		if g.Gate == "policy" && g.Margin != nil {
			t.Fatalf("an unset confidence gate (min_confidence 0) must record a nil margin, got %v", *g.Margin)
		}
	}
}

// TG-178: the policy gate ALSO emits a "policy-band" row carrying the band-composition margin (verdictRank
// distance from the band floor). Killing mutation: drop the emitGateMargin("policy-band",…) call → no
// policy-band row → RED.
func TestPolicyBandGateRecordsMargin(t *testing.T) {
	act := &fakeActuator{}
	sink := &marginCaptureSink{}
	dec := &fakeDecider{
		verdict: policy.VerdictAuto,
		audit:   policy.DecisionAudit{Compose: policy.ComposeRecord{Composed: policy.VerdictAuto, BandMarginRank: 0}},
	}
	i := wired(safety.NewActuatingChokepoint(), act).
		WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto }).
		WithGateVerdictSink(sink)
	if _, err := i.Do(context.Background(), goodRequest(t)); err != nil {
		t.Fatalf("Do: %v", err)
	}
	var band *trace.GateVerdict
	for idx := range sink.rows {
		if sink.rows[idx].Gate == "policy-band" {
			band = &sink.rows[idx]
		}
	}
	if band == nil {
		t.Fatal("no policy-band gate row emitted")
	}
	if band.Margin == nil || *band.Margin != 0 {
		t.Fatalf("policy-band margin = %v, want 0 (policy verdict at the band floor)", band.Margin)
	}
}

// TG-178: the policy gate ALSO emits a "graduation" row carrying the graduation margin — how many
// verified-clean runs the op-class was from its next rung. Killing mutation: drop the
// emitGateMargin("graduation",…) call (or the audit.Graduation threading) → the row loses its margin → RED.
func TestGraduationGateRecordsMargin(t *testing.T) {
	act := &fakeActuator{}
	sink := &marginCaptureSink{}
	dec := &fakeDecider{
		verdict: policy.VerdictAuto,
		audit:   policy.DecisionAudit{Graduation: policy.GraduationRecord{Margin: -1, Present: true}},
	}
	i := wired(safety.NewActuatingChokepoint(), act).
		WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto }).
		WithGateVerdictSink(sink)
	if _, err := i.Do(context.Background(), goodRequest(t)); err != nil {
		t.Fatalf("Do: %v", err)
	}
	var grad *trace.GateVerdict
	for idx := range sink.rows {
		if sink.rows[idx].Gate == "graduation" {
			grad = &sink.rows[idx]
		}
	}
	if grad == nil {
		t.Fatal("no graduation gate row emitted")
	}
	if grad.Margin == nil || *grad.Margin != -1 {
		t.Fatalf("graduation margin = %v, want -1 (one verified-clean run short of the next rung)", grad.Margin)
	}
}

// A class with no rung left to earn (already graduated, or not auto-eligible) records the graduation row
// WITHOUT a margin — a nil margin, never a 0 that would masquerade as an at-threshold boundary case.
func TestGraduationGateEmitsNoMarginWhenNoRung(t *testing.T) {
	act := &fakeActuator{}
	sink := &marginCaptureSink{}
	dec := &fakeDecider{
		verdict: policy.VerdictAuto,
		audit:   policy.DecisionAudit{Graduation: policy.GraduationRecord{Present: false}},
	}
	i := wired(safety.NewActuatingChokepoint(), act).
		WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto }).
		WithGateVerdictSink(sink)
	if _, err := i.Do(context.Background(), goodRequest(t)); err != nil {
		t.Fatalf("Do: %v", err)
	}
	for idx := range sink.rows {
		if sink.rows[idx].Gate == "graduation" {
			if sink.rows[idx].Margin != nil {
				t.Fatalf("graduation margin = %v, want nil (no next rung to be short of)", *sink.rows[idx].Margin)
			}
			return
		}
	}
	t.Fatal("no graduation gate row emitted")
}
