package deploy

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ★ THE GAUGE MUST BE WATCHED, OR IT IS THE DEFECT AGAIN (TG-271).
//
// The host-diagnostic lane failed on 100% of its calls for weeks. Publishing a coverage number and not
// alerting on it would reproduce the ticket exactly one level up: the information exists, and nobody is
// told. Measured live 2026-08-06, 26 resolvable hosts still have no key.
func TestHostDiagnosticCoverageIsWatched(t *testing.T) {
	rules := loadHostdiagRules(t)

	reach, ok := rules["HostDiagnosticsCannotReachAlertedHosts"]
	if !ok {
		t.Fatal("no rule fires when TG cannot reach the hosts it is alerted about. That is the whole finding " +
			"of TG-271 — every diagnostic read failed and the only signal was a sentinel string in a tool result.")
	}
	expr, _ := reach["expr"].(string)
	if !strings.Contains(expr, "tg_hostdiag_hosts_uncovered_resolvable") {
		t.Errorf("the reach rule must read tg_hostdiag_hosts_uncovered_resolvable; expr = %q", expr)
	}
	// IT MUST NOT read the raw uncovered count. Alertmanager's host label carries k8s component names, so a
	// rule over covered-vs-alerted is red forever and gets silenced.
	if strings.Contains(expr, "tg_hostdiag_hosts_alerted") && !strings.Contains(expr, "uncovered_resolvable") {
		t.Error("the rule compares against the raw alerted count. Half of those labels are k8s components " +
			"(cilium-agent, coredns, node-exporter) that TG must never hold a key for — this fires forever " +
			"and trains the operator to ignore it.")
	}
	// Severity must stay warning while the config does not satisfy it. 26 hosts are outstanding; a critical
	// rule shipped now is a fail-closed gate turned on before its migration.
	if lbl, _ := reach["labels"].(map[string]any); lbl != nil {
		if sev, _ := lbl["severity"].(string); sev != "warning" {
			t.Errorf("severity = %q, want warning while hosts are still uncovered — a rule that pages on day "+
				"one against a known-unsatisfied config gets silenced and then never speaks again", sev)
		}
	}

	// THE VACUITY FLOOR.
	abs, ok := rules["HostDiagnosticCoverageUnmeasured"]
	if !ok {
		t.Fatal("no rule fires when coverage is not measured at all. The reach rule cannot fire on a series " +
			"that does not exist, so a blind diagnostic lane and a fully covered one are identical.")
	}
	if e, _ := abs["expr"].(string); !strings.Contains(e, "absent(") {
		t.Errorf("HostDiagnosticCoverageUnmeasured must be an absent() check; expr = %q", e)
	}
}

// The rules must name series cmd/worker actually emits — a rule over a misspelled metric is permanently
// silent and reads as coverage.
func TestHostdiagRulesNameSeriesTheCodeEmits(t *testing.T) {
	src, err := os.ReadFile("../cmd/worker/known_hosts_coverage.go")
	if err != nil {
		t.Fatalf("read known_hosts_coverage.go: %v", err)
	}
	emitted := string(src)
	for _, series := range []string{"tg_hostdiag_hosts_uncovered_resolvable", "tg_hostdiag_hosts_alerted"} {
		if !strings.Contains(emitted, series) {
			t.Errorf("the rules reference %q but cmd/worker/known_hosts_coverage.go does not emit it", series)
		}
	}
}

func loadHostdiagRules(t *testing.T) map[string]map[string]any {
	t.Helper()
	b, err := os.ReadFile("monitoring/alert.rules.yml")
	if err != nil {
		t.Fatalf("read alert.rules.yml: %v", err)
	}
	var doc struct {
		Groups []struct {
			Rules []map[string]any `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse alert.rules.yml: %v", err)
	}
	out := map[string]map[string]any{}
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			if n, _ := r["alert"].(string); n != "" {
				out[n] = r
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no rules parsed — every assertion would be vacuous")
	}
	return out
}
