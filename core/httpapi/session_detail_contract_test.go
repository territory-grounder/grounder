package httpapi

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// THE CONSOLE READS THESE FIELD NAMES. RENAMING ONE SILENTLY BLANKS A SURFACE.
//
// This test exists because of a defect that no other kind of test could catch. The live #reasoning view and
// its own e2e oracle were written together against an IMAGINED shape of this DTO — dto.external_ref,
// dto.steps[], step.kind, step.reason, step.label, step.confidence, step.tools[], dto.alert_rule. The server
// sends none of those names. The view guarded on `if(!dto.external_ref) return fixture()`, which was
// therefore ALWAYS true, so the surface fell back to its labelled fixture on every single load from the day
// it shipped, while the real trace sat fetched and parsed in memory beside it.
//
// The e2e oracle stayed green throughout, because it served its own invented payload and asserted the code
// agreed with it. Two artefacts sharing one wrong assumption prove nothing about the third — the server.
// This test is the only place the two languages meet, so it is the only place the assumption can be checked.
//
// It asserts the JSON KEY SET, not the Go field names, because JSON keys are what crosses the boundary. If
// you rename a field here, this fails and names the consumer, and you can update
// deploy/console/v2/modules/_live/js.txt in the same change instead of discovering it months later on a
// screen that looks perfectly fine.
func TestSessionDetailDTOKeysTheConsoleDependsOn(t *testing.T) {
	// A populated value: json.Marshal drops `omitempty` fields when zero, and a zero-valued struct would
	// therefore "prove" the contract by emitting almost nothing.
	dto := SessionDetailDTO{
		ID: "librenms-dc1-181284", Ref: "librenms-dc1-181284",
		Title: "Service-up/down", Host: "dc1actualbudget01",
		Band: "AUTO", Status: "executed", Risk: "high", Conf: 0.85,
		Action: "9a8cca11", Verdict: "match",
		Nodes: []SessionNodeDTO{{
			T: "agent-cycle", Lb: "ReAct cycle 1 — get-device-status", Ts: "2026-07-29T00:18:53Z",
			St: "investigate", Src: "agent-cycle",
			Pay:  "Investigating alert: Service-up/down. Step 1: get device status.",
			Plan: []string{"get-device-status"}, Gate: "execute", Band: "AUTO", Hash: "sha256:0228a1b4",
			Verdict: "investigate", Conf: 0.42, MinConf: 0.3,
		}},
	}

	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("unmarshal top: %v", err)
	}
	var nodes []map[string]json.RawMessage
	if err := json.Unmarshal(top["nodes"], &nodes); err != nil {
		t.Fatalf("unmarshal nodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("want 1 node, got %d", len(nodes))
	}

	// Each entry names the consumer, so a failure tells you what breaks rather than only that something did.
	for _, c := range []struct{ key, usedBy string }{
		{"ref", "_live/js.txt views.reasoning — the ADOPTION GUARD. If this key is absent or renamed, the " +
			"whole surface silently reverts to the labelled fixture. This is the exact field that was wrong."},
		{"title", "_live/js.txt views.reasoning header"},
		{"host", "_live/js.txt views.reasoning header"},
		{"nodes", "_live/js.txt liveReasonChain — the walk itself"},
		{"id", "console workflow run identity"},
		{"status", "console status pill"},
		{"verdict", "console verdict cell"},
	} {
		if _, present := top[c.key]; !present {
			t.Errorf("SessionDetailDTO no longer serializes %q — used by %s. Present keys: %s",
				c.key, c.usedBy, keysOf(top))
		}
	}

	for _, c := range []struct{ key, usedBy string }{
		{"t", "liveReasonChain REASON_KIND lookup — the step's kind"},
		{"pay", "liveReasonChain step text (the agent's recorded thought)"},
		{"lb", "liveReasonChain step text fallback"},
		{"plan", "liveReasonChain citation — the tool the agent reached for"},
		{"conf", "liveReasonChain stated confidence (0 means NOT STATED, not zero confidence)"},
	} {
		if _, present := nodes[0][c.key]; !present {
			t.Errorf("SessionNodeDTO no longer serializes %q — used by %s. Present keys: %s",
				c.key, c.usedBy, keysOf(nodes[0]))
		}
	}

	// The names that were IMAGINED must stay absent. If one is ever introduced, the console's old broken
	// reading would start half-working, which is worse than failing: a partly-populated chain reads as a
	// complete one.
	for _, ghost := range []string{"external_ref", "steps", "alert_rule"} {
		if _, present := top[ghost]; present {
			t.Errorf("SessionDetailDTO now serializes %q — that name was the console's WRONG guess for a "+
				"real field. Introducing it revives a broken read path; pick a different name", ghost)
		}
	}
	for _, ghost := range []string{"kind", "reason", "label", "confidence", "tools"} {
		if _, present := nodes[0][ghost]; present {
			t.Errorf("SessionNodeDTO now serializes %q — that name was the console's WRONG guess; see above", ghost)
		}
	}
}

func keysOf(m map[string]json.RawMessage) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
