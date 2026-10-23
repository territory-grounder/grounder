package opclassratify

// The package's first test file. Small on purpose: the verbs' full behaviour is driven end to end by the
// worker's oracles; what THIS file pins is the one contract the new Refreshed seam adds (TG-227
// blocker 2's convergence half), so the seam cannot regress into a panic or a silent no-op.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/opclasscat"
)

// KILLING MUTATION: remove the nil check in notifyRefreshed. RED (panic) — every deployment that leaves
// the optional field unset would crash its first ratify.
func TestNotifyRefreshedIsNilSafe(t *testing.T) {
	Deps{}.notifyRefreshed() // must not panic
}

// KILLING MUTATION: notifyRefreshed stops calling the hook (a silent no-op keeps ratified grants waiting
// out the full refresh TTL with nothing red). RED.
func TestNotifyRefreshedFiresTheHook(t *testing.T) {
	fired := 0
	Deps{Refreshed: func() { fired++ }}.notifyRefreshed()
	if fired != 1 {
		t.Fatalf("hook fired %d times, want 1", fired)
	}
}

// --- the barred/tier coupling (TG-227 blocker 4's enforcement half) ---

type fakeLoader struct{ c opclasscat.Candidate }

func (f fakeLoader) CandidateByKey(context.Context, string) (opclasscat.Candidate, bool, error) {
	return f.c, true, nil
}
func (f fakeLoader) Occurrences(context.Context, string, time.Time) ([]opclasscat.Occurrence, error) {
	return nil, nil
}

// KILLING MUTATION: delete or invert the AutoBarred/AutoEligible guard in ratify. RED — a candidate whose
// OBSERVED behaviour was screened destructive could be granted at a tier the ladder can climb to silence.
func TestBarredCandidateRefusedAtAutoEligibleTier(t *testing.T) {
	a := &Activities{D: Deps{Loader: fakeLoader{c: opclasscat.Candidate{AutoBarred: true}}}}
	req := Request{Verb: VerbRatify, CandidateKey: "k", Rationale: "r",
		Spec: opschema.OpClassSpec{OpClass: "x", SafetyTier: opschema.TierLowReversible}}
	_, err := a.OpClassVerbActivity(context.Background(), req)
	if !errors.Is(err, ErrAutoBarredTier) {
		t.Fatalf("barred + auto-eligible tier: got %v, want ErrAutoBarredTier", err)
	}
	// The refusal must be TIER-directional, not universal: the same barred candidate at a never-auto
	// tier gets PAST this guard (it then fails later on these deliberately-nil deps — a different error).
	req.Spec.SafetyTier = opschema.TierIrreversible
	_, err = a.OpClassVerbActivity(context.Background(), req)
	if errors.Is(err, ErrAutoBarredTier) {
		t.Fatal("a barred candidate at a NEVER-AUTO tier was refused by the tier guard — barred classes " +
			"must stay ratifiable at asks-first tiers (visible, never climbable)")
	}
}
