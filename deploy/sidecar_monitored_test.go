package deploy

// TG'S PRODUCTION BRAIN MUST BE SCRAPED, AND THE RULE MUST HAVE A TARGET (TG-231).
//
// Since 2026-07-31 TG runs SINGLE-BRAIN on Opus 5 via the tg-opus-sidecar, deliberately with no fallback:
// an outage fails triage loudly rather than silently swapping brains. Measured 2026-08-06, Prometheus
// scraped four targets — grounder, worker, worker-actuate, prometheus — and none of them was the brain.
//
// The pairing is the point. An alert on `up{job="opus-sidecar"}` with no scrape job for that name can
// NEVER fire: the series simply does not exist, the rule is permanently silent, and a dashboard shows a
// rule that looks like coverage. That is this repo's standing failure shape applied to monitoring config,
// and it is invisible to any check that reads only one of the two files.

import (
	"os"
	"strings"
	"testing"
)

func monitoringFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("monitoring/" + name)
	if err != nil {
		t.Fatalf("read monitoring/%s: %v — this guard cannot assert anything about a file it cannot open", name, err)
	}
	if len(b) == 0 {
		t.Fatalf("monitoring/%s is empty", name)
	}
	return string(b)
}

// stripYAMLCommentLines drops whole-line comments so a scan for configuration cannot match the prose that
// explains it — the trap that made an earlier guard in this package fail on its own comment block.
func stripYAMLCommentLines(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func TestTheSidecarIsScrapedAndItsRuleHasATarget(t *testing.T) {
	scrape := stripYAMLCommentLines(monitoringFile(t, "prometheus.yml"))
	rules := stripYAMLCommentLines(monitoringFile(t, "alert.rules.yml"))

	if !strings.Contains(scrape, "opus-sidecar") {
		t.Fatal("prometheus.yml has no opus-sidecar scrape job. TG runs single-brain with NO fallback by " +
			"design, so this container failing means triage stops entirely — and it was unmonitored for " +
			"the whole of the period TG-231 describes.")
	}
	if !strings.Contains(scrape, "8094") {
		t.Error("the opus-sidecar job names no port; the sidecar serves /metrics on 8094")
	}

	// THE PAIRING. A rule whose series has no producer is permanently silent and looks like coverage.
	if !strings.Contains(rules, "SidecarDown") {
		t.Fatal("no SidecarDown alert — the scrape would collect metrics nobody is alerted on")
	}
	if !strings.Contains(rules, `up{job="opus-sidecar"}`) {
		t.Error(`the sidecar rules do not select up{job="opus-sidecar"}; a rule selecting a job name that ` +
			"no scrape config produces can never fire")
	}
	// The vacuity floor for the pair itself: `up` exists for every configured target, so an ABSENT series
	// means the job was removed — and SidecarDown is silent exactly then.
	if !strings.Contains(rules, "SidecarUnmonitored") {
		t.Error("no SidecarUnmonitored rule. SidecarDown cannot fire when the scrape job is gone, so " +
			"removing the job would silence the critical alert without anything saying so.")
	}
}

// The TG-side dead-man rides series that already exist (TG-221's per-tier model breakers). It must select
// ONLY the model tiers — folding in the governance breakers would page the wrong person for the wrong
// fault, and those have their own rules and meanings.
func TestTheModelBreakerAlertDoesNotSwallowTheGovernanceBreakers(t *testing.T) {
	rules := stripYAMLCommentLines(monitoringFile(t, "alert.rules.yml"))

	if !strings.Contains(rules, "ModelBreakerOpen") {
		t.Fatal("no ModelBreakerOpen alert — litellm has no fallback to mask a failing tier, so a tripped " +
			"breaker is 'N consecutive triage LLM calls failed' and it was only ever logged")
	}
	if !strings.Contains(rules, `circuit_breaker_state{name=~"model-.*"}`) {
		t.Error("ModelBreakerOpen does not scope its selector to the model tiers")
	}
	// The mutation breaker keeps its own dedicated rule; this one must not also match it.
	if strings.Contains(rules, `circuit_breaker_state{name=~".*"}`) {
		t.Error("a rule selects EVERY breaker by wildcard — the mutation and judge-death breakers mean " +
			"different things, have their own rules, and would page the wrong person under this summary")
	}
}
