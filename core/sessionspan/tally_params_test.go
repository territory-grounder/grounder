package sessionspan

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
)

type scriptedParams struct {
	got model.Params
}

func (s *scriptedParams) Complete(_ context.Context, _, _ string, _ []model.Message) (string, error) {
	return "plain", nil
}

func (s *scriptedParams) CompleteWithParams(_ context.Context, _, _ string, _ []model.Message, p model.Params) (model.Completion, error) {
	s.got = p
	return model.Completion{Text: "pinned", Model: "upstream-x", Usage: model.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4, Measured: true}}, nil
}

// TG-670: wrapping the gateway in a Tally must not hide the pinned-sampling seam from the judge, and the usage
// of a parameterised call is tallied like any other.
func TestTallyForwardsTheParamSeamAndTalliesItsUsage(t *testing.T) {
	inner := &scriptedParams{}
	ta := NewTally(inner)
	c, err := ta.CompleteWithParams(context.Background(), "session-judge", "judge", nil, model.ParamsOf(0, 0))
	if err != nil || c.Text != "pinned" || c.Model != "upstream-x" {
		t.Fatalf("forwarded completion = %+v (%v)", c, err)
	}
	if inner.got.Temperature == nil || *inner.got.Temperature != 0 || inner.got.Seed == nil || *inner.got.Seed != 0 {
		t.Fatalf("params not forwarded: %+v", inner.got)
	}
	if tok := ta.Tokens(); tok.Calls != 1 || tok.Total != 4 {
		t.Fatalf("usage not tallied: %+v", tok)
	}
	// A plain completer behind the Tally: the seam falls back to Complete and the model stays unrecorded.
	plain := NewTally(&plainCompleter{})
	c, err = plain.CompleteWithParams(context.Background(), "u", "judge", nil, model.ParamsOf(0, 0))
	if err != nil || c.Model != "" {
		t.Fatalf("fallback completion = %+v (%v), want an unrecorded model", c, err)
	}
}
