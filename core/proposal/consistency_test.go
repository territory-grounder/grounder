package proposal

import (
	"strings"
	"testing"
)

// TestInconsistentReasoningBothDirections — the two disagreements the predecessor's check names, and they
// are different defects: asserting near-certainty in hedging prose, versus proposing a pile of actions
// while claiming low confidence.
//
// RED MUTATION CONTROL (executed 2026-08-01): dropping the low-confidence arm passes the high case and
// fails the low one; restored green.
func TestInconsistentReasoningBothDirections(t *testing.T) {
	// hasConfidence is an INDEPENDENT fixture field, deliberately NOT derived from the confidence value.
	// Deriving it (`c.confidence >= 0`) made the fixture encode the code's own assumption, and a mutation
	// replacing the explicit flag with `confidence < 0` SURVIVED cleanly — the case that matters, an
	// unrecorded confidence arriving as 0.0, was unreachable from the table. Same shape as the
	// `proposed:shadow` fixture that hid a live defect earlier today: a fixture sharing the code's
	// assumption cannot see the bug.
	cases := []struct {
		name          string
		confidence    float64
		hasConfidence bool
		text          string
		want          bool
		reasonHas     string
	}{
		{
			name: "high confidence, hedging prose", confidence: 0.9, hasConfidence: true,
			text: "The unit might be wedged. It is unclear whether the mount is stale, and I am unsure " +
				"which of the two caused it.",
			want: true, reasonHas: "hedging",
		},
		{
			name: "low confidence, action-heavy prose", confidence: 0.3, hasConfidence: true,
			text: "Restart the service, then apply the config, then deploy the new unit file.",
			want: true, reasonHas: "action",
		},
		{
			name: "high confidence, decisive prose", confidence: 0.9, hasConfidence: true,
			text: "The unit is wedged: its pid is gone and the socket is closed. Restart it.",
			want: false,
		},
		{
			name: "low confidence, cautious prose", confidence: 0.3, hasConfidence: true,
			text: "I cannot tell what happened here; the logs are truncated.",
			want: false,
		},
		{
			name: "mid confidence never trips", confidence: 0.65, hasConfidence: true,
			text: "It might be the mount, possibly the network, unclear, uncertain, maybe both. " +
				"Restart, apply, deploy, fix, change.",
			want: false,
		},
		{
			// The predecessor's escape hatch: naming the actions you are DECLINING to take is exactly what
			// a careful low-confidence conclusion looks like.
			name: "low confidence that explicitly stands down", confidence: 0.2, hasConfidence: true,
			text: "I would restart the unit, apply the config and deploy the fix — but stop here and wait " +
				"for a human, because the evidence does not support it.",
			want: false,
		},
		{
			// Two hedges is under the bar; the threshold is deliberately the predecessor's.
			name: "below the word threshold", confidence: 0.95, hasConfidence: true,
			text: "It might be the mount, or possibly the socket.",
			want: false,
		},
		{
			// THE CASE THAT NEARLY SHIPPED AS A FALSE-POSITIVE GENERATOR. An unrecorded confidence arrives
			// as 0.0 from agent.Result (no presence flag), which is below lowConfidence — so without an
			// explicit hasConfidence every confidence-less session naming three actions would be flagged.
			name: "unrecorded confidence never trips", confidence: -1, hasConfidence: false,
			text: "Restart the service, apply the config, deploy the unit.",
			want: false,
		},
		{
			// THE MUTATION-KILLING CASE. An unrecorded confidence arrives from agent.Result as 0.0 — below
			// lowConfidence — so inferring presence from the value flags every confidence-less session that
			// names three actions. This is that session.
			name: "ZERO confidence that was never recorded", confidence: 0.0, hasConfidence: false,
			text: "Restart the service, apply the config, deploy the unit.",
			want: false,
		},
		{
			// ...and its converse: a GENUINE zero-confidence proposal naming three actions IS inconsistent,
			// and must still be caught. Treating 0.0 as always-unrecorded would silence the real case.
			name: "ZERO confidence that WAS recorded", confidence: 0.0, hasConfidence: true,
			text: "Restart the service, apply the config, deploy the unit.",
			want: true, reasonHas: "action",
		},
		{
			name: "empty text never trips", confidence: 0.99, hasConfidence: true, text: "   ", want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := InconsistentReasoning(c.confidence, c.hasConfidence, c.text)
			if got != c.want {
				t.Fatalf("InconsistentReasoning(%.2f, ...) = %v (%q), want %v", c.confidence, got, reason, c.want)
			}
			if c.want && !strings.Contains(reason, c.reasonHas) {
				t.Errorf("the reason must name WHICH direction tripped — %q lacks %q. A bare boolean "+
					"conflates two different defects.", reason, c.reasonHas)
			}
			if !c.want && reason != "" {
				t.Errorf("a consistent proposal must carry no reason, got %q", reason)
			}
		})
	}
}

// TestCheckIsCaseInsensitiveAndWordBounded — the vocabularies are ported verbatim from the predecessor,
// including its word boundaries. "Fixed" must count and "prefix" must not, or the signal drifts from the
// one the predecessor measured.
func TestCheckIsCaseInsensitiveAndWordBounded(t *testing.T) {
	if got, _ := InconsistentReasoning(0.2, true, "RESTART the unit, Apply the change, DEPLOY it."); !got {
		t.Error("the word sets must be case-insensitive, as the predecessor's regexes are")
	}
	// "prefix" contains "fix"; "updated" contains "update". Word boundaries must exclude the first and
	// INCLUDE the second only as its own token — matching the predecessor's \b anchors exactly.
	if got, _ := InconsistentReasoning(0.2, true, "The prefix is affixed to the suffix, transfixed."); got {
		t.Error("substrings inside other words must not count — that would inflate the action count on " +
			"prose that names no action at all")
	}
}
