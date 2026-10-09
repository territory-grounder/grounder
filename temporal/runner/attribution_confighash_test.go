package runner

// TG-466 slice 2: AttributeActivity's confighash wiring (Deps.GuestConfigChangedWithin). These tests call
// AttributeActivity directly (no Temporal test env needed — it is a plain method) and hold the reader
// evidence FIXED across every case (a single affirmative CoverageMarker, zero actor entries): the only
// variable under test is the confighash seam, so a taxonomy flip can only be attributed to it.
//
// Ship-dark contract under test (TG-466 guardrail): nil GuestConfigChangedWithin (the default — no
// TG_PVE_CONFIGHASH_ENABLED at the composition root) must reproduce PRE-TG-466 behavior EXACTLY —
// covered-but-empty stays observe-only, never security-escalate.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	"github.com/territory-grounder/grounder/core/attribution"
)

// coveredEmptyReader returns a single affirmative-coverage, no-actor marker for exactly one host — the
// REQ-2304-half-2 "covered but empty" shape confighash's signal is meant to discriminate.
func coveredEmptyReader(host string) actorevidence.Reader {
	return fakeActorReader{domain: "pve", ev: []attribution.Evidence{
		attribution.CoverageMarker("pve", host, time.Now()),
	}}
}

// (1) INERTNESS: nil GuestConfigChangedWithin (the ship-dark default) must leave the covered-but-empty
// finding exactly as it was before TG-466 — Unattributable + CoveredButEmpty, no escalation. This is the
// merge-time behavior: TG_PVE_CONFIGHASH_ENABLED unset wires nothing, so this is what production runs today.
func TestAttributeActivityConfighashNilSeamStaysInert(t *testing.T) {
	deps := Deps{}
	attributeDeps(t, coveredEmptyReader("web01"))(&deps)
	// deps.GuestConfigChangedWithin left at its zero value: nil.
	a := &Activities{D: deps}
	res, err := a.AttributeActivity(context.Background(), AttributeInput{Host: "web01", FaultClass: "start-service"})
	if err != nil {
		t.Fatalf("AttributeActivity must not error: %v", err)
	}
	if res.Security || res.Escalate {
		t.Fatalf("nil GuestConfigChangedWithin must NEVER escalate (ship-dark default): %+v", res)
	}
	if res.Finding.Taxonomy != attribution.Unattributable {
		t.Fatalf("taxonomy must stay unattributable (observe-only), got %v", res.Finding.Taxonomy)
	}
	if !res.Finding.CoveredButEmpty {
		t.Fatal("the covered-but-empty FACT must still be surfaced even though it does not escalate")
	}
}

// (2) ARMED + CHANGED: with the seam wired and reporting a change for the SAME subject the CoverageMarker
// covers, plus the other three AttributeObserving conditions (affirmative coverage, answered, zero actor
// entries), the covered-but-empty finding escalates to attributed-suspicious / security-escalate.
func TestAttributeActivityConfighashArmedChangeEscalates(t *testing.T) {
	deps := Deps{}
	attributeDeps(t, coveredEmptyReader("web01"))(&deps)
	deps.GuestConfigChangedWithin = func(ctx context.Context, guest string, window time.Duration) (bool, error) {
		return guest == "web01", nil
	}
	a := &Activities{D: deps}
	res, err := a.AttributeActivity(context.Background(), AttributeInput{Host: "web01", FaultClass: "start-service"})
	if err != nil {
		t.Fatalf("AttributeActivity must not error: %v", err)
	}
	if res.Finding.Taxonomy != attribution.AttributedSuspicious {
		t.Fatalf("a grounded change on the covered subject must escalate to attributed-suspicious, got %v (%+v)", res.Finding.Taxonomy, res)
	}
	if !res.Security {
		t.Fatalf("attributed-suspicious must map to the security-escalate disposition: %+v", res)
	}
}

// (3) ARMED + NO CHANGE: the seam is wired but confighash reports no change for the subject — the finding
// must stay exactly the observe-only shape of case (1). Armed-but-quiet must never escalate.
func TestAttributeActivityConfighashArmedNoChangeStaysObserveOnly(t *testing.T) {
	deps := Deps{}
	attributeDeps(t, coveredEmptyReader("web01"))(&deps)
	deps.GuestConfigChangedWithin = func(ctx context.Context, guest string, window time.Duration) (bool, error) {
		return false, nil
	}
	a := &Activities{D: deps}
	res, err := a.AttributeActivity(context.Background(), AttributeInput{Host: "web01", FaultClass: "start-service"})
	if err != nil {
		t.Fatalf("AttributeActivity must not error: %v", err)
	}
	if res.Security || res.Escalate {
		t.Fatalf("confighash reporting NO change must never escalate: %+v", res)
	}
	if res.Finding.Taxonomy != attribution.Unattributable || !res.Finding.CoveredButEmpty {
		t.Fatalf("expected observe-only covered-but-empty, got %+v", res.Finding)
	}
}

// (4) SUBJECT SCOPING: a confighash signal armed to fire ONLY for "web01" must escalate web01's session and
// must NOT escalate a co-occurring db02 session, even though db02 ALSO carries its own covered-but-empty
// evidence (so the non-escalation is provably due to per-subject scoping, not merely absent coverage).
func TestAttributeActivityConfighashSubjectScoped(t *testing.T) {
	fn := func(ctx context.Context, guest string, window time.Duration) (bool, error) {
		return guest == "web01", nil // armed for web01 ONLY
	}

	depsA := Deps{}
	attributeDeps(t, coveredEmptyReader("web01"))(&depsA)
	depsA.GuestConfigChangedWithin = fn
	resA, err := (&Activities{D: depsA}).AttributeActivity(context.Background(), AttributeInput{Host: "web01", FaultClass: "start-service"})
	if err != nil {
		t.Fatalf("AttributeActivity(web01) must not error: %v", err)
	}
	if !resA.Security || resA.Finding.Taxonomy != attribution.AttributedSuspicious {
		t.Fatalf("web01 (the guest confighash reports changed) must escalate: %+v", resA)
	}

	depsB := Deps{}
	attributeDeps(t, coveredEmptyReader("db02"))(&depsB)
	depsB.GuestConfigChangedWithin = fn
	resB, err := (&Activities{D: depsB}).AttributeActivity(context.Background(), AttributeInput{Host: "db02", FaultClass: "start-service"})
	if err != nil {
		t.Fatalf("AttributeActivity(db02) must not error: %v", err)
	}
	if resB.Security || resB.Escalate {
		t.Fatalf("db02 (confighash reports NO change for it) must NOT escalate — a change on web01 must not bleed across subjects: %+v", resB)
	}
	if !resB.Finding.CoveredButEmpty {
		t.Fatal("db02 must still be genuinely covered-but-empty (proving the non-escalation is SCOPING, not absent coverage)")
	}
}

// (5a) FAIL-SAFE ON ERROR: a store error must NEVER be read as a positive signal, even when the fake
// misbehaves and returns changed=true ALONGSIDE the error (mirrors TestEnrichSanctionedFailsOpenOnError's
// style — robust to a misbehaving seam, not just a well-behaved one that returns the zero value on error).
func TestAttributeActivityConfighashErrorFailsSafe(t *testing.T) {
	deps := Deps{}
	attributeDeps(t, coveredEmptyReader("web01"))(&deps)
	deps.GuestConfigChangedWithin = func(ctx context.Context, guest string, window time.Duration) (bool, error) {
		return true, errors.New("guest_config_baseline: connection reset")
	}
	a := &Activities{D: deps}
	res, err := a.AttributeActivity(context.Background(), AttributeInput{Host: "web01", FaultClass: "start-service"})
	if err != nil {
		t.Fatalf("a confighash read error is ADVISORY — it must never fail the activity: %v", err)
	}
	if res.Security || res.Escalate {
		t.Fatalf("a store ERROR must never mint a mutation signal, even when the fake returns changed=true alongside it: %+v", res)
	}
	if res.Finding.Taxonomy != attribution.Unattributable || !res.Finding.CoveredButEmpty {
		t.Fatalf("on a read error the finding must degrade to the SAME observe-only shape as the nil-seam case, got %+v", res.Finding)
	}
}

// (5b) FAIL-SAFE ON WINDOW: AttributeActivity must never pass a zero/non-positive window to the seam, even
// when the ruleset leaves AttributionConfig.Window unset — it floors to the compiled 30-minute ceiling
// BEFORE calling GuestConfigChangedWithin, preserving ChangedWithin's own zero-window fail-closed contract.
func TestAttributeActivityConfighashNeverPassesAZeroWindow(t *testing.T) {
	deps := Deps{}
	attributeDeps(t, coveredEmptyReader("web01"))(&deps)
	deps.AttributionConfig.Window = 0 // the ruleset-unset case
	var gotWindow time.Duration
	seen := false
	deps.GuestConfigChangedWithin = func(ctx context.Context, guest string, window time.Duration) (bool, error) {
		gotWindow, seen = window, true
		return false, nil
	}
	a := &Activities{D: deps}
	if _, err := a.AttributeActivity(context.Background(), AttributeInput{Host: "web01", FaultClass: "start-service"}); err != nil {
		t.Fatalf("AttributeActivity must not error: %v", err)
	}
	if !seen {
		t.Fatal("GuestConfigChangedWithin was never called")
	}
	if gotWindow <= 0 {
		t.Fatalf("the window threaded to the confighash seam must NEVER be non-positive (ChangedWithin fails closed on <=0), got %s", gotWindow)
	}
	if gotWindow != 30*time.Minute {
		t.Fatalf("expected the compiled 30-minute ceiling when the ruleset leaves Window unset, got %s", gotWindow)
	}
}
