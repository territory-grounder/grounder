package policy

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
)

// This file is the END-TO-END oracle for the composed Engine.Decide (spec/015 T-015-1, design.md's
// most-restrictive-first decision procedure): it drives the WHOLE chain — Rego deny-overrides base →
// EffectiveParams inheritance → confidence + rate clamps → band composition → graduation → execution
// deny-floor → approve_by — through the real engine, and asserts the required-field PolicyDecision. Each
// case fixes exactly one stage as the deciding one so the composition order is proven, not just the pieces.

// fixedGovernor is a rate governor on a frozen clock so the composition is deterministic (no rate clamp fires
// with a high limit; the window arithmetic never depends on wall time).
func fixedGovernor(t *testing.T) *RateGovernor {
	t.Helper()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return NewRateGovernor(func() time.Time { return at })
}

// autoRule builds a validated op-class `auto` rule with the given band_mode ("" = respect inherit).
func autoRule(t *testing.T, id, opClass string, band BandMode) Rule {
	t.Helper()
	r, err := NewRule(Rule{ID: id, Match: Match{OpClass: opClass}, Verdict: VerdictAuto, Params: Params{BandMode: band}})
	if err != nil {
		t.Fatalf("build rule %q: %v", id, err)
	}
	return r
}

// promotedLadder returns a ladder in which opClass has been promoted to LevelAuto (N verified-clean runs).
func promotedLadder(t *testing.T, opClass string) *Ladder {
	t.Helper()
	l := NewLadder(2, NewMemGraduationStore(), nil)
	for i := 0; i < 2; i++ {
		if _, err := l.Record(context.Background(), opClass, OutcomeFromVerdict(safety.VerdictMatch, true)); err != nil {
			t.Fatalf("promote: %v", err)
		}
	}
	if l.LevelOf(context.Background(), opClass) != LevelAuto {
		t.Fatalf("precondition: class %q did not promote to auto", opClass)
	}
	return l
}

// autoAction is a reversible, non-floor, high-confidence action whose risk band permits auto — the input that
// SURVIVES every stage to `auto` unless a specific stage tightens it.
func autoAction(opClass string) EvalInput {
	return EvalInput{OpClass: opClass, Reversible: true, Confidence: 0.95, Band: safety.BandAuto, Mode: ModeSemiAuto}
}

func mustDecide(t *testing.T, e *Engine, in EvalInput) PolicyDecision {
	t.Helper()
	d, err := e.Decide(context.Background(), in)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	return d
}

// TestCompose_AutoSurvivesFullChain: a rule=auto that clears confidence + band + graduation + floor resolves
// to `auto`, and the PolicyDecision carries every required field (ComposedBand + Mode populated).
func TestCompose_AutoSurvivesFullChain(t *testing.T) {
	const class = "service.restart"
	e := mustEngine(t, autoRule(t, "allow-restart", class, BandRespect)).
		WithRateGovernor(fixedGovernor(t)).
		WithGraduation(promotedLadder(t, class)).
		WithFloor(nil) // no execution-floor entry matches.

	d := mustDecide(t, e, autoAction(class))
	if d.Verdict() != VerdictAuto {
		t.Fatalf("full-chain verdict = %q, want auto; audit=%+v", d.Verdict(), d.Audit())
	}
	if d.ComposedBand() != safety.BandAuto {
		t.Fatalf("ComposedBand = %v, want the carried BandAuto", d.ComposedBand())
	}
	if d.Mode() != ModeSemiAuto {
		t.Fatalf("Mode = %v, want the carried Semi-auto", d.Mode())
	}
	if d.MatchedRuleID() != "allow-restart" {
		t.Fatalf("MatchedRuleID = %q, want allow-restart", d.MatchedRuleID())
	}
}

// TestCompose_LowConfidenceClampsToApprove: the SAME auto rule, but a below-min_confidence action is clamped
// auto→approve at step 3 (the default 0.60 gate resolved via EffectiveParams).
func TestCompose_LowConfidenceClampsToApprove(t *testing.T) {
	const class = "service.restart"
	e := mustEngine(t, autoRule(t, "allow-restart", class, BandRespect)).
		WithRateGovernor(fixedGovernor(t)).
		WithGraduation(promotedLadder(t, class))

	in := autoAction(class)
	in.Confidence = 0.30 // below the resolved default 0.60
	d := mustDecide(t, e, in)
	if d.Verdict() != VerdictApprove {
		t.Fatalf("low-confidence verdict = %q, want approve", d.Verdict())
	}
	if !d.Audit().Refine.ConfidenceClamped {
		t.Fatalf("confidence clamp not recorded: %+v", d.Audit().Refine)
	}
}

// TestCompose_UngraduatedClassClampsToApprove: the SAME auto rule that clears confidence + band, but the
// op-class has NOT graduated, so step 5 downgrades auto→approve.
func TestCompose_UngraduatedClassClampsToApprove(t *testing.T) {
	const class = "service.restart"
	e := mustEngine(t, autoRule(t, "allow-restart", class, BandRespect)).
		WithRateGovernor(fixedGovernor(t)).
		WithGraduation(NewLadder(5, NewMemGraduationStore(), nil)) // fresh — class at approve.

	d := mustDecide(t, e, autoAction(class))
	if d.Verdict() != VerdictApprove {
		t.Fatalf("ungraduated verdict = %q, want approve (not yet earned auto)", d.Verdict())
	}
}

// TestCompose_MatchingFloorEntryDenies: the SAME graduated auto, but an execution-floor entry matches, so
// step 6 floors the execution to deny.
func TestCompose_MatchingFloorEntryDenies(t *testing.T) {
	const class = "service.restart"
	floor, err := NewFloor(FloorEntry{ID: "floor-restart", Match: Match{OpClass: class}})
	if err != nil {
		t.Fatal(err)
	}
	e := mustEngine(t, autoRule(t, "allow-restart", class, BandRespect)).
		WithRateGovernor(fixedGovernor(t)).
		WithGraduation(promotedLadder(t, class)).
		WithFloor(floor)

	d := mustDecide(t, e, autoAction(class))
	if d.Verdict() != VerdictDeny {
		t.Fatalf("floored verdict = %q, want deny", d.Verdict())
	}
	if !d.Audit().Floor.Floored || d.Audit().Floor.MatchedEntryID != "floor-restart" {
		t.Fatalf("execution-floor record not captured: %+v", d.Audit().Floor)
	}
}

// TestCompose_DenyShortCircuits: a matching deny rule wins and SKIPS the later stages (no band/floor record).
func TestCompose_DenyShortCircuits(t *testing.T) {
	const class = "service.restart"
	deny, err := NewRule(Rule{ID: "deny-restart", Match: Match{OpClass: class}, Verdict: VerdictDeny})
	if err != nil {
		t.Fatal(err)
	}
	// Include a permissive auto rule + a floor that would otherwise engage — the deny must short-circuit past them.
	floor, err := NewFloor(FloorEntry{ID: "floor-restart", Match: Match{OpClass: class}})
	if err != nil {
		t.Fatal(err)
	}
	e := mustEngine(t, autoRule(t, "allow-restart", class, BandRespect), deny).
		WithRateGovernor(fixedGovernor(t)).
		WithGraduation(promotedLadder(t, class)).
		WithFloor(floor)

	d := mustDecide(t, e, autoAction(class))
	if d.Verdict() != VerdictDeny {
		t.Fatalf("deny short-circuit verdict = %q, want deny", d.Verdict())
	}
	if d.MatchedRuleID() != "deny-restart" {
		t.Fatalf("deny provenance = %q, want deny-restart", d.MatchedRuleID())
	}
	// Short-circuit: the later composition stages did NOT run, so their records stay zero.
	if d.Audit().Compose.Composed != "" || d.Audit().Floor.Floored {
		t.Fatalf("deny did not short-circuit: compose=%+v floor=%+v", d.Audit().Compose, d.Audit().Floor)
	}
}

// TestCompose_NeverAutoFloorClampsUnderForce: the constitutional never-auto floor clamps an `auto` even when
// the rule's band_mode is `force` — the inviolable INV-09 floor beneath the engine.
func TestCompose_NeverAutoFloorClampsUnderForce(t *testing.T) {
	// reboot is a mechanical never-auto floor class (core/safety).
	e := mustEngine(t, autoRule(t, "force-reboot", "reboot", BandForce)).
		WithRateGovernor(fixedGovernor(t)).
		WithGraduation(promotedLadder(t, "reboot"))

	in := autoAction("reboot") // reversible flag irrelevant — IsNeverAuto("reboot") already triggers the floor.
	d := mustDecide(t, e, in)
	if d.Verdict() == VerdictAuto {
		t.Fatalf("never-auto floor was bypassed under force: verdict=%q", d.Verdict())
	}
	if !d.Audit().Compose.FloorClamped {
		t.Fatalf("never-auto floor clamp not recorded under force: %+v", d.Audit().Compose)
	}
}

// TestCompose_ApproveCarriesApproveBy: a resolved `approve` carries the matched rule's approve_by principals
// and the action's composed band.
func TestCompose_ApproveCarriesApproveBy(t *testing.T) {
	r, err := NewRule(Rule{
		ID: "vote-restart", Match: Match{OpClass: "service.restart"},
		Verdict: VerdictApprove, ApproveBy: []string{"group:sre-oncall", "user:alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := mustEngine(t, r)
	in := autoAction("service.restart")
	in.Band = safety.BandPollPause
	d := mustDecide(t, e, in)
	if d.Verdict() != VerdictApprove {
		t.Fatalf("verdict = %q, want approve", d.Verdict())
	}
	got := d.ApproveBy()
	if len(got) != 2 || got[0] != "group:sre-oncall" || got[1] != "user:alice" {
		t.Fatalf("ApproveBy = %v, want the matched rule's approve_by", got)
	}
	if d.ComposedBand() != safety.BandPollPause {
		t.Fatalf("ComposedBand = %v, want POLL_PAUSE", d.ComposedBand())
	}
}

// TestCompose_NilPiecesPassThroughExceptFloor: with ALL optional pieces nil, an auto that clears confidence
// survives to auto — EXCEPT the never-auto floor, which still clamps a floor-class action to approve (it is a
// pure function of the input, not an injectable piece, so it can never be nil'd away).
func TestCompose_NilPiecesPassThroughExceptFloor(t *testing.T) {
	// (a) all pieces nil, a benign reversible high-confidence auto → survives to auto.
	e := mustEngine(t, autoRule(t, "allow-restart", "service.restart", BandRespect))
	if d := mustDecide(t, e, autoAction("service.restart")); d.Verdict() != VerdictAuto {
		t.Fatalf("nil-piece pass-through verdict = %q, want auto", d.Verdict())
	}

	// (b) all pieces nil, but a never-auto floor-class op → STILL clamped to approve (floor not bypassable).
	e2 := mustEngine(t, autoRule(t, "allow-reboot", "reboot", BandRespect))
	d := mustDecide(t, e2, autoAction("reboot"))
	if d.Verdict() == VerdictAuto {
		t.Fatalf("never-auto floor bypassed with nil pieces: verdict=%q", d.Verdict())
	}
	if !d.Audit().NeverAutoFloor {
		t.Fatalf("never-auto floor not flagged in the audit for a floor-class op: %+v", d.Audit())
	}
}

// TestCompose_NilEngineFailsClosed: a nil/uninitialised engine fails closed to approve, never auto.
func TestCompose_NilEngineFailsClosed(t *testing.T) {
	var e *Engine
	d, err := e.Decide(context.Background(), autoAction("service.restart"))
	if err != nil {
		t.Fatalf("nil engine returned an error: %v", err)
	}
	if d.Verdict() != VerdictApprove {
		t.Fatalf("nil engine verdict = %q, want the fail-closed approve", d.Verdict())
	}
}

// TestPolicyDecision_RequiredFieldConstructor documents + exercises the type-level required-field guarantee
// (REQ-1518): the ONLY way to build a PolicyDecision is NewPolicyDecision, whose signature names every field,
// so omitting one is a COMPILE error (the fields are unexported — a struct literal from outside the package
// cannot set them, and inside the package the literal would fail vet/review). This test constructs one the
// sanctioned way and reads every accessor.
func TestPolicyDecision_RequiredFieldConstructor(t *testing.T) {
	audit := DecisionAudit{Base: Decision{Verdict: VerdictApprove}, GraduatedVerdict: VerdictApprove}
	d := NewPolicyDecision(VerdictApprove, "rule-1", safety.BandPollPause, []string{"user:alice"}, ModeHITL, "reason", audit)
	if d.Verdict() != VerdictApprove || d.MatchedRuleID() != "rule-1" || d.ComposedBand() != safety.BandPollPause ||
		d.Mode() != ModeHITL || d.Reason() != "reason" || len(d.ApproveBy()) != 1 {
		t.Fatalf("constructor did not populate every required field: %+v", d)
	}
	// The returned slice is a defensive copy — mutating it does not mutate the decision.
	got := d.ApproveBy()
	got[0] = "user:mallory"
	if d.ApproveBy()[0] != "user:alice" {
		t.Fatal("ApproveBy is not a defensive copy")
	}
}
