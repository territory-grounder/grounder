package deploy

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ★ A STALE TRIP MUST BE ALERTABLE (TG-347).
//
// judge-death read OPEN on both planes for days on a demonstrably live judge, refusing every skill-trial
// graduation the whole time. circuit_breaker_state said OPEN and nothing said "for three days", so no rule
// could tell a latch from a fresh trip.
func TestALatchedBreakerIsWatched(t *testing.T) {
	rules := loadLatchRules(t)

	r, ok := rules["CircuitBreakerLatchedOpen"]
	if !ok {
		t.Fatal("no rule fires on a breaker that has been open for a long time. A latch that outlives its " +
			"cause is invisible: the state gauge reads OPEN identically at one minute and at three days.")
	}
	expr, _ := r["expr"].(string)
	if !strings.Contains(expr, "circuit_breaker_opened_at_seconds") {
		t.Errorf("the rule does not read circuit_breaker_opened_at_seconds; expr = %q", expr)
	}
	// It must measure ELAPSED time, not compare the state gauge — otherwise it fires the moment anything
	// trips and is just a duplicate of the per-name breaker alerts.
	if !strings.Contains(expr, "time()") {
		t.Errorf("the rule does not measure elapsed time against now(); expr = %q — without that it fires on "+
			"every fresh trip and duplicates ModelBreakerOpen", expr)
	}
}

// The gauge the rule reads must be the one the worker actually emits.
func TestTheLatchRuleNamesASeriesTheWorkerEmits(t *testing.T) {
	src, err := readFile("../cmd/worker/admin.go")
	if err != nil {
		t.Fatalf("read admin.go: %v", err)
	}
	// The QUOTED name, not a substring: renaming the emit to "circuit_breaker_opened_at_seconds_DISABLED"
	// still contains the bare string, and that mutation passed this assertion until it was quoted.
	if !strings.Contains(src, `"circuit_breaker_opened_at_seconds"`) {
		t.Error("the rule references circuit_breaker_opened_at_seconds but cmd/worker/admin.go does not emit " +
			"it — a rule over a series nothing publishes is permanently silent and reads as coverage")
	}
	// And it must stay conditional on the breaker being open: an unconditional emit exports epoch-zero for
	// every closed breaker, which makes this rule fire forever and get silenced.
	if !strings.Contains(src, "!rec.OpenedAt.IsZero()") {
		t.Error("the opened-at emit is no longer guarded by !rec.OpenedAt.IsZero(). OpenedAt is zero unless " +
			"the breaker is open, so an unconditional emit publishes 1970 for healthy breakers and " +
			"CircuitBreakerLatchedOpen then fires on all of them")
	}
}

func readFile(p string) (string, error) { b, err := os.ReadFile(p); return string(b), err }

// loadLatchRules parses alert.rules.yml into name -> rule. It FAILS on an empty parse rather than returning
// an empty map: a map with no rules satisfies every not-contains assertion and would report coverage for a
// file nobody managed to read.
func loadLatchRules(t *testing.T) map[string]map[string]any {
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
		t.Fatal("no rules parsed — every assertion below would be vacuous")
	}
	return out
}
