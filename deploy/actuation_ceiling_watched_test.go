package deploy

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ★ THE CEILING NOBODY WATCHED (TG-286, found by TG-339, measured again 2026-08-06).
//
// core/actuate/limiter.go is the last-resort bound on how much of production TG may change in a window.
// TG-339 found it genuinely wired at interceptor.go:689 and publishing NO series at all. The four series
// now exist on both planes — and a grep of alert.rules.yml for any of them returned ZERO. A bound that
// nothing alerts on is a number on a dashboard, which is this board category's whole point.
func TestTheActuationCeilingIsWatched(t *testing.T) {
	rules := loadAlertRules(t)

	byName := map[string]map[string]any{}
	for _, r := range rules {
		if n, _ := r["alert"].(string); n != "" {
			byName[n] = r
		}
	}

	hit, ok := byName["ActuationCeilingHit"]
	if !ok {
		t.Fatal("no rule fires when TG refuses its own actuation. tg_actuation_refused_total exists on both " +
			"planes and nothing reads it, so the agent looping on one target looks identical to a quiet estate.")
	}
	if e, _ := hit["expr"].(string); !strings.Contains(e, "tg_actuation_refused_total") {
		t.Errorf("ActuationCeilingHit does not read tg_actuation_refused_total; expr = %q", e)
	}

	// THE VACUITY FLOOR. Without it, the limiter disappearing entirely — the exact state TG-339 measured —
	// makes every rule above it silent, and silence reads as a bounded system.
	abs, ok := byName["ActuationCeilingUnpublished"]
	if !ok {
		t.Fatal("no rule fires when the actuation ceiling stops being published. ActuationCeilingHit cannot " +
			"fire on a series that does not exist, so its silence would mean 'no refusals' and 'no limiter' " +
			"identically.")
	}
	if e, _ := abs["expr"].(string); !strings.Contains(e, "absent(") || !strings.Contains(e, "tg_actuation_limit") {
		t.Errorf("ActuationCeilingUnpublished must be an absent() check over tg_actuation_limit; expr = %q", e)
	}
}

// Both rules must sit on series the code actually emits. A rule over a misspelled metric is worse than no
// rule: it is permanently silent and looks like coverage.
func TestTheCeilingRulesNameSeriesTheCodeEmits(t *testing.T) {
	// The limiter's series are emitted from the worker admin surface, not the shared families table —
	// checked by locating them where they are actually written rather than where a reader might assume.
	src, err := os.ReadFile("../cmd/worker/admin.go")
	if err != nil {
		t.Fatalf("read cmd/worker/admin.go: %v", err)
	}
	families := string(src)
	for _, series := range []string{"tg_actuation_limit", "tg_actuation_refused_total"} {
		if !strings.Contains(families, series) {
			t.Errorf("the rules reference %q but cmd/worker/admin.go does not emit it — the rule "+
				"would never fire and would read as coverage", series)
		}
	}
}

func loadAlertRules(t *testing.T) []map[string]any {
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
	var out []map[string]any
	for _, g := range doc.Groups {
		out = append(out, g.Rules...)
	}
	if len(out) == 0 {
		t.Fatal("no rules parsed from alert.rules.yml — every assertion below would be vacuous")
	}
	return out
}
