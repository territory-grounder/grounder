package main

// The drift gate covered ONE of the eighteen artifacts the generator writes. These oracles pin the breadth:
// every emitted artifact is compared, a missing one is drift, and a committed schema the generator no longer
// emits is drift too (the retired-but-present shape). Without them, mutating the schema or AsyncAPI emitter
// leaves `gencontracts -check` green while the published contract goes stale — the exact defect INV-15 exists
// to prevent, one level up from the openapi-only comparison the file's own comment warns about.

import (
	"os"
	"path/filepath"
	"testing"

	gc "github.com/territory-grounder/grounder/tools/gencontracts"
)

// materialiseContracts writes a full, CURRENT set of artifacts into a temp repo root and returns that root.
func materialiseContracts(t *testing.T, model gc.Model) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, contractsDir)
	if err := os.MkdirAll(filepath.Join(dir, "schemas"), 0o755); err != nil {
		t.Fatal(err)
	}
	art := gc.Generate(model, "2026-01-01T00:00:00Z")
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("openapi.yaml", art.OpenAPI)
	write("asyncapi.yaml", art.AsyncAPI)
	for table, s := range art.JSONSchemas {
		write(filepath.Join("schemas", table+".schema.json"), s)
	}
	return root
}

func modelForTest(t *testing.T) gc.Model {
	t.Helper()
	model, err := gc.BuildModel()
	if err != nil {
		t.Fatalf("build model: %v", err)
	}
	return model
}

// A freshly materialised set matches — the anti-vacuity floor for every case below (a check that always
// failed would "detect" every mutation without proving anything).
func TestAllArtifactsMatchesAFreshSet(t *testing.T) {
	model := modelForTest(t)
	if err := allArtifactsMatchGenerator(materialiseContracts(t, model), model); err != nil {
		t.Fatalf("a freshly generated artifact set must match: %v", err)
	}
}

// THE GAP THIS CLOSES: a drifted JSON SCHEMA. The old check compared only openapi.yaml, so this was invisible.
func TestAllArtifactsDetectsSchemaDrift(t *testing.T) {
	model := modelForTest(t)
	root := materialiseContracts(t, model)
	schemas, err := os.ReadDir(filepath.Join(root, contractsDir, "schemas"))
	if err != nil || len(schemas) == 0 {
		t.Fatalf("no schemas materialised (%v) — the case would be vacuous", err)
	}
	victim := filepath.Join(root, contractsDir, "schemas", schemas[0].Name())
	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, append(body, []byte("\n  \"x-stale-emitter-field\": true\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := allArtifactsMatchGenerator(root, model); err == nil {
		t.Fatalf("a drifted schema (%s) must be reported as drift", schemas[0].Name())
	}
}

// A drifted ASYNCAPI document — the other artifact the openapi-only check never looked at.
func TestAllArtifactsDetectsAsyncAPIDrift(t *testing.T) {
	model := modelForTest(t)
	root := materialiseContracts(t, model)
	p := filepath.Join(root, contractsDir, "asyncapi.yaml")
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append(body, []byte("\nx-stale-emitter-block: true\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := allArtifactsMatchGenerator(root, model); err == nil {
		t.Fatal("a drifted asyncapi.yaml must be reported as drift")
	}
}

// An artifact the generator emits but nothing commits is drift, not a pass — otherwise deleting a contract
// would be the way to make the gate green.
func TestAllArtifactsDetectsAMissingArtifact(t *testing.T) {
	model := modelForTest(t)
	root := materialiseContracts(t, model)
	if err := os.Remove(filepath.Join(root, contractsDir, "asyncapi.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := allArtifactsMatchGenerator(root, model); err == nil {
		t.Fatal("a missing committed artifact must be reported as drift")
	}
}

// An ORPHAN schema — committed, but the generator no longer emits it: a retired entity whose published
// contract is still shipping.
func TestAllArtifactsDetectsAnOrphanSchema(t *testing.T) {
	model := modelForTest(t)
	root := materialiseContracts(t, model)
	orphan := filepath.Join(root, contractsDir, "schemas", "retired_entity.schema.json")
	if err := os.WriteFile(orphan, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := allArtifactsMatchGenerator(root, model); err == nil {
		t.Fatal("a committed schema the generator no longer emits must be reported as drift")
	}
}

// A timestamp difference is still NOT drift, across the whole set — the normaliser applies to every artifact,
// so the broadened check does not turn every run red.
func TestAllArtifactsIgnoresTimestampOnlyDifference(t *testing.T) {
	model := modelForTest(t)
	root := materialiseContracts(t, model)
	// Re-materialise openapi.yaml with a DIFFERENT generated_at and nothing else changed.
	other := gc.Generate(model, "2099-12-31T23:59:59Z")
	if err := os.WriteFile(filepath.Join(root, contractsDir, "openapi.yaml"), []byte(other.OpenAPI), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := allArtifactsMatchGenerator(root, model); err != nil {
		t.Fatalf("a timestamp-only difference must not be drift: %v", err)
	}
}
