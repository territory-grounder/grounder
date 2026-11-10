package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TG-667 (deep-review F6/F7). A HOLD is TG deciding not to heal. Ten of them sat in the ledger on 2026-11-07 and
// nothing alerted: an incident TG had chosen to leave alone looked, from monitoring, like one it had healed. The
// hold watch (cmd/worker/hold_watch.go) publishes the series; these rules make the operator look. promtool
// parses them but cannot see their intent — a threshold raised to a week, or a comparison inverted, both
// `check rules` cleanly. That is what this file is for.

type holdAlertRule struct {
	Alert, Expr, For, Severity string
}

func holdRules(t *testing.T) map[string]holdAlertRule {
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
				For    string            `yaml:"for"`
				Labels map[string]string `yaml:"labels"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse alert.rules.yml: %v", err)
	}
	total := 0
	out := map[string]holdAlertRule{}
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			total++
			if strings.Contains(r.Expr, "tg_hold_") || strings.Contains(r.Expr, "tg_failed_investigation_") {
				out[r.Alert] = holdAlertRule{r.Alert, strings.Join(strings.Fields(r.Expr), " "), r.For, r.Labels["severity"]}
			}
		}
	}
	if total < 20 {
		t.Fatalf("VACUITY FLOOR: parsed only %d alert rules — the file is not being read", total)
	}
	return out
}

func TestHoldAlertsWatchWhatTGDecidedNotToHeal(t *testing.T) {
	rules := holdRules(t)
	want := map[string]struct{ expr, severity, within string }{
		// The ACTIONABLE hold: the incident keeps raising (re-fired within the last hour, see hold_watch.go's
		// holdRefireRecent) and TG keeps declining. Two hours is long enough for the first re-detections to be
		// the flap of a single stop, and short enough that a person's change left in place is looked at the
		// same shift.
		"TGHeldIncidentStillRaising": {"tg_hold_redetected_oldest_seconds > 7200", "warning", "15m"},
		// TG tried, crashed, and never came back. Any count is a defect in the loop, not an estate state.
		"TGFailedInvestigationUnretried": {"tg_failed_investigation_unretried > 0", "warning", "30m"},
		// The vacuity floor as a rule: tg_hold_open is emitted at 0 on a quiet estate, so its absence is the
		// watcher being gone and every rule above going silent for the wrong reason.
		"TGHoldWatchUnmeasured": {"absent(tg_hold_open)", "warning", "15m"},
	}
	for name, w := range want {
		r, ok := rules[name]
		if !ok {
			t.Errorf("alert %s is missing — the hold watch publishes a series nobody reads", name)
			continue
		}
		if r.Expr != w.expr {
			t.Errorf("%s expr = %q, want %q", name, r.Expr, w.expr)
		}
		if r.Severity != w.severity {
			t.Errorf("%s severity = %q, want %q", name, r.Severity, w.severity)
		}
		if r.For != w.within {
			t.Errorf("%s `for` = %q, want %q — a single scrape must not page, and a shortened window is a louder rule nobody ruled on", name, r.For, w.within)
		}
	}
	for name := range rules {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected hold rule %s — add it to this oracle so its intent is pinned too", name)
		}
	}
}

// The series the rules read must be the series the job emits — a renamed gauge leaves a permanently silent
// rule. The producer is pinned by name in cmd/worker/hold_watch.go.
func TestHoldAlertSeriesAreProducedByTheHoldWatch(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "cmd", "worker", "hold_watch.go"))
	if err != nil {
		t.Fatalf("read hold_watch.go: %v", err)
	}
	for _, name := range []string{"tg_hold_open", "tg_hold_redetected_oldest_seconds", "tg_failed_investigation_unretried"} {
		if !strings.Contains(string(src), `"`+name+`"`) {
			t.Errorf("cmd/worker/hold_watch.go does not emit %q, which alert.rules.yml reads", name)
		}
	}
}
