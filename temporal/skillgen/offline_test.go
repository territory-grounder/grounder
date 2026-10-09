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

// The gate math: admit only when the target dimension improves AND no other dimension regresses beyond
// slack (REQ-1307). AdmitToTrial reads RegressionPass && DiscoveryDelta > 0.
func TestOfflineDecision(t *testing.T) {
	cases := []struct {
		name          string
		cand, prod    map[string][]float64
		dim           string
		slack         float64
		wantPass      bool
		wantDeltaSign int // -1, 0, +1
	}{
		{
			name: "improves target, no regression -> admit",
			cand: map[string][]float64{"correct_diagnosis": {4, 4, 4}, "appropriate_band": {5, 5}},
			prod: map[string][]float64{"correct_diagnosis": {3, 3, 3}, "appropriate_band": {5, 5}},
			dim:  "correct_diagnosis", slack: 0.25, wantPass: true, wantDeltaSign: +1,
		},
		{
			name: "improves target but regresses the safety analog -> refuse",
			cand: map[string][]float64{"correct_diagnosis": {5, 5}, "appropriate_band": {3, 3}},
			prod: map[string][]float64{"correct_diagnosis": {3, 3}, "appropriate_band": {5, 5}},
			dim:  "correct_diagnosis", slack: 0.25, wantPass: false, wantDeltaSign: +1,
		},
		{
			name: "no target improvement -> delta not positive (not admitted even if regression holds)",
			cand: map[string][]float64{"correct_diagnosis": {3, 3}, "appropriate_band": {5, 5}},
			prod: map[string][]float64{"correct_diagnosis": {3, 3}, "appropriate_band": {5, 5}},
			dim:  "correct_diagnosis", slack: 0.25, wantPass: true, wantDeltaSign: 0,
		},
		{
			name: "a small dip within slack does not count as a regression",
			cand: map[string][]float64{"correct_diagnosis": {4, 4}, "evidence_grounded": {3.8, 3.8}},
			prod: map[string][]float64{"correct_diagnosis": {3, 3}, "evidence_grounded": {4, 4}},
			dim:  "correct_diagnosis", slack: 0.25, wantPass: true, wantDeltaSign: +1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := OfflineDecision(tc.cand, tc.prod, tc.dim, tc.slack)
			if res.RegressionPass != tc.wantPass {
				t.Fatalf("RegressionPass = %v, want %v (detail: %s)", res.RegressionPass, tc.wantPass, res.Detail)
			}
			gotSign := 0
			if res.DiscoveryDelta > 0 {
				gotSign = +1
			} else if res.DiscoveryDelta < 0 {
				gotSign = -1
			}
			if gotSign != tc.wantDeltaSign {
				t.Fatalf("DiscoveryDelta sign = %d (%.3f), want %d", gotSign, res.DiscoveryDelta, tc.wantDeltaSign)
			}
			admitted := res.RegressionPass && res.DiscoveryDelta > 0
			wantAdmit := tc.wantPass && tc.wantDeltaSign > 0
			if admitted != wantAdmit {
				t.Fatalf("admit gate = %v, want %v", admitted, wantAdmit)
			}
		})
	}
}

// A dimension the judge scored on ONLY one arm never manufactures a phantom regression.
func TestOfflineDecisionIgnoresOneSidedDimensions(t *testing.T) {
	cand := map[string][]float64{"correct_diagnosis": {4, 4}}
	prod := map[string][]float64{"correct_diagnosis": {3, 3}, "appropriate_band": {5, 5}}
	res := OfflineDecision(cand, prod, "correct_diagnosis", 0.25)
	if !res.RegressionPass {
		t.Fatalf("a dimension present for only one arm must not regress the gate, got fail: %s", res.Detail)
	}
	if res.DiscoveryDelta <= 0 {
		t.Fatalf("target improved, want positive delta, got %.3f", res.DiscoveryDelta)
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
