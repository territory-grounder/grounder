package agent

import (
	"context"
	"strings"
	"testing"
)

// TG-659: a confidence-threshold stop records WHY on its step: whether the confidence was absent or low, the
// action, and the bounded raw reply, so a lost heal can be diagnosed.
func TestConfidenceStopRecordsWhyOnTheStep(t *testing.T) {
	for name, tc := range map[string]struct{ reply, want string }{
		"absent": {`{"action":"tool","tool":"get-logs","args":{"host":"web01"}}`, "(absent 0.00 < 0.50) on action \"tool\""},
		"low":    {`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.3}`, "(typed 0.30 < 0.50) on action \"tool\""},
	} {
		t.Run(name, func(t *testing.T) {
			m := &scriptedModel{responses: []string{tc.reply}}
			res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
			if res.Reason != "confidence below stop threshold" {
				t.Fatalf("reason = %q", res.Reason)
			}
			if len(res.ToolResults) != 0 {
				t.Fatalf("a below-threshold directive must not run its tool, ran %d", len(res.ToolResults))
			}
			obs := res.Steps[len(res.Steps)-1].Observation
			if !strings.Contains(obs, "CONFIDENCE BELOW STOP THRESHOLD "+tc.want) || !strings.Contains(obs, `"get-logs"`) {
				t.Fatalf("the step must record the source, value, action and the raw reply, got %q", obs)
			}
		})
	}
}
