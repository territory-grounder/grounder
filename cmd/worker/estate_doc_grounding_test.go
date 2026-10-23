package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TG-86 slice 2b: estateDocSeedGrounding builds the Deps.EstateDocs provider. Unarmed ⇒ nil (grounding OFF,
// byte-identical seed). Armed over a real docs dir ⇒ a func that grounds a host named in a doc, and returns
// nothing for a host no doc mentions.
func TestEstateDocSeedGrounding(t *testing.T) {
	quiet := func(string, ...any) {}

	// OFF: TG_ESTATE_DOC_SEED unset ⇒ nil.
	if fn := estateDocSeedGrounding(func(_, d string) string { return d }, quiet, nil); fn != nil {
		t.Error("unarmed (TG_ESTATE_DOC_SEED unset) must yield a nil grounding func")
	}

	// Armed with no corpus source ⇒ nil (non-fatal).
	armedNoCorpus := func(k, d string) string {
		if k == "TG_ESTATE_DOC_SEED" {
			return "1"
		}
		return d
	}
	if fn := estateDocSeedGrounding(armedNoCorpus, quiet, nil); fn != nil {
		t.Error("armed but no corpus source must yield nil (non-fatal), got a func")
	}

	// Armed with a docs dir that names a host in a doc body: the func grounds that host.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "librespeed.md"),
		[]byte("# Librespeed\nThe librespeed01 speedtest service runs in docker; grow the disk if it fills.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"TG_ESTATE_DOC_SEED": "1", "TG_ESTATE_DOCS_DIR": dir}
	getenv := func(k, d string) string {
		if v, ok := env[k]; ok {
			return v
		}
		return d
	}
	fn := estateDocSeedGrounding(getenv, quiet, func() []string { return []string{"librespeed01"} })
	if fn == nil {
		t.Fatal("armed with a non-empty docs dir must yield a grounding func")
	}
	// The doc names host librespeed01. With the redaction pass (TG-486) the grounding surfaces the DOC CONTENT
	// (the model sees the guidance) but the estate identifier is REDACTED to the marker — never the host name.
	block := fn("librespeed01", "disk filling")
	if !strings.Contains(block, "ESTATE DOCUMENTATION") || !strings.Contains(block, "speedtest") {
		t.Errorf("grounding must surface the doc content in a grounding block, got %q", block)
	}
	if strings.Contains(block, "librespeed01") {
		t.Errorf("the estate identifier must be REDACTED from the model-facing grounding (TG-486), got %q", block)
	}
	if !strings.Contains(block, "[REDACTED:estate-id]") {
		t.Errorf("the redaction marker must be present in the grounding, got %q", block)
	}
	// A host no doc mentions (and an empty summary) grounds nothing.
	if block := fn("unrelated-host-999", ""); block != "" {
		t.Errorf("a host no doc mentions must yield no grounding, got %q", block)
	}
}
