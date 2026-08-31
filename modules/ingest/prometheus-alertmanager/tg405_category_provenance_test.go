package alertmanager

import (
	"context"
	"fmt"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// TG-405 — an operator "category" label whose VALUE collides with TG's safety vocabulary must not reach the
// key the poll-forcing clamp reads. The estate uses labels["category"] for subsystem names; the safety
// driver reads the same key for {maintenance, security-incident, deployment}. A subsystem named
// "maintenance" would otherwise force POLL_PAUSE on every one of its alerts, forever.

// TestCollidingCategoryIsDemotedOffTheSafetyKey — the end-to-end oracle over the real Normalize pipeline
// (not just the helper), because the defect was that the pipeline passed the raw label through.
func TestCollidingCategoryIsDemotedOffTheSafetyKey(t *testing.T) {
	payload := `{"status":"firing","alerts":[{"status":"firing",
	  "labels":{"alertname":"NodeMaintenanceWindow","instance":"nl-app01:9100","severity":"warning","category":"maintenance"},
	  "annotations":{"summary":"x"},"startsAt":"2026-07-15T12:00:00Z","fingerprint":"m1"}]}`

	env, err := mod().Normalize(context.Background(), []byte(payload))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got := env.Labels["category"]; got != "" {
		t.Errorf("labels[category] = %q — a colliding operator value reached the SAFETY key, so a subsystem "+
			"named for a TG high-risk category would force POLL_PAUSE on every alert (TG-405)", got)
	}
	if got := env.Labels[AlertCategoryLabel]; got != "maintenance" {
		t.Errorf("labels[%s] = %q, want maintenance — the value must be PRESERVED for RAG, only moved off the "+
			"safety key", AlertCategoryLabel, got)
	}
	// The invariant the whole fix rests on: what the safety driver will read is not high-risk.
	if safety.HighRiskCategory(env.Labels["category"]) {
		t.Error("the demoted alert still reads high-risk at the safety key — the clamp is still operator-driven")
	}
}

// TestNonCollidingCategoryPassesThrough — the negative control. Every real estate value measured
// (mesh-bgp, storage-write-path, ...) is NOT in the safety set and must be untouched, or the reachability
// gauge (cmd/worker/category_coverage.go) loses the collision signal it was built to show, and RAG loses
// the label.
func TestNonCollidingCategoryPassesThrough(t *testing.T) {
	payload := `{"status":"firing","alerts":[{"status":"firing",
	  "labels":{"alertname":"MeshBGPDown","instance":"nl-frr01:9100","severity":"warning","category":"mesh-bgp"},
	  "annotations":{"summary":"x"},"startsAt":"2026-07-15T12:00:00Z","fingerprint":"b1"}]}`

	env, err := mod().Normalize(context.Background(), []byte(payload))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got := env.Labels["category"]; got != "mesh-bgp" {
		t.Errorf("labels[category] = %q, want mesh-bgp — a non-colliding subsystem label must pass through "+
			"untouched", got)
	}
	if _, ok := env.Labels[AlertCategoryLabel]; ok {
		t.Errorf("labels[%s] was set for a non-colliding category — only colliding values are demoted", AlertCategoryLabel)
	}
}

// TestEverySafetyCategoryIsDemoted — ties the demotion to safety.HighRiskCategory's ACTUAL set, not a
// copied list. A value added to the safety set (e.g. a fourth high-risk category) is defended here on the
// same commit, with no second vocabulary to keep in sync. Killing mutation: gate the demotion on a
// hardcoded {"maintenance"} instead of safety.HighRiskCategory → the others reach the safety key → RED.
func TestEverySafetyCategoryIsDemoted(t *testing.T) {
	for _, v := range []string{"maintenance", "security-incident", "deployment"} {
		if !safety.HighRiskCategory(v) {
			t.Fatalf("test premise stale: %q is no longer a high-risk category — update this list", v)
		}
		in := map[string]string{"alertname": "X", "category": v}
		out := demoteCollidingSafetyCategory(in)
		if _, ok := out["category"]; ok {
			t.Errorf("category %q was left on the safety key", v)
		}
		if out[AlertCategoryLabel] != v {
			t.Errorf("category %q was not preserved as %s", v, AlertCategoryLabel)
		}
	}
}

// TestDemotionIsCaseInsensitiveLikeTheDriver — safety.HighRiskCategory lower-cases before matching, so a
// mixed-case "Maintenance" clamps; the demotion must defend the same values the driver acts on, or a
// capitalised collision slips straight through to the clamp.
func TestDemotionIsCaseInsensitiveLikeTheDriver(t *testing.T) {
	for _, v := range []string{"Maintenance", "SECURITY-INCIDENT", "Deployment"} {
		if !safety.HighRiskCategory(v) {
			continue // driver would not clamp on it, so no need to demote
		}
		out := demoteCollidingSafetyCategory(map[string]string{"category": v})
		if got, ok := out["category"]; ok {
			t.Errorf("%q left on the safety key (%q) — a capitalised collision reaches the clamp the driver "+
				"WOULD fire on", v, got)
		}
	}
}

// A tiny sanity check the helper does not disturb unrelated labels or the no-category case.
func TestDemotionLeavesOtherLabelsAndEmptyCase(t *testing.T) {
	got := demoteCollidingSafetyCategory(map[string]string{"alertname": "X", "severity": "warning"})
	if len(got) != 2 {
		t.Errorf("no-category input was altered: %v", got)
	}
	_ = fmt.Sprint(got) // keep fmt imported for the error paths above
}
