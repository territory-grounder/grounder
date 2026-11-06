package skillgen

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/judge"
)

// tierCapCompleter captures the model tier the gate sends and returns a parseable triage JSON.
type tierCapCompleter struct{ lastModel string }

func (c *tierCapCompleter) Complete(_ context.Context, _, modelName string, _ []model.Message) (string, error) {
	c.lastModel = modelName
	return `{"diagnosis":"d","propose_action":false,"proposed_op":"","evidence":[],"prediction":"","conclusion":"c"}`, nil
}

// The offline PRE-FILTER runs on the low-latency "fast" tier by default — never the "primary" reasoning
// flagship whose ~50s judge-sized latency times the whole admission out (proven live: context deadline
// exceeded). A zero-value runner also falls back to fast; an explicit tier is honored and actually sent.
func TestOfflineGateUsesFastTierByDefault(t *testing.T) {
	if got := DefaultOfflineConfig().Model; got != "fast" {
		t.Fatalf("default offline model must be 'fast', got %q", got)
	}
	if got := (OfflineRunner{}).modelTier(); got != "fast" {
		t.Fatalf("zero-config modelTier must be 'fast' (never the timeout-prone primary), got %q", got)
	}
	if got := (OfflineRunner{Cfg: OfflineConfig{Model: "primary"}}).modelTier(); got != "primary" {
		t.Fatalf("an explicit tier must be respected, got %q", got)
	}
	cap := &tierCapCompleter{}
	r := OfflineRunner{Model: cap, Cfg: OfflineConfig{Model: "fast"}}
	if _, err := r.triage(context.Background(), "body", judge.TriageRow{ExternalRef: "i1", AlertRule: "R", Host: "h", Band: "AUTO"}); err != nil {
		t.Fatalf("triage: %v", err)
	}
	if cap.lastModel != "fast" {
		t.Fatalf("triage sent model %q to the gateway, want 'fast'", cap.lastModel)
	}
}

// A fail-closed on too-few-cleanly-scored incidents must be DIAGNOSABLE: when every incident errored,
// the detail has to carry the reason, otherwise model flakiness, a parse bug, and a thin corpus all look
// identical in eval_offline (no-silent-caps). A clean shortfall (no errors) stays terse.
func TestScoringShortfallSurfacesReason(t *testing.T) {
	if got := scoringShortfall(2, 5, 0, ""); got != "only 2/5 incidents scored cleanly (fail closed)" {
		t.Fatalf("a clean shortfall must stay terse, got %q", got)
	}
	got := scoringShortfall(0, 2, 2, "gateway 503: model timeout")
	for _, want := range []string{"0/2", "2 scoring error(s)", "gateway 503"} {
		if !strings.Contains(got, want) {
			t.Fatalf("shortfall must surface %q, got %q", want, got)
		}
	}
}

// The surfaced reason is bounded rune-safely: a long or multibyte error never splits a rune (the value
// rides into a json.RawMessage eval_offline blob and must stay valid UTF-8).
func TestTruncateReasonRuneSafe(t *testing.T) {
	if got := truncateReason("  short reason  "); got != "short reason" {
		t.Fatalf("short reason should pass through trimmed, got %q", got)
	}
	long := truncateReason(strings.Repeat("x", 500))
	if utf8.RuneCountInString(long) > 203 { // 200 runes + "..."
		t.Fatalf("reason not bounded: %d runes", utf8.RuneCountInString(long))
	}
	if got := truncateReason(strings.Repeat("é", 300)); !utf8.ValidString(got) {
		t.Fatalf("multibyte truncation produced invalid utf-8")
	}
}

// The single-shot triage parser is defensive: a proposal and a grounded stop map to distinct judgeable
// sessions, and garbage is an honestly-empty stopped session (never a fabricated decision).
func TestParseTriage(t *testing.T) {
	inc := judge.TriageRow{ExternalRef: "inc-1", AlertRule: "DeviceDown", Host: "h1", Band: "AUTO_NOTICE"}

	prop := parseTriage(inc, `here you go: {"diagnosis":"link flap","propose_action":true,"proposed_op":"restart-iface","evidence":["e1","e2"],"prediction":"iface up in 60s","conclusion":""}`)
	if !prop.Proposed || prop.Op != "restart-iface" || prop.Outcome != "proposed" {
		t.Fatalf("proposal not parsed: %+v", prop)
	}
	if !prop.Predicted || len(prop.Evidence) != 2 {
		t.Fatalf("prediction/evidence not carried: %+v", prop)
	}
	if prop.Conclusion != "link flap" {
		t.Fatalf("diagnosis should ride into conclusion on a proposal, got %q", prop.Conclusion)
	}
	if prop.Band != "AUTO_NOTICE" {
		t.Fatalf("the incident's original band must ride through, got %q", prop.Band)
	}

	stop := parseTriage(inc, `{"diagnosis":"device admin-disabled","propose_action":false,"proposed_op":"","evidence":["e9"],"prediction":"","conclusion":"stale alert, DISABLED in NetBox"}`)
	if stop.Proposed || stop.Outcome != "stopped" || stop.Predicted {
		t.Fatalf("grounded stop not parsed: %+v", stop)
	}

	garbage := parseTriage(inc, "the model rambled with no json")
	if garbage.Proposed || garbage.Outcome != "unparseable" {
		t.Fatalf("garbage must be an honestly-empty session, got %+v", garbage)
	}
}
