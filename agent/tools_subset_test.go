package agent

import (
	"context"
	"testing"
)

type subsetProbeTool struct {
	name     string
	readOnly bool
}

func (t subsetProbeTool) Name() string   { return t.name }
func (t subsetProbeTool) ReadOnly() bool { return t.readOnly }
func (t subsetProbeTool) Invoke(context.Context, map[string]string) (ToolResult, error) {
	return ToolResult{}, nil
}

// TG-80 P2-5: a pack's tool allowlist is a SUBSET of the registered read-only set — present names carry
// their source namespace, absent names are reported. KILLING MUTATION: make SubsetFor return the parent
// set (ignore names) and the absence assertions fail.
func TestSubsetForPartitionsAndPreservesSources(t *testing.T) {
	s := NewReadOnlyToolSet()
	if err := s.RegisterFrom("cisco", subsetProbeTool{name: "show-device-config", readOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterFrom("host", subsetProbeTool{name: "host-df", readOnly: true}); err != nil {
		t.Fatal(err)
	}

	sub, missing := s.SubsetFor([]string{"show-device-config", "no-such-tool"})
	if _, ok := sub.Get("show-device-config"); !ok {
		t.Fatal("named tool absent from the subset")
	}
	if _, ok := sub.Get("host-df"); ok {
		t.Fatal("an unnamed tool leaked into the subset")
	}
	if len(missing) != 1 || missing[0] != "no-such-tool" {
		t.Fatalf("missing = %v", missing)
	}
	if got := sub.sources["show-device-config"]; got != "cisco" {
		t.Fatalf("source namespace lost: %q", got)
	}
}

func TestSubsetForNilReceiverReportsEverythingMissing(t *testing.T) {
	var s *ToolSet
	sub, missing := s.SubsetFor([]string{"a", "b"})
	if sub == nil || len(sub.Names()) != 0 {
		t.Fatalf("nil receiver must yield an empty set, got %v", sub.Names())
	}
	if len(missing) != 2 {
		t.Fatalf("missing = %v", missing)
	}
}
