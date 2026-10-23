package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func edcGetenv(env map[string]string) func(string, string) string {
	return func(k, d string) string {
		if v, ok := env[k]; ok {
			return v
		}
		return d
	}
}

var edcQuiet = func(string, ...any) {}

// UNCONFIGURED must emit NOTHING — honest absence, not a phantom zero (the self-dependency discipline).
// KILLING MUTATION: drop the `root == ""` guard and the job loads an empty corpus and emits 0-valued gauges,
// reddening this.
func TestEstateDocCoverage_UnconfiguredEmitsNothing(t *testing.T) {
	if got := startEstateDocCoverageJob(edcGetenv(nil), edcQuiet)(); got != nil {
		t.Errorf("unconfigured estate-doc coverage emitted %v, want nil (absent, not a silent zero)", got)
	}
}

func TestEstateDocCoverage_ReportsCoverageFromFixture(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("# H1\nbody one\n# H2\nbody two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.md"), []byte("# B\nbody b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	samples := startEstateDocCoverageJob(edcGetenv(map[string]string{"TG_ESTATE_DOCS_DIR": root}), edcQuiet)()
	byName := map[string]float64{}
	for _, s := range samples {
		byName[s.Name] = s.Value
	}
	if byName["tg_estate_doc_files"] != 2 {
		t.Errorf("tg_estate_doc_files = %v, want 2", byName["tg_estate_doc_files"])
	}
	if byName["tg_estate_doc_chunks"] != 3 { // a.md → H1,H2 ; b.md → B
		t.Errorf("tg_estate_doc_chunks = %v, want 3", byName["tg_estate_doc_chunks"])
	}
	if v, ok := byName["tg_estate_doc_load_failed"]; !ok || v != 0 {
		t.Errorf("tg_estate_doc_load_failed = %v (ok=%v), want 0 on a clean ingest", v, ok)
	}
}

// edcPatternsFile writes a synthetic redaction-vocabulary file (denylist format). SYNTHETIC tokens only — a
// real estate identifier in a committed test would itself be the leak this pass exists to stop.
func edcPatternsFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "deny.txt")
	if err := os.WriteFile(p, []byte("# synthetic vocabulary\nzzsite[0-9]{2}\nexample-int\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// With TG_ESTATE_DOC_CORPUS set AND TG_ESTATE_DOC_REDACT_PATTERNS armed, the ingest persists the
// identifier-redacted corpus for the eval-gated slice-2 retriever.
func TestEstateDocCoverage_PersistsWhenCorpusPathSet(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("# H\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	corpusPath := filepath.Join(t.TempDir(), "corpus.json")
	env := map[string]string{
		"TG_ESTATE_DOCS_DIR":            root,
		"TG_ESTATE_DOC_CORPUS":          corpusPath,
		"TG_ESTATE_DOC_REDACT_PATTERNS": edcPatternsFile(t),
	}
	_ = startEstateDocCoverageJob(edcGetenv(env), edcQuiet)()
	if _, err := os.Stat(corpusPath); err != nil {
		t.Errorf("corpus was not persisted to %q: %v", corpusPath, err)
	}
}

// TG-486 second transport: with TG_ESTATE_DOC_REDACT_PATTERNS UNSET, the docs-dir walk is secret-scrubbed but
// identifier-LADEN — persisting it would reopen the STONITH-class leak through the worker's own producer path
// (no abort-on-survivor gate runs here). The write must be REFUSED; coverage still arms from memory.
// KILLING MUTATION: drop the `pathMatch == nil` refusal case and the raw corpus lands on disk, reddening this.
func TestEstateDocCoverage_RefusesPersistWithoutRedactPatterns(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("# H\nzzsite01core01 body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	corpusPath := filepath.Join(t.TempDir(), "corpus.json")
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	samples := startEstateDocCoverageJob(edcGetenv(map[string]string{
		"TG_ESTATE_DOCS_DIR": root, "TG_ESTATE_DOC_CORPUS": corpusPath,
	}), logf)()
	if _, err := os.Stat(corpusPath); err == nil {
		t.Fatalf("an identifier-laden corpus was PERSISTED to %q — the refusal gate is gone", corpusPath)
	}
	var refused bool
	for _, l := range logged {
		if strings.Contains(l, "REFUSING to persist") {
			refused = true
		}
	}
	if !refused {
		t.Errorf("the refusal must be logged loudly, got %q", logged)
	}
	byName := map[string]float64{}
	for _, s := range samples {
		byName[s.Name] = s.Value
	}
	if byName["tg_estate_doc_files"] != 1 {
		t.Errorf("coverage must still arm from the in-memory corpus, got %v", byName)
	}
}

// With patterns armed, the PERSISTED corpus must be vocabulary-clean — the same gate-parity property
// scripts/estate-docs-corpus.sh's abort-on-survivor scan enforces, proven here through the WORKER's own
// producer transport (content AND Path/ExternalRef refs).
func TestEstateDocCoverage_PersistedCorpusIsVocabularyClean(t *testing.T) {
	root := t.TempDir()
	siteDir := filepath.Join(root, "zzsite01")
	if err := os.MkdirAll(siteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siteDir, "runbook.md"),
		[]byte("# zzsite01core01 recovery\nssh to zzsite01core01.example-int.lan and restart\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	corpusPath := filepath.Join(t.TempDir(), "corpus.json")
	_ = startEstateDocCoverageJob(edcGetenv(map[string]string{
		"TG_ESTATE_DOCS_DIR":            root,
		"TG_ESTATE_DOC_CORPUS":          corpusPath,
		"TG_ESTATE_DOC_REDACT_PATTERNS": edcPatternsFile(t),
	}), edcQuiet)()
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("corpus not persisted: %v", err)
	}
	for _, pat := range []string{`zzsite[0-9]{2}`, `example-int`} {
		if regexp.MustCompile(pat).Match(raw) {
			t.Errorf("pattern %q still matches the persisted corpus — the worker transport leaks identifiers", pat)
		}
	}
}

// TG_ESTATE_DOC_REDACT_PATTERNS set but unusable (missing file) = the operator ASKED for redaction and it
// could not arm. Falling back to an unredacted walk would be a silent leak — fail closed to load-failed.
func TestEstateDocCoverage_BadRedactPatternsFailsClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("# H\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	samples := startEstateDocCoverageJob(edcGetenv(map[string]string{
		"TG_ESTATE_DOCS_DIR":            root,
		"TG_ESTATE_DOC_REDACT_PATTERNS": filepath.Join(t.TempDir(), "absent.txt"),
	}), edcQuiet)()
	byName := map[string]float64{}
	for _, s := range samples {
		byName[s.Name] = s.Value
	}
	if byName["tg_estate_doc_load_failed"] != 1 {
		t.Errorf("an unarmable redaction request must fail closed to load_failed=1, got %v", byName)
	}
}

// TG-487: the corpus-only transport (distroless worker mounts ONLY the scrubbed corpus, no raw docs dir) must
// arm coverage by READING the pre-built corpus. Producer writes it from a docs dir; consumer, with NO
// TG_ESTATE_DOCS_DIR, reads it back and publishes the same gauges. Before the fix this consumer shape emitted
// nothing (the gauge was published only from a live docs-dir walk).
// KILLING MUTATION: drop the `case corpusPath != "":` branch and the consumer config falls to default → nil,
// reddening this.
func TestEstateDocCoverage_ArmsFromPrebuiltCorpusWhenNoDocsDir(t *testing.T) {
	// Producer: write a corpus from a docs dir (2 files, 3 chunks — same fixture as ReportsCoverageFromFixture).
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("# H1\nbody one\n# H2\nbody two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.md"), []byte("# B\nbody b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	corpusPath := filepath.Join(t.TempDir(), "corpus.json")
	_ = startEstateDocCoverageJob(edcGetenv(map[string]string{
		"TG_ESTATE_DOCS_DIR": root, "TG_ESTATE_DOC_CORPUS": corpusPath,
		"TG_ESTATE_DOC_REDACT_PATTERNS": edcPatternsFile(t),
	}), edcQuiet)()

	// Consumer: NO docs dir, only the pre-built corpus mounted.
	samples := startEstateDocCoverageJob(edcGetenv(map[string]string{"TG_ESTATE_DOC_CORPUS": corpusPath}), edcQuiet)()
	if samples == nil {
		t.Fatal("corpus-only transport emitted nil — coverage did not arm from the pre-built corpus (TG-487)")
	}
	byName := map[string]float64{}
	for _, s := range samples {
		byName[s.Name] = s.Value
	}
	if byName["tg_estate_doc_files"] != 2 {
		t.Errorf("tg_estate_doc_files = %v, want 2 (armed from the pre-built corpus)", byName["tg_estate_doc_files"])
	}
	if byName["tg_estate_doc_chunks"] != 3 {
		t.Errorf("tg_estate_doc_chunks = %v, want 3 (armed from the pre-built corpus)", byName["tg_estate_doc_chunks"])
	}
	if v, ok := byName["tg_estate_doc_load_failed"]; !ok || v != 0 {
		t.Errorf("tg_estate_doc_load_failed = %v (ok=%v), want 0 on a clean corpus read", v, ok)
	}
}

// TG-487: a corpus-only config pointing at a missing/corrupt corpus is a NON-FATAL boot failure — it surfaces
// tg_estate_doc_load_failed=1 rather than crashing the worker or silently emitting nothing.
func TestEstateDocCoverage_CorruptPrebuiltCorpusSurfacesLoadFailed(t *testing.T) {
	badPath := filepath.Join(t.TempDir(), "corpus.json")
	if err := os.WriteFile(badPath, []byte("{ this is not a valid corpus"), 0o644); err != nil {
		t.Fatal(err)
	}
	samples := startEstateDocCoverageJob(edcGetenv(map[string]string{"TG_ESTATE_DOC_CORPUS": badPath}), edcQuiet)()
	byName := map[string]float64{}
	for _, s := range samples {
		byName[s.Name] = s.Value
	}
	if byName["tg_estate_doc_load_failed"] != 1 {
		t.Errorf("a corrupt pre-built corpus must surface tg_estate_doc_load_failed=1, got %v (samples=%v)", byName["tg_estate_doc_load_failed"], samples)
	}
}
