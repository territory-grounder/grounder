package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/knowledge"
)

type fakeQueryRewriteCompleter struct {
	out      string
	err      error
	gotModel string
	gotMsgs  []model.Message
}

func (f *fakeQueryRewriteCompleter) Complete(_ context.Context, _ string, modelName string, msgs []model.Message) (string, error) {
	f.gotModel, f.gotMsgs = modelName, msgs
	return f.out, f.err
}

func TestQueryRewrite(t *testing.T) {
	q := knowledge.Query{Host: "db1", AlertRule: "DiskFull", Summary: "out of space"}
	armed := func(k, d string) string {
		if k == "TG_RETRIEVE_QUERY_REWRITE" {
			return "1"
		}
		return d
	}
	unset := func(_, d string) string { return d }

	if queryRewrite(&fakeQueryRewriteCompleter{out: "x"}, unset) != nil {
		t.Error("unarmed (TG_RETRIEVE_QUERY_REWRITE unset) must yield a nil seam (byte-identical)")
	}
	if queryRewrite(nil, armed) != nil {
		t.Error("a nil gateway must yield a nil seam")
	}

	fc := &fakeQueryRewriteCompleter{out: "  postgres data volume full, service down  "}
	fn := queryRewrite(fc, armed)
	if fn == nil {
		t.Fatal("armed with a gateway must yield a seam func")
	}
	got := fn(context.Background(), q)
	if got.Summary != "postgres data volume full, service down" {
		t.Errorf("must set Summary to the trimmed rewrite, got %q", got.Summary)
	}
	if got.Host != "db1" || got.AlertRule != "DiskFull" {
		t.Errorf("typed fields must be preserved, got host=%q rule=%q", got.Host, got.AlertRule)
	}
	if fc.gotModel != "fast" {
		t.Errorf("default rewrite model must be 'fast', got %q", fc.gotModel)
	}
	var joined strings.Builder
	for _, m := range fc.gotMsgs {
		joined.WriteString(m.Content)
		joined.WriteByte(' ')
	}
	if !strings.Contains(joined.String(), "DiskFull") || !strings.Contains(joined.String(), "db1") {
		t.Errorf("the rewrite prompt must carry the incident identity, got %q", joined.String())
	}

	// An LLM error degrades to the ORIGINAL query (the stack retrieves on the raw query).
	if got := queryRewrite(&fakeQueryRewriteCompleter{err: errors.New("gateway down")}, armed)(context.Background(), q); got.Summary != "out of space" {
		t.Errorf("an LLM error must return the original query, got %q", got.Summary)
	}
	// An empty rewrite likewise returns the original.
	if got := queryRewrite(&fakeQueryRewriteCompleter{out: "   "}, armed)(context.Background(), q); got.Summary != "out of space" {
		t.Errorf("an empty rewrite must return the original query, got %q", got.Summary)
	}

	// TG-530: compose's ${TG_QUERY_REWRITE_MODEL:-} makes the var PRESENT-but-empty — getenv returns ""
	// instead of the compiled default and the rewrite POSTed model:"" (one doomed 400 per retrieval,
	// silently degraded to the unrewritten query). Blank must fall back to "fast".
	emptyModel := func(k, d string) string {
		switch k {
		case "TG_RETRIEVE_QUERY_REWRITE":
			return "1"
		case "TG_QUERY_REWRITE_MODEL":
			return "" // present-but-empty, the live compose shape
		}
		return d
	}
	fc2 := &fakeQueryRewriteCompleter{out: "disk full on db volume"}
	if got := queryRewrite(fc2, emptyModel)(context.Background(), q); got.Summary != "disk full on db volume" {
		t.Fatalf("present-but-empty model env must still rewrite, got %q", got.Summary)
	}
	if fc2.gotModel != "fast" {
		t.Errorf("present-but-empty TG_QUERY_REWRITE_MODEL must fall back to 'fast', got %q (TG-530)", fc2.gotModel)
	}
}

func TestQueryRewritePrompt(t *testing.T) {
	if queryRewritePrompt(knowledge.Query{}) != "" {
		t.Error("no rule and no summary ⇒ empty prompt (nothing to rewrite from)")
	}
	p := queryRewritePrompt(knowledge.Query{AlertRule: "DiskFull", Host: "db1", Summary: "out of space"})
	for _, w := range []string{"DiskFull", "db1", "out of space"} {
		if !strings.Contains(p, w) {
			t.Errorf("prompt must contain %q, got %q", w, p)
		}
	}
}
