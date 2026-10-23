package harnessid

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func fixture() Parts {
	return Parts{
		Prose:          "PROTOCOL\nTools:\n{{TOOL_CATALOG}}\nOps:\n{{OPCLASS_CATALOG}}\n",
		ToolCatalog:    "- read_file: reads a file",
		OpClassCatalog: "- restart-service: params.unit",
		Guidance:       "## Guidance A",
	}
}

// Assemble is pinned to the historical renderPreamble assembly byte for byte: both placeholders in
// order, then ONE newline and the guidance. Any drift here would silently change every fingerprint.
func TestAssembleIsTheHistoricalPreambleAssembly(t *testing.T) {
	got := Assemble(fixture())
	want := "PROTOCOL\nTools:\n- read_file: reads a file\nOps:\n- restart-service: params.unit\n\n## Guidance A"
	if got != want {
		t.Fatalf("assembly drifted:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "{{") {
		t.Fatal("a placeholder survived assembly")
	}
}

// Bind is the TG-575 two-step hash (hex content hash ‖ NUL ‖ version), reproduced here independently so
// a refactor of the runner's harnessFingerprint cannot move the persisted values.
func TestBindReproducesTheTG575Hash(t *testing.T) {
	content := ContentHash("system half")
	h := sha256.New()
	h.Write([]byte(content))
	h.Write([]byte{0})
	h.Write([]byte("preamble/5"))
	want := hex.EncodeToString(h.Sum(nil))
	if got := Bind(content, "preamble/5"); got != want {
		t.Fatalf("Bind drifted from the persisted hash: got %s want %s", got[:12], want[:12])
	}
	if Bind(content, "preamble/5") == Bind(content, "preamble/6") {
		t.Fatal("the preamble version must move the bound fingerprint")
	}
}

// A record is accepted only when its key derives from its parts; every other shape is refused, and the
// refusal names the key it would have derived.
func TestVerifyRefusesAKeyThatDoesNotDeriveFromTheParts(t *testing.T) {
	p := fixture()
	good := Record{HarnessFP: Fingerprint(p, "preamble/5"), PromptVersion: "preamble/5", Parts: p}
	if err := Verify(good); err != nil {
		t.Fatalf("a derived key must verify: %v", err)
	}
	other := p
	other.Guidance = "## Guidance B (trial arm)"
	forged := Record{HarnessFP: Fingerprint(other, "preamble/5"), PromptVersion: "preamble/5", Parts: p}
	err := Verify(forged)
	if err == nil {
		t.Fatal("a record keyed by ANOTHER configuration's fingerprint must be refused")
	}
	if !strings.Contains(err.Error(), "does not derive") || !strings.Contains(err.Error(), Short(good.HarnessFP)) {
		t.Fatalf("the refusal must name the key the parts derive to: %v", err)
	}
	wrongVersion := good
	wrongVersion.PromptVersion = "preamble/6"
	if Verify(wrongVersion) == nil {
		t.Fatal("the same parts under another preamble version derive to another key and must be refused")
	}
	for name, bad := range map[string]Record{
		"malformed key":     {HarnessFP: "abc", PromptVersion: "preamble/5", Parts: p},
		"no version":        {HarnessFP: good.HarnessFP, PromptVersion: " ", Parts: p},
		"no prose":          {HarnessFP: Fingerprint(Parts{}, "preamble/5"), PromptVersion: "preamble/5", Parts: Parts{}},
		"uppercase hex key": {HarnessFP: strings.ToUpper(good.HarnessFP), PromptVersion: "preamble/5", Parts: p},
	} {
		if Verify(bad) == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

// Each component hash moves with its own component only — the property the per-column diff rests on.
func TestHashesMoveWithTheirOwnComponentOnly(t *testing.T) {
	base := fixture().Hashes()
	g := fixture()
	g.Guidance = "## Guidance B"
	gh := g.Hashes()
	if gh.Guidance == base.Guidance || gh.Prose != base.Prose || gh.ToolCatalog != base.ToolCatalog || gh.OpClassCatalog != base.OpClassCatalog {
		t.Fatal("a guidance change must move the guidance hash and nothing else")
	}
	tl := fixture()
	tl.ToolCatalog += "\n- list_dir: lists"
	th := tl.Hashes()
	if th.ToolCatalog == base.ToolCatalog || th.Guidance != base.Guidance {
		t.Fatal("a tool-catalog change must move the tools hash and nothing else")
	}
	if Fingerprint(g, "preamble/5") == Fingerprint(fixture(), "preamble/5") {
		t.Fatal("a moved component must move the fingerprint")
	}
}

func TestShortAndIsFingerprint(t *testing.T) {
	fp := Fingerprint(fixture(), "preamble/5")
	if !IsFingerprint(fp) || IsFingerprint(fp[:63]) || IsFingerprint("") {
		t.Fatal("IsFingerprint must accept exactly 64 lowercase hex")
	}
	if Short(fp) != fp[:12] || Short("ab") != "ab" {
		t.Fatal("Short is the 12-hex prefix, or the whole value when shorter")
	}
}
