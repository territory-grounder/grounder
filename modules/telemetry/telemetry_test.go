package telemetry_test

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/telemetry"
)

// TestCapabilitySamples proves the worker's self-telemetry reflects the registry: a liveness gauge, a total,
// and per-(surface,state) counts — so a disabled member is visibly counted separately from an enabled one.
func TestCapabilitySamples(t *testing.T) {
	reg := modules.NewRegistry()
	_ = reg.Register(modules.Registration{Surface: modules.SurfaceModel, SourceType: "zai", Capability: "model.zai", Enabled: true, Adapter: struct{}{}})
	_ = reg.Register(modules.Registration{Surface: modules.SurfaceModel, SourceType: "openai", Capability: "model.openai", Enabled: true, Adapter: struct{}{}})
	_ = reg.Register(modules.Registration{Surface: modules.SurfaceActuation, SourceType: "ssh", Capability: "actuation.ssh", Enabled: false, Adapter: struct{}{}})

	now := time.Unix(1_700_000_000, 0)
	samples := telemetry.CapabilitySamples(reg, now)

	get := func(name, surface, state string) (float64, bool) {
		for _, s := range samples {
			if s.Name == name && s.Labels["surface"] == surface && s.Labels["state"] == state {
				return s.Value, true
			}
		}
		return 0, false
	}

	if v, ok := get("tg_worker_up", "", ""); !ok || v != 1 {
		t.Errorf("tg_worker_up must be 1, got %v (ok=%v)", v, ok)
	}
	if v, ok := get("tg_module_capabilities_total", "", ""); !ok || v != 3 {
		t.Errorf("total must be 3, got %v", v)
	}
	if v, ok := get("tg_module_capabilities", "model", "enabled"); !ok || v != 2 {
		t.Errorf("model/enabled must be 2, got %v", v)
	}
	if v, ok := get("tg_module_capabilities", "actuation", "disabled"); !ok || v != 1 {
		t.Errorf("actuation/disabled must be 1 (visibly separate from enabled), got %v", v)
	}
	// The disabled actuator must NOT be counted as enabled.
	if _, ok := get("tg_module_capabilities", "actuation", "enabled"); ok {
		t.Error("a disabled actuator must not appear under state=enabled")
	}
	// All samples stamped at now (deterministic).
	for _, s := range samples {
		if !s.Stamped.Equal(now) {
			t.Errorf("sample %s not stamped at now", s.Name)
		}
	}
}

// TestSuppressionSamples proves the suppression decision counts render as per-outcome gauges (sorted,
// stamped), so ops can dashboard the suppression rate.
func TestSuppressionSamples(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	got := telemetry.SuppressionSamples(map[string]int{"escalate": 7, "suppressed": 3}, now)
	if len(got) != 2 {
		t.Fatalf("want 2 samples, got %d", len(got))
	}
	find := func(outcome string) (float64, bool) {
		for _, s := range got {
			if s.Name == "tg_suppression_decisions" && s.Labels["outcome"] == outcome {
				return s.Value, s.Stamped.Equal(now)
			}
		}
		return 0, false
	}
	if v, ok := find("suppressed"); !ok || v != 3 {
		t.Errorf("suppressed gauge = %v (stamped-ok=%v), want 3", v, ok)
	}
	if v, ok := find("escalate"); !ok || v != 7 {
		t.Errorf("escalate gauge = %v (stamped-ok=%v), want 7", v, ok)
	}
}
