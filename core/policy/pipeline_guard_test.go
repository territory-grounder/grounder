package policy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
)

// realEngineFactory builds a representative REAL, fully-composed engine (Rego deny-overrides base + a
// fixed-clock rate governor + a graduation ladder + an execution deny-floor) as a fresh instance per call, so
// the guard evaluates each mode from identical per-action state. It carries the verdict trinary as data rules.
func realEngineFactory(t *testing.T) DeciderFactory {
	t.Helper()
	return func(ctx context.Context) (ModeDecider, error) {
		auto, err := NewRule(Rule{ID: "auto", Match: Match{OpClass: "svc.restart"}, Verdict: VerdictAuto})
		if err != nil {
			return nil, err
		}
		deny, err := NewRule(Rule{ID: "deny", Match: Match{OpClass: "wipe.disk"}, Verdict: VerdictDeny})
		if err != nil {
			return nil, err
		}
		approve, err := NewRule(Rule{ID: "approve", Match: Match{OpClass: "cfg.change"}, Verdict: VerdictApprove, ApproveBy: []string{"group:sre-oncall"}})
		if err != nil {
			return nil, err
		}
		rs := RuleSet{Default: Params{MinConfidence: f64ptr(0.60), RateLimit: intptr(3)}, Rules: []Rule{auto, deny, approve}}
		eng, err := NewEngine(ctx, rs)
		if err != nil {
			return nil, err
		}
		fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		eng = eng.WithRateGovernor(NewRateGovernor(func() time.Time { return fixed }))
		return eng, nil
	}
}

func f64ptr(v float64) *float64 { return &v }
func intptr(v int) *int         { return &v }

// TestAssertModeInvariant_RealEngineHoldsAcrossModes is the POSITIVE control: a representative real EvalInput
// yields an identical verdict / composed band / approve_by across all four modes — the REQ-1501 invariant
// holds for the current composed Decide.
func TestAssertModeInvariant_RealEngineHoldsAcrossModes(t *testing.T) {
	factory := realEngineFactory(t)
	cases := []struct {
		name string
		in   EvalInput
	}{
		{"auto", EvalInput{OpClass: "svc.restart", Reversible: true, Confidence: 1.0, Band: safety.BandAuto}},
		{"deny", EvalInput{OpClass: "wipe.disk", Reversible: true, Confidence: 1.0, Band: safety.BandAuto}},
		{"approve", EvalInput{OpClass: "cfg.change", Reversible: true, Confidence: 1.0, Band: safety.BandAuto}},
		{"nomatch", EvalInput{OpClass: "unknown.op", Reversible: true, Confidence: 1.0, Band: safety.BandAuto}},
		{"poll-pause-band", EvalInput{OpClass: "svc.restart", Reversible: true, Confidence: 1.0, Band: safety.BandPollPause}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := AssertModeInvariant(context.Background(), factory, tc.in); err != nil {
				t.Fatalf("REQ-1501 invariant must hold for %q, but the guard reported a divergence: %v", tc.name, err)
			}
		})
	}
}

// TestAssertModeInvariant_CarriesActiveModeIntoDecision proves the ONE thing that legitimately differs across
// modes — PolicyDecision.Mode records the active mode — is exactly what the guard permits to differ (it
// compares verdict/band/approve_by only, never the mode field). Without this, the guard's "only the mode may
// differ" claim would be untested.
func TestAssertModeInvariant_CarriesActiveModeIntoDecision(t *testing.T) {
	factory := realEngineFactory(t)
	in := EvalInput{OpClass: "svc.restart", Reversible: true, Confidence: 1.0, Band: safety.BandAuto}
	for _, mode := range allModes {
		d, err := factory(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		probe := in
		probe.Mode = mode
		dec, err := d.Decide(context.Background(), probe)
		if err != nil {
			t.Fatal(err)
		}
		if dec.Mode() != mode {
			t.Fatalf("decision did not record the active mode: got %v, want %v", dec.Mode(), mode)
		}
	}
	// And the guard passes despite PolicyDecision.Mode differing per mode.
	if err := AssertModeInvariant(context.Background(), factory, in); err != nil {
		t.Fatalf("the guard must tolerate the recorded-mode field differing: %v", err)
	}
}

// modeDependentDecider is a DELIBERATELY BROKEN decider: its verdict depends on the active mode (auto in an
// actuating mode, approve otherwise). It is the negative control — a real REQ-1501 violation the guard MUST
// catch. It exists ONLY in this test.
type modeDependentDecider struct{}

func (modeDependentDecider) Decide(_ context.Context, in EvalInput) (PolicyDecision, error) {
	v := VerdictApprove
	if in.Mode.MayAutoActuate() { // BUG: the pipeline verdict is a function of the mode.
		v = VerdictAuto
	}
	return NewPolicyDecision(v, "buggy", in.Band, nil, in.Mode, "mode-dependent stub", DecisionAudit{}), nil
}

// TestAssertModeInvariant_DetectsModeDependentPipeline is the NEGATIVE control: the guard must DETECT a
// mode-dependent decider and return the typed ErrPipelineModeDependent naming the diverging verdict — proving
// the guard actually guards and is not a vacuous no-op.
func TestAssertModeInvariant_DetectsModeDependentPipeline(t *testing.T) {
	factory := func(context.Context) (ModeDecider, error) { return modeDependentDecider{}, nil }
	err := AssertModeInvariant(context.Background(), factory,
		EvalInput{OpClass: "svc.restart", Reversible: true, Confidence: 1.0, Band: safety.BandAuto})
	if err == nil {
		t.Fatal("the guard is a NO-OP: it did not detect a deliberately mode-dependent pipeline (REQ-1501)")
	}
	if !errors.Is(err, ErrPipelineModeDependent) {
		t.Fatalf("divergence error must be ErrPipelineModeDependent, got: %v", err)
	}
	var div *ModeDivergence
	if !errors.As(err, &div) {
		t.Fatalf("divergence error must carry a *ModeDivergence, got: %v", err)
	}
	if div.Field != "verdict" {
		t.Fatalf("expected the verdict field to diverge, got %q", div.Field)
	}
}

// TestAssertModeInvariant_NilFactory fails closed on a nil factory rather than vacuously passing.
func TestAssertModeInvariant_NilFactory(t *testing.T) {
	if err := AssertModeInvariant(context.Background(), nil, EvalInput{}); err == nil {
		t.Fatal("a nil factory must fail closed, not pass")
	}
}

// TestSelfTest_PassesOnRealEngine asserts the boot self-test passes on the real composed Engine.Decide — the
// hook the boot preflight calls to refuse a mode-dependent worker.
func TestSelfTest_PassesOnRealEngine(t *testing.T) {
	if err := SelfTest(context.Background()); err != nil {
		t.Fatalf("SelfTest must pass on the real engine (REQ-1501 holds): %v", err)
	}
}
