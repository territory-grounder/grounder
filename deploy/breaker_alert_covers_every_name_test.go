package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TG-397. `circuit_breaker_state`'s label set is OPEN and its alert selectors were CLOSED.
//
// cmd/worker/admin.go emits one series for the "mutation" breaker plus one for EVERY ROW in the shared
// breaker store, so a name enters the metric the moment anything writes a row —
// core/governance/judge_liveness.go does exactly that with "judge-death". The rules selected
// name="mutation" and name=~"model-.*".
//
// Measured live 2026-08-06, five breakers were publishing: mutation, model-primary, model-fast,
// model-embed-nomic, judge-death. The last matched NEITHER rule, so a trip would have fired nothing.
//
// The durable property is not "judge-death is covered" — adding it to the list would leave the next name
// uncovered exactly the same way. It is that AT LEAST ONE rule selects the metric with no name constraint,
// so the selector cannot fall behind a producer that nobody controls.

type alertDoc struct {
	Groups []struct {
		Rules []struct {
			Alert string `yaml:"alert"`
			Expr  string `yaml:"expr"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

func breakerAlertRules(t *testing.T) []struct{ Alert, Expr string } {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("monitoring", "alert.rules.yml"))
	if err != nil {
		t.Fatalf("read alert.rules.yml: %v", err)
	}
	var doc alertDoc
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse alert.rules.yml: %v", err)
	}
	var total int
	var out []struct{ Alert, Expr string }
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			total++
			if strings.Contains(r.Expr, "circuit_breaker_state") {
				out = append(out, struct{ Alert, Expr string }{r.Alert, r.Expr})
			}
		}
	}
	if total < 20 {
		t.Fatalf("VACUITY FLOOR: parsed only %d alert rules — the file is not being read properly and every "+
			"assertion below would pass on an empty set", total)
	}
	if len(out) == 0 {
		t.Fatal("no alert rule mentions circuit_breaker_state at all — every breaker in the system, " +
			"including the mutation breaker, would trip in silence")
	}
	return out
}

// nameSelector matches a `{...name=...}` constraint on the metric.
var nameSelector = regexp.MustCompile(`circuit_breaker_state\s*\{[^}]*name\s*(=|=~|!=|!~)`)

// TestSomeBreakerRuleHasNoNameSelector is the finding. A closed selector over an open producer means the
// rule set silently misses whatever name it did not anticipate.
func TestSomeBreakerRuleHasNoNameSelector(t *testing.T) {
	rules := breakerAlertRules(t)

	var catchAll []string
	for _, r := range rules {
		// A rule is name-agnostic when its circuit_breaker_state reference carries no name constraint.
		if !nameSelector.MatchString(r.Expr) {
			catchAll = append(catchAll, r.Alert)
		}
	}
	if len(catchAll) == 0 {
		var listed []string
		for _, r := range rules {
			listed = append(listed, r.Alert+": "+strings.Join(strings.Fields(r.Expr), " "))
		}
		t.Errorf("every circuit_breaker_state rule pins a specific name, so a breaker whose name nobody "+
			"anticipated trips in SILENCE.\n"+
			"The metric's label set is open — admin.go publishes one series per row in the shared breaker "+
			"store — so a hand-written selector is guaranteed to fall behind it. That is how `judge-death` "+
			"came to be unalertable while four other breakers were covered.\n"+
			"rules found:\n  %s", strings.Join(listed, "\n  "))
	}
}

// TestTheCatchAllStillTestsForOpen guards the lazy way to satisfy the test above: a rule that selects the
// metric with no name AND no meaningful condition would pass while alerting on nothing (or on everything).
func TestTheCatchAllStillTestsForOpen(t *testing.T) {
	rules := breakerAlertRules(t)

	var checked bool
	for _, r := range rules {
		if nameSelector.MatchString(r.Expr) {
			continue
		}
		checked = true
		flat := strings.Join(strings.Fields(r.Expr), " ")
		if !strings.Contains(flat, "== 2") {
			t.Errorf("%s selects every breaker but does not test for the OPEN state (== 2): %q. "+
				"0 is closed and 1 is half-open; a rule that fires on those is noise that will be muted, "+
				"and muting it removes the only name-agnostic coverage there is.", r.Alert, flat)
		}
	}
	if !checked {
		t.Fatal("VACUITY FLOOR: no name-agnostic rule was examined — the test above reports that")
	}
}

// TestTheSpecificRulesSurvive. The catch-all does not replace them: `mutation` is critical and carries its
// own runbook, and deleting the specific rules in favour of one generic warning would downgrade the
// severity of the breaker that stops the estate being mutated.
func TestTheSpecificRulesSurvive(t *testing.T) {
	rules := breakerAlertRules(t)

	var haveMutation bool
	for _, r := range rules {
		if strings.Contains(r.Expr, `name="mutation"`) {
			haveMutation = true
		}
	}
	if !haveMutation {
		t.Error("no rule selects the mutation breaker specifically. The name-agnostic rule is a floor, not " +
			"a replacement — the mutation breaker is the one that stops the estate being mutated and it " +
			"carries a critical severity and its own runbook.")
	}
}
