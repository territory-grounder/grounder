package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/harnessid"
)

// TG-600 (Harness² H2-1c): the parts the render store persists ARE the system message — assembled they
// equal the canonical-class preamble byte for byte, and the fingerprint is their content hash. This is the
// seam that makes a stored rendering trustworthy: the agent, the fingerprint and the store share one
// assembly (harnessid.Assemble), so none can drift from the others.
func TestHarnessPartsAssembleToTheCanonicalPreamble(t *testing.T) {
	ag := &Agent{Tools: NewReadOnlyToolSet(), Guidance: "## Guidance A (store-composed)"}
	parts := ag.HarnessParts()
	want := protocolPreamble(ag.Tools, execclass.StandardAgent, ag.Guidance).Content
	if got := harnessid.Assemble(parts); got != want {
		t.Fatalf("Assemble(HarnessParts()) must equal the canonical preamble byte for byte (%d vs %d bytes)", len(got), len(want))
	}
	if harnessid.Fingerprint(parts, "v") != harnessid.Bind(harnessid.ContentHash(want), "v") {
		t.Fatal("the fingerprint must be the content hash of the assembled parts")
	}
	if !strings.Contains(parts.Prose, harnessid.ToolCatalogPlaceholder) || !strings.Contains(parts.Prose, harnessid.OpClassCatalogPlaceholder) {
		t.Fatal("the prose part is the TEMPLATE, placeholders included — the catalogs are their own parts")
	}
	if parts.Guidance != ag.Guidance {
		t.Fatal("a store-composed guidance half must be recorded verbatim")
	}
	if strings.TrimSpace(parts.ToolCatalog) == "" || strings.TrimSpace(parts.OpClassCatalog) == "" {
		t.Fatal("the tool and op-class catalogs must be recorded as rendered")
	}
}

// Each part moves its own hash only; the embedded guidance fallback is recorded as the EFFECTIVE part
// (what the model saw), not as an empty string.
func TestHarnessPartsRecordEffectiveFallbacksAndMoveIndependently(t *testing.T) {
	base := (&Agent{Tools: NewReadOnlyToolSet(), Guidance: "## A"}).HarnessParts().Hashes()
	fallback := (&Agent{Tools: NewReadOnlyToolSet()}).HarnessParts()
	if strings.TrimSpace(fallback.Guidance) == "" || fallback.Guidance != promptPart("base-prompt-guidance") {
		t.Fatal("an empty guidance half must record the embedded fallback the model actually received")
	}
	other := (&Agent{Tools: NewReadOnlyToolSet(), Guidance: "## B"}).HarnessParts().Hashes()
	if other.Guidance == base.Guidance || other.Prose != base.Prose || other.ToolCatalog != base.ToolCatalog || other.OpClassCatalog != base.OpClassCatalog {
		t.Fatal("a guidance change must move the guidance hash and nothing else")
	}
	ts := NewReadOnlyToolSet()
	if err := ts.Register(tg600Tool{}); err != nil {
		t.Fatal(err)
	}
	withTool := (&Agent{Tools: ts, Guidance: "## A"}).HarnessParts().Hashes()
	if withTool.ToolCatalog == base.ToolCatalog || withTool.Guidance != base.Guidance || withTool.Prose != base.Prose {
		t.Fatal("a tool-catalog change must move the tools hash and nothing else")
	}
}

// tg600Tool is a read-only tool that exists only in this test, so registering it changes the rendered
// tool catalog and nothing else.
type tg600Tool struct{}

func (tg600Tool) Name() string   { return "tg600_probe_tool" }
func (tg600Tool) ReadOnly() bool { return true }
func (tg600Tool) Invoke(context.Context, map[string]string) (ToolResult, error) {
	return ToolResult{}, nil
}
