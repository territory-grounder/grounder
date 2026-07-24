package metrics

import (
	"strings"
	"testing"
)

// TestClampsBoundLabelCardinality proves every label value is folded to a CLOSED enum — the guarantee that
// a hostname/ref/op can never become an unbounded label on the /metrics surface.
func TestClampsBoundLabelCardinality(t *testing.T) {
	for in, want := range map[string]string{
		"proposed": "proposed", "stop": "stop", "escalate": "escalate", "hard-halt": "hard-halt",
		"": "other", "web01": "other", "anything-else": "other",
	} {
		if got := ClampAgentOutcome(in); got != want {
			t.Fatalf("ClampAgentOutcome(%q)=%q want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"match": "match", "partial": "partial", "deviation": "deviation",
		"": "unset", "unset": "unset", "bogus": "other",
	} {
		if got := ClampVerdict(in); got != want {
			t.Fatalf("ClampVerdict(%q)=%q want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"AUTO": "AUTO", "AUTO_NOTICE": "AUTO_NOTICE", "POLL_PAUSE": "POLL_PAUSE",
		"": "other", "dc1web01": "other",
	} {
		if got := ClampBand(in); got != want {
			t.Fatalf("ClampBand(%q)=%q want %q", in, got, want)
		}
	}
}

// TestFamilyConstructorsPresetNameKindHelp proves the constructors preset the metric identity and clamp
// their label values, so a caller cannot mislabel a family or smuggle an unbounded label through them.
func TestFamilyConstructorsPresetNameKindHelp(t *testing.T) {
	runs := AgentRunsSample("not-an-outcome", 3)
	if runs.Name != MetricAgentRuns || runs.Kind != Counter || runs.Labels["outcome"] != "other" {
		t.Fatalf("AgentRunsSample mislabeled: %+v", runs)
	}
	dec := DecisionsSample("not-a-band", true, 1)
	if dec.Labels["band"] != "other" || dec.Labels["withheld"] != "true" {
		t.Fatalf("DecisionsSample mislabeled: %+v", dec)
	}
	// the four base counters carry no label at all.
	for _, s := range []Sample{AgentRunSecondsSample(1), AgentToolCallsSample(1), AgentToolErrorsSample(1), AgentTokensApproxSample(1)} {
		if len(s.Labels) != 0 || s.Kind != Counter {
			t.Fatalf("base counter must be an unlabelled counter: %+v", s)
		}
	}

	// rendered together they stay grouped + deterministic (the exposition contract).
	out := Render([]Sample{
		AgentRunsSample("proposed", 1),
		VerdictsSample("match", 1),
		DecisionsSample("POLL_PAUSE", true, 1),
	})
	if strings.Count(out, "# TYPE tg_governance_decisions_total counter") != 1 {
		t.Fatalf("decision family must carry exactly one TYPE header:\n%s", out)
	}
}
