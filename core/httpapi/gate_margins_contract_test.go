package httpapi

import (
	"encoding/json"
	"testing"
)

// THE CONSOLE WILL READ THESE FIELD NAMES (TG-178, the within-ε gate boundary-case queue). Renaming one
// silently blanks the surface — the exact failure mode memorialised in session_detail_contract_test.go and
// deploy/console/v2/e2e/reasoning.mjs: a view and its own e2e can agree on an IMAGINED DTO while the server
// sends different names, so the surface falls back to a fixture on every load and the e2e stays green. This
// test is the one place the Go producer and the JSON the console will consume meet, so it is the only place a
// rename is caught before it ships. It asserts the JSON KEY SET (what crosses the boundary), not the Go field
// names. Landed with the API (!1268) so the contract is pinned BEFORE the console consumer is written against
// it — the sequencing the reasoning.mjs post-mortem wishes it had.
func TestGateBoundaryDTOKeysTheConsoleWillDependOn(t *testing.T) {
	// Populated: json.Marshal drops omitempty fields when zero, so a zero value would "prove" the contract by
	// emitting almost nothing.
	page := GateBoundaryPage{
		Epsilon: 0.05,
		Cases: []GateBoundaryCase{{
			ActionID: "9a8cca11", ExternalRef: "librenms-dc1-181284", Ordinal: 7,
			Gate: "policy", Verdict: "pass", Reason: "confidence 0.59 < min_confidence 0.60",
			Margin: -0.01,
		}},
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("unmarshal top: %v", err)
	}
	var cases []map[string]json.RawMessage
	if err := json.Unmarshal(top["cases"], &cases); err != nil {
		t.Fatalf("unmarshal cases: %v", err)
	}
	if len(cases) != 1 {
		t.Fatalf("want 1 case, got %d", len(cases))
	}

	// Each entry names the consumer, so a failure says WHAT breaks rather than only that something did.
	for _, c := range []struct{ key, usedBy string }{
		{"epsilon", "the within-ε queue header — the band the set was selected on (never assume the default)"},
		{"cases", "the boundary-case list itself"},
	} {
		if _, present := top[c.key]; !present {
			t.Errorf("GateBoundaryPage no longer serializes %q — used by %s. Present keys: %s", c.key, c.usedBy, keysOf(top))
		}
	}
	for _, c := range []struct{ key, usedBy string }{
		{"action_id", "the action a boundary decision belongs to (joins the decision-tracer walk)"},
		{"gate", "which gate decided within ε"},
		{"verdict", "the pass/refuse cell"},
		{"margin", "the signed distance-to-threshold — the whole point of the queue (0 is a REAL at-threshold value, never 'unknown')"},
		{"ordinal", "the gate's position in the interceptor chain"},
	} {
		if _, present := cases[0][c.key]; !present {
			t.Errorf("GateBoundaryCase no longer serializes %q — used by %s. Present keys: %s", c.key, c.usedBy, keysOf(cases[0]))
		}
	}
	// external_ref and reason are omitempty but MUST serialize when populated (the console shows the incident + why).
	for _, c := range []struct{ key, usedBy string }{
		{"external_ref", "the incident the action answers (the correlation key that joins the walk)"},
		{"reason", "the human-readable decision reason"},
	} {
		if _, present := cases[0][c.key]; !present {
			t.Errorf("GateBoundaryCase drops %q even when populated — used by %s", c.key, c.usedBy)
		}
	}
	// Ghost names: the margin is a signed DISTANCE, not a probability. If it were ever renamed to one of these,
	// a console consumer reading the wrong key would half-work — worse than failing, because a partly-populated
	// queue reads as a complete one.
	for _, ghost := range []string{"confidence", "conf", "min_confidence", "threshold", "value", "score"} {
		if _, present := cases[0][ghost]; present {
			t.Errorf("GateBoundaryCase now serializes %q — the margin is a signed distance-to-threshold, not that; name it deliberately", ghost)
		}
	}
}
