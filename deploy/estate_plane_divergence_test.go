package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TG-346. `EstateGraphEmpty` fires on tg_estate_edges == 0. The state that actually occurred — and is
// still live — is not zero:
//
//	tg_estate_edges{job="worker"}          392    nodes 369
//	tg_estate_edges{job="worker-actuate"}   17    nodes  20
//	tg_estate_sources_failed                 0 on BOTH
//
// The actuation plane models 4% of the estate the triage plane models and NOTHING is erroring, so every
// existing signal reads healthy. TG-343's gauge found this within minutes of shipping and then had no
// rule that could fire on it.

func divergenceRule(t *testing.T) (expr string, found bool) {
	t.Helper()
	for _, r := range breakerAlertRulesAll(t) {
		if r.Alert == "EstateGraphDivergesBetweenPlanes" {
			return strings.Join(strings.Fields(r.Expr), " "), true
		}
	}
	return "", false
}

// breakerAlertRulesAll returns EVERY rule in the file. Named distinctly from breakerAlertRules in the
// sibling test, which filters to circuit_breaker_state — six duplicate-helper collisions in this repo in
// one day, one of which reddened main.
func breakerAlertRulesAll(t *testing.T) []struct{ Alert, Expr string } {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("monitoring", "alert.rules.yml"))
	if err != nil {
		t.Fatalf("read alert.rules.yml: %v", err)
	}
	var doc alertDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse alert.rules.yml: %v", err)
	}
	var all []struct{ Alert, Expr string }
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			all = append(all, struct{ Alert, Expr string }{r.Alert, r.Expr})
		}
	}
	if len(all) < 20 {
		t.Fatalf("VACUITY FLOOR: parsed only %d alert rules — every assertion below would pass on an "+
			"unread file", len(all))
	}
	return all
}

func TestThePlaneDivergenceIsAlertable(t *testing.T) {
	expr, ok := divergenceRule(t)
	if !ok {
		t.Fatal("no EstateGraphDivergesBetweenPlanes rule. EstateGraphEmpty only fires at ZERO, so the " +
			"live 17-vs-392 split — with 0 source failures on both planes — is invisible to every rule in " +
			"the file. That is the exact state TG-343's gauge was built to surface and could not alert on.")
	}

	// It must compare ACROSS planes, so it cannot be pinned to one job.
	if strings.Contains(expr, `job="worker"`) || strings.Contains(expr, `job="worker-actuate"`) {
		t.Errorf("the divergence rule pins a job: %q. Pinning one plane cannot express \"these two "+
			"disagree\", and it silently stops covering a plane that is renamed or added.", expr)
	}
	// A RATIO, not an absolute edge difference: the estate grows, and a fixed edge gap needs re-tuning
	// every time it does — a threshold nobody re-tunes is a threshold that stops firing.
	if !strings.Contains(expr, "/") {
		t.Errorf("the divergence rule uses no ratio: %q. An absolute difference has to be re-tuned as the "+
			"estate grows; a ratio does not.", expr)
	}
	// 0/0 is NaN and would make the rule undefined exactly when both planes are broken.
	if !strings.Contains(expr, "> 0") {
		t.Errorf("the divergence rule does not guard against a zero denominator: %q. With both planes at "+
			"0 edges the ratio is NaN and the rule is silent precisely when the estate is gone.", expr)
	}
}

// TestTheZeroCaseIsStillCovered. The divergence rule is an ADDITION: with both planes at 0 the ratio is
// guarded away, so EstateGraphEmpty is the only thing standing between an empty estate and silence.
func TestTheZeroCaseIsStillCovered(t *testing.T) {
	var haveEmpty bool
	for _, r := range breakerAlertRulesAll(t) {
		if r.Alert == "EstateGraphEmpty" {
			haveEmpty = true
		}
	}
	if !haveEmpty {
		t.Error("EstateGraphEmpty is gone. The divergence rule deliberately excludes the both-planes-zero " +
			"case (max > 0), so removing the zero rule leaves a totally empty estate unalerted.")
	}
}
