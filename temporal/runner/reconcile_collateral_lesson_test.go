package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/lessons"
)

// TG-673 (deep-review F14): a heal whose blast radius surfaced a NEW (host, rule) during the settle window is
// not a clean run. It already lost auto-close and the graduation credit (TG-483); until this gate it still
// became a "verified-resolution" precedent and de-noveled the incident — TG's collateral damage taught back to
// it as what worked. Now the precedent gate carries ¬collateral and the session lands in the caution lane.
// KILLING MUTATION (executed, RED): drop `&& !s.CollateralObserved` from the precedent gate in reconcile.go —
// the collateral terminus writes a precedent again and the first assertion fails.
func TestCollateralTerminusIsACautionNotAPrecedent(t *testing.T) {
	var precedents, cautions []lessons.ResolvedIncident
	acts := NewActivities(Deps{
		LearnResolved: func(_ context.Context, ri lessons.ResolvedIncident) error {
			precedents = append(precedents, ri)
			return nil
		},
		LearnCaution: func(_ context.Context, ri lessons.ResolvedIncident) error {
			cautions = append(cautions, ri)
			return nil
		},
	})
	in := cleanWritebackInput()
	in.CollateralObserved = true
	if _, err := acts.ReconcileActivity(context.Background(), in); err != nil {
		t.Fatalf("reconcile must not error: %v", err)
	}
	if len(precedents) != 0 {
		t.Fatalf("a terminus with observed collateral must NEVER become a precedent, got %d", len(precedents))
	}
	if len(cautions) != 1 {
		t.Fatalf("a terminus with observed collateral is the caution lane's subject, got %d caution(s)", len(cautions))
	}
	if cautions[0].ExternalRef != in.ExternalRef || cautions[0].Host != in.Host || cautions[0].AlertRule != in.AlertRule || !cautions[0].Collateral {
		t.Fatalf("caution signature drifted (Collateral must ride the record): %+v", cautions[0])
	}
	// The record must pass the REAL caution gate the production seam applies (lessons.CautionMerge → Caution)
	// and fail the real precedent gate — a stubbed seam proves nothing about the corpus the worker writes
	// (fresh-eyes review of this change, 2026-11-07).
	if _, isLesson := lessons.Lesson(cautions[0]); isLesson {
		t.Fatal("a collateral outcome must never distill to a precedent")
	}
	c, isCaution := lessons.Caution(cautions[0])
	if !isCaution {
		t.Fatal("a collateral outcome must distill to a caution in production, not be dropped as a clean success")
	}
	if !strings.Contains(c.Resolution, "collateral") {
		t.Fatalf("the caution must say WHY: %q", c.Resolution)
	}

	// CONTROL: the identical terminus without collateral is still the precedent's, and fires no caution.
	precedents, cautions = nil, nil
	if _, err := acts.ReconcileActivity(context.Background(), cleanWritebackInput()); err != nil {
		t.Fatal(err)
	}
	if len(precedents) != 1 || len(cautions) != 0 {
		t.Fatalf("control: a clean terminus writes exactly one precedent and no caution, got %d/%d", len(precedents), len(cautions))
	}
}
