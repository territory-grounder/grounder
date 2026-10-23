package librenms

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// testSlugRe mirrors core/ingest.slugRe — the join-safe alert_rule charset the core validator enforces.
// A rule slug that fails this is rejected with HTTP 400 before triage.
var testSlugRe = regexp.MustCompile(`^[A-Za-z0-9._:@/+-]+$`)

// TestSlugifyRuleProducesValidSlug: standard LibreNMS default alert rules carry % > < = ',' and previously
// slugged to an invalid string (whitespace-only collapse), 400'ing the most common infra alerts at ingest.
func TestSlugifyRuleProducesValidSlug(t *testing.T) {
	rules := []string{
		"Space on / is >= 90% and < 95% in use",
		"Processor usage over 85%",
		"Linux High Memory Usage, >= 90% in use",
		"Sensor over limit - Check Device Health Settings",
		"Devices up/down",
		"Service up/down",
		"BGP Session (>= 1) down",
		"ifOperStatus != up",
	}
	for _, r := range rules {
		got := SlugifyRule(r)
		if got == "" {
			t.Errorf("SlugifyRule(%q) is empty — the alert would be rejected", r)
			continue
		}
		if !testSlugRe.MatchString(got) {
			t.Errorf("SlugifyRule(%q) = %q — not a valid alert_rule slug (would 400 at ingest)", r, got)
		}
	}
	// Exact shape: whitespace + out-of-charset runs collapse to one hyphen; no leading/trailing hyphen.
	if got := SlugifyRule("Space on / is >= 90% and < 95% in use"); got != "Space-on-/-is-90-and-95-in-use" {
		t.Errorf("disk-rule slug = %q, want Space-on-/-is-90-and-95-in-use", got)
	}
	// A rule that is all special chars / whitespace slugs to empty (caller falls back to title, then the
	// core validator rejects an empty alert_rule — fail-closed, unchanged).
	if got := SlugifyRule("  >>> === %%% "); got != "" {
		t.Errorf("all-special rule must slug to empty, got %q", got)
	}
}

// TestRecoveryTransitionLabelled: a state-0 (recovery/up) LibreNMS push is labelled transition=recovery on
// the envelope so the front door can route it as positive clear-evidence (spec/012 clear-confirm) instead of
// re-triaging it; a state-1 (fault) push carries no such label. Both share the same external_ref (LibreNMS
// reuses the alert id across its lifecycle), which is what lets the recovery reach the waiting session.
func TestRecoveryTransitionLabelled(t *testing.T) {
	fixed := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	m := New([]Deployment{{Site: "NL"}}, WithClock(func() time.Time { return fixed }))
	labelsFor := func(state int) map[string]string {
		body := fmt.Sprintf(`{"site":"NL","id":"42","rule":"Devices up/down","severity":"critical",`+
			`"host":"192.0.2.1","hostname":"web01","title":"web01 down","timestamp":"2026-07-23 12:00:00","state":%d}`, state)
		env, err := m.Normalize(context.Background(), []byte(body))
		if err != nil {
			t.Fatalf("state=%d: Normalize returned error: %v", state, err)
		}
		return env.Labels
	}
	if got := labelsFor(1)[coreingest.LabelTransition]; got != "" {
		t.Fatalf("a fault (state=1) envelope must carry NO transition label, got %q", got)
	}
	if got := labelsFor(0)[coreingest.LabelTransition]; got != coreingest.TransitionRecovery {
		t.Fatalf("a recovery (state=0) envelope must carry transition=recovery, got %q", got)
	}
}
