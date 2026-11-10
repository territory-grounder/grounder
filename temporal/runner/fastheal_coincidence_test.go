package runner

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/correlate"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/modules/ingest/pveliveness"
)

// TG-646, the G3 soak shape (2026-11-06 16:24 NL): a confirmed-stopped guest shares its correlation window
// with unrelated alerts. The election is non-causal (earliest-ref), so the guest is no cascade member and
// must keep the deterministic heal instead of going to an agent that cannot see it.
func TestCoincidentalWindowKeepsTheDeterministicGuestHeal(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	in := CorrelateInput{ExternalRef: "tg-liveness-site01vm01-1", SourceID: pveliveness.SourceType,
		AlertRule: pveliveness.DeviceDownRule, Host: "site01vm01", Severity: "critical", At: base}
	window := correlate.Window{Span: 10 * time.Minute, Observations: []correlate.Observation{
		{ExternalRef: "librenms-other-1", Host: "site01app01", SourceType: "librenms", AlertRule: "Service up/down", At: base.Add(-2 * time.Minute)},
		{ExternalRef: in.ExternalRef, Host: in.Host, SourceType: pveliveness.SourceType, AlertRule: pveliveness.DeviceDownRule, At: base},
	}}
	deps := Deps{
		CorrelationWindow: func(context.Context, time.Time) (correlate.Window, error) { return window, nil },
		Gate:              &predict.PredictionGate{GuestRunning: stopped},
	}
	res, err := NewActivities(deps).CorrelateActivity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Correlated {
		t.Fatalf("precondition: the two-source window must assess correlated, got %+v", res)
	}
	if !res.FastHeal || res.ExecClass != string(execclass.Deterministic) {
		t.Fatalf("a coincidental window must keep the deterministic heal, got FastHeal=%v class=%s", res.FastHeal, res.ExecClass)
	}
}

// ...but a guest that runs under the causally elected root (its hypervisor is down) is a cascade member:
// no deterministic heal, the cascade is investigated.
func TestCausalCascadeMemberLosesTheDeterministicGuestHeal(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	in := CorrelateInput{ExternalRef: "tg-liveness-site01vm01-2", SourceID: pveliveness.SourceType,
		AlertRule: pveliveness.DeviceDownRule, Host: "site01vm01", Severity: "critical", At: base}
	window := correlate.Window{Span: 10 * time.Minute, Observations: []correlate.Observation{
		{ExternalRef: "librenms-node-down", Host: "site01node01", SourceType: "librenms", AlertRule: "Device down", At: base.Add(-time.Minute)},
		{ExternalRef: "tg-liveness-site01vm02-1", Host: "site01vm02", SourceType: pveliveness.SourceType, AlertRule: pveliveness.DeviceDownRule, At: base.Add(-30 * time.Second)},
		{ExternalRef: in.ExternalRef, Host: in.Host, SourceType: pveliveness.SourceType, AlertRule: pveliveness.DeviceDownRule, At: base},
	}}
	topo := causalTopoFake{ind: map[string]int{"site01node01": 12},
		par: map[string]string{"site01vm01": "site01node01", "site01vm02": "site01node01"}}
	deps := Deps{
		CorrelationWindow: func(context.Context, time.Time) (correlate.Window, error) { return window, nil },
		ClusterTopology:   topo,
		Gate:              &predict.PredictionGate{GuestRunning: stopped},
	}
	res, err := NewActivities(deps).CorrelateActivity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.FastHeal || res.ExecClass == string(execclass.Deterministic) {
		t.Fatalf("a guest under a down hypervisor must not take the deterministic heal, got FastHeal=%v class=%s", res.FastHeal, res.ExecClass)
	}
}

func TestCausalCascadeMember(t *testing.T) {
	topo := causalTopoFake{par: map[string]string{"vm01": "node01"}}
	causal := correlate.Election{Rule: correlate.ElectRuleIndegree, ElectedHost: "node01"}
	if !causalCascadeMember(true, causal, topo, "vm01") {
		t.Fatal("a guest under the causally elected host is a member")
	}
	if causalCascadeMember(true, correlate.Election{Rule: correlate.ElectRuleEarliest, ElectedHost: "node01"}, topo, "vm01") {
		t.Fatal("a non-causal election makes no member")
	}
	if causalCascadeMember(true, correlate.Election{Rule: correlate.ElectRuleIndegree, ElectedHost: "vm01.example"}, topo, "vm01") {
		t.Fatal("the elected root itself is not a member")
	}
	if causalCascadeMember(false, causal, topo, "vm01") {
		t.Fatal("an uncorrelated window has no members")
	}
}
