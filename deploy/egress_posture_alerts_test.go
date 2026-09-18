package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TG-324 item 1. The egress meter was flipped to enforce and verified live 2026-08-07 —
// tg_egress_enforcing = 1 on both planes, 0 refusals and 0 off-allowlist destinations across 28,294
// metered requests. NOTHING WATCHED ANY OF IT. The posture is an env var and the allowlist is config;
// both change without a commit, so a control verified once drifts in silence.
//
// promtool parses these rules but cannot see their INTENT: raising the refusal threshold to >1000, or
// inverting the enforcement comparison, both `check rules` cleanly. That is what this file is for.

type egressAlertRule struct{ Alert, Expr, Severity string }

func egressRules(t *testing.T) []egressAlertRule {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("monitoring", "alert.rules.yml"))
	if err != nil {
		t.Fatalf("read alert.rules.yml: %v", err)
	}
	var doc struct {
		Groups []struct {
			Rules []struct {
				Alert  string            `yaml:"alert"`
				Expr   string            `yaml:"expr"`
				Labels map[string]string `yaml:"labels"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse alert.rules.yml: %v", err)
	}
	var total int
	var out []egressAlertRule
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			total++
			if strings.Contains(r.Expr, "tg_egress_") || strings.Contains(r.Expr, "tg_estate_edges") {
				out = append(out, egressAlertRule{r.Alert, strings.Join(strings.Fields(r.Expr), " "), r.Labels["severity"]})
			}
		}
	}
	// VACUITY FLOOR: this file asserts things ABOUT rules, so an unread file would pass every assertion
	// below by having nothing to check.
	if total < 20 {
		t.Fatalf("VACUITY FLOOR: parsed only %d alert rules — the file is not being read", total)
	}
	return out
}

func findEgressRule(t *testing.T, name string) egressAlertRule {
	t.Helper()
	for _, r := range egressRules(t) {
		if r.Alert == name {
			return r
		}
	}
	t.Fatalf("alert %s is gone. The egress control is enforcing in production and this is what would "+
		"say so if it stopped; deleting it returns the posture to unwatched.", name)
	return egressAlertRule{}
}

// TestEgressEnforcementOffFiresOnTheREVERTEDPosture. The comparison direction is the whole rule: `== 1`
// parses identically and fires continuously on the HEALTHY estate, which gets the alert muted and
// removes the coverage entirely.
func TestEgressEnforcementOffFiresOnTheREVERTEDPosture(t *testing.T) {
	r := findEgressRule(t, "EgressEnforcementOff")
	if !strings.Contains(r.Expr, "tg_egress_enforcing == 0") {
		t.Errorf("EgressEnforcementOff is %q. It must fire on tg_egress_enforcing == 0 — the REVERTED "+
			"posture. Any other comparison either never fires or fires on the healthy state, and an alert "+
			"that fires on health gets muted, which is worse than not having it.", r.Expr)
	}
	// It must NOT double as a liveness check: an absent series is a dead worker, which other rules own.
	if strings.Contains(r.Expr, "absent(") {
		t.Error("EgressEnforcementOff uses absent() — a worker restart would then page as a security " +
			"regression, and the alert that cried liveness gets muted before the real posture change.")
	}
}

// TestEgressRefusalAlertFiresOnTheFirstRefusal is the enforce-flip's REVERT SIGNAL. TG-324 states the
// risk in one line: "a wrong allowlist under enforcement takes production off the network." One refused
// destination is already a capability TG has silently lost — the caller sees the failure, not this rule.
func TestEgressRefusalAlertFiresOnTheFirstRefusal(t *testing.T) {
	r := findEgressRule(t, "EgressRefusingTraffic")
	if !strings.Contains(r.Expr, "tg_egress_refused_total") {
		t.Fatalf("EgressRefusingTraffic no longer reads tg_egress_refused_total: %q", r.Expr)
	}
	if !strings.Contains(r.Expr, "> 0") {
		t.Errorf("EgressRefusingTraffic is %q — it must fire above ZERO. Any positive threshold swallows "+
			"exactly the case the flip was gated on: a legitimate destination that was never declared, "+
			"refused a handful of times, surfacing as an unrelated failure deep inside a subsystem.", r.Expr)
	}
	if !strings.Contains(r.Expr, "increase(") {
		t.Errorf("EgressRefusingTraffic is %q — a bare counter comparison latches forever after the first "+
			"refusal (and resets on deploy). It must read a RATE of new refusals.", r.Expr)
	}
}

// TestEgressAllowlistEmptyGuardsTheVacuousComparison. TG-324 names this precondition itself: "a zero
// there means the meter is comparing against an empty declaration, so a flat off-allowlist series proves
// nothing."
func TestEgressAllowlistEmptyGuardsTheVacuousComparison(t *testing.T) {
	r := findEgressRule(t, "EgressAllowlistEmpty")
	if !strings.Contains(r.Expr, "tg_egress_allowlist_rules == 0") {
		t.Errorf("EgressAllowlistEmpty is %q, want it to fire on an EMPTY allowlist. At zero, no reading "+
			"of tg_egress_offallowlist_requests_total means anything in either mode — the control reports "+
			"health because it is comparing against nothing.", r.Expr)
	}
}

// TestTheEgressPostureIsCoveredAtAll is the floor over all three: naming them individually above would
// still pass if someone deleted the group and left one rule behind.
func TestTheEgressPostureIsCoveredAtAll(t *testing.T) {
	rules := egressRules(t)
	if len(rules) < 3 {
		var names []string
		for _, r := range rules {
			names = append(names, r.Alert)
		}
		t.Fatalf("only %d rule(s) reference tg_egress_* (%v). The egress control ENFORCES in production; "+
			"the three ways it stops protecting anything are posture reverted, allowlist empty, and "+
			"refusing real traffic.", len(rules), names)
	}
}

// TG-395 channel (b). A PARTIAL estate collapse had no expressible rule: EstateGraphEmpty catches only
// the total case (== 0), and nothing spoke about a graph that lost most of itself and kept going. On
// 2026-08-06 a dead hypervisor correctly reported no guests, 52 `runs_on` edges vanished, and the only
// warnings the operator received were 17-edge wobbles at a different site.
//
// The threshold is the point. Over 27.5h of live series the largest step-over-step relative drops were
// 79.5%, 13.3%, 0.9%, 0.9% — ordinary churn <=0.9%, real events 13.3% and 79.5%. A 10% line sits an
// order of magnitude above the noise and an order below the smaller real event. A 20% line, which is
// the number one would reach for without measuring, MISSES the 13.3% event this ticket was filed about.
func TestEstateCollapseRuleThresholdCatchesTheMeasuredEvent(t *testing.T) {
	r := findEgressRule(t, "EstateGraphCollapsed")

	if !strings.Contains(r.Expr, "tg_estate_edges") {
		t.Fatalf("EstateGraphCollapsed does not read tg_estate_edges: %q", r.Expr)
	}
	if !strings.Contains(r.Expr, "max_over_time") {
		t.Errorf("EstateGraphCollapsed is %q — it must compare against the graph's own recent PEAK. An "+
			"absolute floor cannot work: the right size of this graph is deployment-shaped and grows "+
			"with the estate.", r.Expr)
	}
	// The multiplier IS the threshold. 0.9 catches the measured 13.3% event; 0.8 does not.
	if !strings.Contains(r.Expr, "0.9") {
		t.Errorf("EstateGraphCollapsed is %q. The threshold must be 10%% (× 0.9): measured churn is "+
			"<=0.9%% and the real event this ticket describes was a 13.3%% drop, so a looser line (0.8, "+
			"i.e. 20%%) is silent on exactly the incident that motivated the rule.", r.Expr)
	}
}

// TestTheTotalAndPartialCollapseRulesBothSurvive. The partial rule does not replace EstateGraphEmpty:
// a graph that goes straight to zero never establishes a peak-relative drop from a nonzero base if the
// exporter restarted with it, and "cannot refuse anything" deserves its own statement.
func TestTheTotalAndPartialCollapseRulesBothSurvive(t *testing.T) {
	var haveEmpty, haveCollapsed bool
	for _, r := range egressRules(t) {
		switch r.Alert {
		case "EstateGraphEmpty":
			haveEmpty = true
		case "EstateGraphCollapsed":
			haveCollapsed = true
		}
	}
	if !haveEmpty {
		t.Error("EstateGraphEmpty is gone — the partial-collapse rule is a companion, not a replacement; " +
			"an empty graph is the case where the mutation gate cannot refuse anything at all")
	}
	if !haveCollapsed {
		t.Error("EstateGraphCollapsed is gone — a partial collapse is unexpressible again")
	}
}
