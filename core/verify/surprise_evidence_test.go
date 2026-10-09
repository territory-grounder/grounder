package verify

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// THE MOST CONSEQUENTIAL VERDICT CARRIED THE THINNEST EVIDENCE.
//
// A RuleMismatch — a PARTIAL trigger — recorded (host, rule). A surprise host — the trigger that DEMOTES an
// op-class and TRIPS the estate-wide breaker — recorded only a hostname. Measured cost (governance_ledger seq
// 6555, 2026-07-30): `surprise-hosts=[dc2lte01]` demoted restart-container auto->approve and discarded ~80
// hands-off clean runs. Recovering WHICH alert that was took six external queries against two LibreNMS
// instances plus discovering that one of them stores local time instead of UTC. It turned out to be an
// unrelated 59-second sensor flap at the other site.
//
// These tests assert the evidence reaches the LEDGER SURFACE (Summary), not merely the struct field. A field
// that is populated correctly and never rendered is the defect shape this repo keeps rediscovering — a
// correctly-computed finding thrown away at the last hop.
func TestSurpriseEvidenceNamesTheRuleAndReachesTheLedgerLine(t *testing.T) {
	pred := Prediction{
		ActionID: "a1", TargetHost: "dc1mealie01", Site: "dc1",
		PredictedHosts: map[string]struct{}{},
		PredictedRules: map[string]struct{}{},
	}
	// The live shape: the action's own host alerts (expected), plus ONE unrelated alert on a host in another
	// site that the prediction never named.
	observed := []ObservedAlert{
		{Host: "dc1mealie01", Rule: "Devices up/down", Site: "dc1"},
		{Host: "dc2lte01", Rule: "Sensor under limit - Check Device Health Settings", Site: "dc2"},
	}

	d := ComputeVerdictDetail(pred, observed)

	if d.Verdict != safety.VerdictDeviation {
		t.Fatalf("verdict = %v, want deviation — the fixture must actually exercise the surprise branch, or "+
			"this whole test is vacuous", d.Verdict)
	}
	// 1. THE RULE IS CAPTURED.
	if len(d.SurpriseAlerts) != 1 {
		t.Fatalf("SurpriseAlerts = %+v, want exactly 1", d.SurpriseAlerts)
	}
	if got := d.SurpriseAlerts[0]; got.Host != "dc2lte01" ||
		got.Rule != "Sensor under limit - Check Device Health Settings" {
		t.Errorf("SurpriseAlerts[0] = %+v — the deviation trigger must name the rule that fired, which is the "+
			"field whose absence made a real deviation undiagnosable", got)
	}
	// 2. AND IT REACHES THE LEDGER LINE. interceptor.go composes the DEVIATION row from detail.Summary(); a
	//    struct field that never renders there is invisible to the only surface an operator diagnoses from.
	sum := d.Summary()
	if !strings.Contains(sum, "Sensor under limit") {
		t.Errorf("Summary() = %q — it does not name the triggering rule, so the governance ledger row is still "+
			"undiagnosable and this change bought nothing", sum)
	}
	// 3. THE PROMOTION-GATING IDENTITY IS UNTOUCHED. falsify.DiscoveryRecord's DeviationKey is
	//    (target, site, sorted-surprise-hosts) and gates "reproduces >= N"; core/estate decay keys its disproof
	//    hosts off the same list. Widening either silently redefines a promotion gate.
	if len(d.SurpriseHosts) != 1 || d.SurpriseHosts[0] != "dc2lte01" {
		t.Errorf("SurpriseHosts = %v, want exactly [dc2lte01] — this list is a promotion-gating signature "+
			"and must stay byte-identical", d.SurpriseHosts)
	}
	if !strings.Contains(sum, "surprise-hosts=[dc2lte01]") {
		t.Errorf("Summary() = %q — the pre-existing surprise-hosts rendering changed shape; the judge corpus "+
			"and downstream string consumers parse it", sum)
	}
}

// One surprise HOST carrying several rules is still ONE host (the gating identity) and several alerts (the
// evidence). Getting this backwards would either inflate the deviation key or lose a rule.
func TestSeveralRulesOnOneSurpriseHostStayOneHostAndManyAlerts(t *testing.T) {
	pred := Prediction{TargetHost: "h-target", PredictedHosts: map[string]struct{}{}, PredictedRules: map[string]struct{}{}}
	observed := []ObservedAlert{
		{Host: "zeta01", Rule: "rule-b"},
		{Host: "alpha01", Rule: "rule-a"},
		{Host: "zeta01", Rule: "rule-a"},
		{Host: "zeta01", Rule: "rule-b"}, // exact duplicate — must dedup
	}
	d := ComputeVerdictDetail(pred, observed)

	if len(d.SurpriseHosts) != 2 {
		t.Errorf("SurpriseHosts = %v, want 2 distinct hosts", d.SurpriseHosts)
	}
	if len(d.SurpriseAlerts) != 3 {
		t.Fatalf("SurpriseAlerts = %+v, want 3 distinct (host,rule) pairs — duplicates must collapse and "+
			"distinct rules on one host must NOT", d.SurpriseAlerts)
	}
	// Deterministic ordering, or the ledger line and the judge corpus differ run to run for identical inputs.
	want := []SurpriseAlert{
		{Host: "alpha01", Rule: "rule-a"},
		{Host: "zeta01", Rule: "rule-a"},
		{Host: "zeta01", Rule: "rule-b"},
	}
	for i, w := range want {
		if d.SurpriseAlerts[i] != w {
			t.Errorf("SurpriseAlerts[%d] = %+v, want %+v — ordering must be sorted by (host,rule)", i, d.SurpriseAlerts[i], w)
		}
	}
}

// THE EXCLUSIONS MUST STILL HOLD, and each must be exercised separately — otherwise the new field could be
// populated from alerts the verdict itself correctly ignores, which would make the ledger row actively
// misleading (naming "evidence" that did not cause the verdict).
func TestSurpriseAlertsHonourEveryExclusionTheVerdictHonours(t *testing.T) {
	pred := Prediction{
		TargetHost:     "target01",
		PredictedHosts: map[string]struct{}{"predicted01": {}},
		PredictedRules: map[string]struct{}{RuleKey("predicted01", "known-rule"): {}},
	}
	observed := []ObservedAlert{
		{Host: "target01", Rule: "any"},              // the action's own host — expected direct effect
		{Host: "predicted01", Rule: "known-rule"},    // fully predicted
		{Host: "predicted01", Rule: "surprise-rule"}, // predicted host, unpredicted rule => MISMATCH, not surprise
		{Host: "prebaseline01", Rule: "old"},         // pre-existing (host,rule) pair
		{Host: "preanomalous01", Rule: "whatever"},   // host already held an OPEN incident
	}
	baseline := []ObservedAlert{{Host: "prebaseline01", Rule: "old"}}
	preAnom := map[string]bool{"preanomalous01": true}

	d := ComputeVerdictDetailWithBaselines(pred, observed, baseline, preAnom)

	if len(d.SurpriseAlerts) != 0 {
		t.Errorf("SurpriseAlerts = %+v, want none — every observed alert here is excluded by the target, the "+
			"pair baseline, the pre-anomalous host arm, or is a rule MISMATCH rather than a surprise", d.SurpriseAlerts)
	}
	if d.Verdict != safety.VerdictPartial {
		t.Errorf("verdict = %v, want partial (the unpredicted rule on a predicted host)", d.Verdict)
	}
	if len(d.Mismatches) != 1 {
		t.Errorf("Mismatches = %+v, want the one predicted-host/unpredicted-rule pair", d.Mismatches)
	}
	// A clean run must render an EMPTY surprise-alerts list, not omit the key — an absent field and a
	// no-surprises field must be distinguishable in the ledger.
	if !strings.Contains(d.Summary(), "surprise-alerts=") {
		t.Errorf("Summary() = %q — the key must always render so its emptiness is an assertion, not a silence", d.Summary())
	}
}
