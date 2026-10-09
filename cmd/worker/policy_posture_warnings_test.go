package main

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/policy"
)

// The metric renders WarnFor's verdict onto the closed code set: an active condition reads 1, the rest read 0
// (never absent), so a permissive posture is visible and a clean one is an explicit row of zeros.
func TestPolicyPostureWarningSamples_ActiveReadOneRestZeroNeverAbsent(t *testing.T) {
	warns := []policy.PolicyWarning{
		{Code: policy.WarnAllowAllRule, Message: "allow-all"},
		{Code: policy.WarnFullAuto, Message: "full-auto"},
	}
	got := map[string]float64{}
	for _, s := range policyPostureWarningSamples(warns, time.Time{}) {
		if s.Name != "tg_policy_posture_warning" {
			t.Fatalf("unexpected metric name %q", s.Name)
		}
		if s.Kind != "gauge" {
			t.Errorf("posture warning must be a gauge, got %q", s.Kind)
		}
		got[s.Labels["code"]] = s.Value
	}
	// the FULL closed set is emitted (legible zeros), not just the active ones.
	if len(got) != len(policyPostureWarningCodes) {
		t.Fatalf("expected the full closed set emitted, got %d of %d", len(got), len(policyPostureWarningCodes))
	}
	if got["allow-all-rule"] != 1 || got["full-auto-mode"] != 1 {
		t.Errorf("active conditions must read 1: allow-all=%v full-auto=%v", got["allow-all-rule"], got["full-auto-mode"])
	}
	if got["floor-entry-removed"] != 0 || got["engine-disabled"] != 0 {
		t.Errorf("inactive conditions must read 0, not be absent: floor=%v engine-disabled=%v",
			got["floor-entry-removed"], got["engine-disabled"])
	}
}

// A clean posture still emits the closed set — as all-zeros (visible), never as a dark/absent metric.
func TestPolicyPostureWarningSamples_CleanPostureIsVisibleZeros(t *testing.T) {
	got := policyPostureWarningSamples(nil, time.Time{})
	if len(got) != len(policyPostureWarningCodes) {
		t.Fatalf("clean posture must still emit the full closed set, got %d", len(got))
	}
	for _, s := range got {
		if s.Value != 0 {
			t.Errorf("clean posture code %q must be 0, got %v", s.Labels["code"], s.Value)
		}
	}
}
