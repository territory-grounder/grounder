package agent

import (
	"context"
	"strings"
	"testing"
)

// TG-643: an empty completion is retried once with the same prompt; it is not an "unparseable" decision.
func TestEmptyCompletionIsRetriedOnce(t *testing.T) {
	m := &scriptedModel{responses: []string{"", toolCall, `{"action":"stop","confidence":0.9,"reason":"fine","evidence_ids":["tr-1"]}`}}
	res, err := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason == "unparseable model output" || res.Reason == "empty model output" {
		t.Fatalf("one empty completion must be retried, not end the session: %s", res.Reason)
	}
	if len(res.ToolResults) == 0 {
		t.Fatal("the retried answer (a tool call) must have been executed")
	}
}

func TestTwoEmptyCompletionsStopWithTheirOwnReason(t *testing.T) {
	m := &scriptedModel{responses: []string{"  ", "\n"}}
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Outcome != OutcomeStop || res.Reason != "empty model output" {
		t.Fatalf("two empty completions must stop as %q, got %s / %q", "empty model output", res.Outcome, res.Reason)
	}
}

// A non-empty malformed answer still fails closed as before: the retry is for EMPTY output only.
func TestMalformedCompletionStillFailsClosed(t *testing.T) {
	m := &scriptedModel{responses: []string{"not json at all"}}
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Reason != "unparseable model output" {
		t.Fatalf("a malformed answer must still stop as unparseable, got %q", res.Reason)
	}
	// TG-643: the unparseable reply is recorded, bounded, on its step so the failure shape is visible.
	if len(res.Steps) == 0 || !strings.Contains(res.Steps[0].Observation, "UNPARSEABLE model output (15 bytes): not json at all") {
		t.Fatalf("the unparseable reply must be recorded on its step, got %+v", res.Steps)
	}
}
