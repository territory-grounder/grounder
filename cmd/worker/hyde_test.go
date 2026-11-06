package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/knowledge"
)

type fakeHydeCompleter struct {
	out      string
	err      error
	gotModel string
	gotMsgs  []model.Message
}

func (f *fakeHydeCompleter) Complete(_ context.Context, _ string, modelName string, msgs []model.Message) (string, error) {
	f.gotModel, f.gotMsgs = modelName, msgs
	return f.out, f.err
}

func TestHydeHypothetical(t *testing.T) {
	q := knowledge.Query{Host: "db1", AlertRule: "DiskFull", Summary: "out of space"}
	armed := func(k, d string) string {
		if k == "TG_RETRIEVE_HYDE" {
			return "1"
		}
		return d
	}
	unset := func(_, d string) string { return d }

	if hydeHypothetical(&fakeHydeCompleter{out: "x"}, unset) != nil {
		t.Error("unarmed (TG_RETRIEVE_HYDE unset) must yield a nil seam (byte-identical)")
	}
	if hydeHypothetical(nil, armed) != nil {
		t.Error("a nil gateway must yield a nil seam")
	}

	fc := &fakeHydeCompleter{out: "  grow the postgres volume and restart the service  "}
	fn := hydeHypothetical(fc, armed)
	if fn == nil {
		t.Fatal("armed with a gateway must yield a seam func")
	}
	if got := fn(context.Background(), q); got != "grow the postgres volume and restart the service" {
		t.Errorf("must return the trimmed hypothetical, got %q", got)
	}
	if fc.gotModel != "fast" {
		t.Errorf("default HyDE model must be 'fast', got %q", fc.gotModel)
	}
	var joined strings.Builder
	for _, m := range fc.gotMsgs {
		joined.WriteString(m.Content)
		joined.WriteByte(' ')
	}
	if !strings.Contains(joined.String(), "DiskFull") || !strings.Contains(joined.String(), "db1") {
		t.Errorf("the HyDE prompt must carry the incident identity, got %q", joined.String())
	}

	// An LLM error degrades to "" (the FusedRetriever then embeds the raw query).
	if got := hydeHypothetical(&fakeHydeCompleter{err: errors.New("gateway down")}, armed)(context.Background(), q); got != "" {
		t.Errorf("an LLM error must degrade to \"\", got %q", got)
	}

	// TG-530: compose forwards TG_HYDE_MODEL as ${TG_HYDE_MODEL:-}, making the variable PRESENT-but-empty —
	// getenv then returns "" instead of the compiled default, and every HyDE completion POSTed model:""
	// (a doomed LiteLLM 400, ~10 per deep retrieval, silently swallowed). Blank must fall back to "fast".
	emptyModel := func(k, d string) string {
		switch k {
		case "TG_RETRIEVE_HYDE":
			return "1"
		case "TG_HYDE_MODEL":
			return "" // present-but-empty, the live compose shape
		}
		return d
	}
	fc2 := &fakeHydeCompleter{out: "resize the volume"}
	if got := hydeHypothetical(fc2, emptyModel)(context.Background(), q); got != "resize the volume" {
		t.Fatalf("present-but-empty model env must still complete, got %q", got)
	}
	if fc2.gotModel != "fast" {
		t.Errorf("present-but-empty TG_HYDE_MODEL must fall back to 'fast', got %q — model:\"\" is the TG-530 doomed call", fc2.gotModel)
	}
}

func TestHydePrompt(t *testing.T) {
	if hydePrompt(knowledge.Query{}) != "" {
		t.Error("no rule and no summary ⇒ empty prompt (nothing to hypothesize from)")
	}
	p := hydePrompt(knowledge.Query{AlertRule: "DiskFull", Host: "db1", Summary: "out of space"})
	for _, w := range []string{"DiskFull", "db1", "out of space"} {
		if !strings.Contains(p, w) {
			t.Errorf("prompt must contain %q, got %q", w, p)
		}
	}
}
