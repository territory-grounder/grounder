package opclasscluster

// ORACLES FOR THE READY RESOLVER (TG-227 blocker 1). Each names its killing mutation.

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/opclasscat"
)

func roc(op, target string) opclasscat.Occurrence {
	return opclasscat.Occurrence{Op: op, Target: target, ExternalRef: op + "/" + target}
}

// KILLING MUTATION: pass only the slug/op to ScreenAutoBarred, ignoring the observed journal. RED —
// a destructive command hiding under a benign slug would screen clean.
func TestScreenStampsBarredFromObservedOpsAndTargets(t *testing.T) {
	r := NewReadyResolver(func() *estate.Graph { return nil }, nil)
	c := opclasscat.Candidate{OpClass: "restart-service", Op: "restart"} // benign by slug
	in, err := r(context.Background(), c, []opclasscat.Occurrence{roc("rm -rf /var/lib/data", "dc1x01")})
	if err != nil {
		t.Fatal(err)
	}
	if !in.AutoBarredStamped {
		t.Fatal("screen did not stamp")
	}
	if !in.AutoBarred {
		t.Fatal("a destructive OBSERVED op under a benign slug screened clean — the screen is reading " +
			"the slug's claim, not the candidate's behaviour")
	}
	// control: genuinely benign observations screen clean, so the test cannot pass by barring everything
	in2, err := r(context.Background(), c, []opclasscat.Occurrence{roc("restart", "svc-a")})
	if err != nil {
		t.Fatal(err)
	}
	if in2.AutoBarred {
		t.Fatal("a benign candidate was barred — the screen refuses everything, which would make the " +
			"barred stamp meaningless")
	}
}

// KILLING MUTATION: default coverage to 1.0, count unresolvable targets as covered, or treat zero
// targets as full coverage. RED on each — an incomplete dossier must stay below the gate.
func TestCoverageFailsClosedOnUnresolvableTargets(t *testing.T) {
	r := NewReadyResolver(func() *estate.Graph { return estate.NewGraph() }, nil)
	c := opclasscat.Candidate{OpClass: "restart-service", Op: "restart"}

	// zero targets ⇒ zero coverage, not vacuous 100%
	in, err := r(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.BlastRadiusCoverage != 0 {
		t.Fatalf("zero targets yielded coverage %.2f, want 0 — nothing was computed, so nothing is covered",
			in.BlastRadiusCoverage)
	}

	// unresolvable targets count AGAINST coverage
	in, err = r(context.Background(), c, []opclasscat.Occurrence{roc("restart", "ghost-host-a"), roc("restart", "ghost-host-b")})
	if err != nil {
		t.Fatal(err)
	}
	if in.BlastRadiusCoverage != 0 {
		t.Fatalf("unresolvable targets yielded coverage %.2f, want 0", in.BlastRadiusCoverage)
	}

	// a nil graph is 0 coverage, never a crash and never 100%
	rNil := NewReadyResolver(func() *estate.Graph { return nil }, nil)
	in, err = rNil(context.Background(), c, []opclasscat.Occurrence{roc("restart", "any")})
	if err != nil {
		t.Fatal(err)
	}
	if in.BlastRadiusCoverage != 0 {
		t.Fatalf("nil graph yielded coverage %.2f, want 0", in.BlastRadiusCoverage)
	}
}

// KILLING MUTATION: first-match family guessing (return the first keyword hit instead of refusing a
// conflict), or defaulting an unknown to some family. RED — a mislabeled family is a wrong closed-set
// claim inside a grant.
func TestFamilyAmbiguityKeepsCandidateBelowTheGate(t *testing.T) {
	// conflict: "docker" votes container-lifecycle, "vacuum" votes disk-reclaim
	if fam, ok := opclasscat.AssignFamily("vacuum-docker-logs", "vacuum"); ok {
		t.Fatalf("conflicting keyword votes assigned family %q — ambiguity must fail closed", fam)
	}
	// zero hits
	if fam, ok := opclasscat.AssignFamily("frobnicate-widget", "frobnicate"); ok {
		t.Fatalf("no keyword hit assigned family %q — unknown must fail closed", fam)
	}
	// control: an unambiguous slug assigns, so the refusals above are discriminating, not universal
	fam, ok := opclasscat.AssignFamily("vacuum-systemd-journal", "vacuum")
	if !ok || fam != "disk-reclaim" {
		t.Fatalf("unambiguous slug: got (%q,%v), want (disk-reclaim,true)", fam, ok)
	}
	// and the resolver leaves Family/Tier empty on ambiguity, which MeetsRatifyReady refuses
	r := NewReadyResolver(func() *estate.Graph { return nil }, nil)
	in, err := r(context.Background(), opclasscat.Candidate{OpClass: "frobnicate-widget", Op: "frobnicate"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.Family != "" || in.Tier != "" {
		t.Fatalf("ambiguous candidate carries family %q tier %q — must stay empty so the gate holds", in.Family, in.Tier)
	}
	if opclasscat.MeetsRatifyReady(opclasscat.Evidence{DistinctRefs: 99}, in) {
		t.Fatal("an ambiguous, uncovered dossier passed MeetsRatifyReady")
	}
}

// KILLING MUTATION: revert the allowed-map ratify_ready→expired edge. RED — a 60-day-silent ratify_ready
// row would fail its expiry transition on every cron pass, forever.
func TestRatifyReadySilenceMayExpire(t *testing.T) {
	if !opclasscat.TransitionAllowed(opclasscat.StatusRatifyReady, opclasscat.StatusExpired) {
		t.Fatal("ratify_ready→expired refused — the cron's silence expiry would wedge with " +
			"ErrBadTransition on every pass")
	}
}

// KILLING MUTATION: AssignTier hands a machine-assigned auto-eligible tier to a barred class (or any
// class). RED — TierLowReversible is an operator's attestation, never a machine's.
func TestMechanicalTierIsNeverAutoEligible(t *testing.T) {
	for _, tc := range []struct {
		family string
		barred bool
		want   string
	}{
		{"disk-reclaim", true, "irreversible"},
		{"service-lifecycle", false, "medium"},
		{"network-device", false, "vendor-critical"},
		{"network-device", true, "vendor-critical"},
	} {
		if got := opclasscat.AssignTier(tc.family, tc.barred); got != tc.want {
			t.Errorf("AssignTier(%q, barred=%v) = %q, want %q", tc.family, tc.barred, got, tc.want)
		}
	}
}
