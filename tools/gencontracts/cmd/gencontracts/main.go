// Command gencontracts regenerates Territory Grounder's wire contracts from the canonical model, or
// (with -check) verifies the committed contracts have not drifted from the served surface. It is a thin
// wrapper over the gencontracts library so the acceptance oracle can drive the same code.
//
// Usage:
//
//	go run ./tools/gencontracts/cmd/gencontracts            # (re)write docs/contracts/*
//	go run ./tools/gencontracts/cmd/gencontracts -check     # fail on drift (CI gate)
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	gc "github.com/territory-grounder/grounder/tools/gencontracts"
)

const contractsDir = "docs/contracts"

var sourceHashRe = regexp.MustCompile(`source_hash:\s*"([0-9a-f]{64})"`)

func main() {
	check := flag.Bool("check", false, "verify committed contracts have not drifted (CI gate); exit nonzero on drift")
	flag.Parse()

	root := repoRoot()
	model, err := gc.BuildModel()
	if err != nil {
		fatal("build model: %v", err)
	}

	if *check {
		committed, err := os.ReadFile(filepath.Join(root, contractsDir, "openapi.yaml"))
		if err != nil {
			fatal("read committed openapi.yaml (run gencontracts to create it): %v", err)
		}
		m := sourceHashRe.FindSubmatch(committed)
		if m == nil {
			fatal("committed openapi.yaml has no source_hash provenance")
		}
		if string(m[1]) != model.SourceHash() {
			fatal("CONTRACT DRIFT: the served surface changed but docs/contracts was not regenerated (committed %s, fresh %s)", string(m[1])[:12], model.SourceHash()[:12])
		}
		// THE HASH IS NOT THE DOCUMENT. Model.SourceHash() covers routes and entities only — not the
		// emitter's own output. So any change to what Generate WRITES (the securitySchemes block, the
		// OpenAPI scaffolding, a description) leaves the hash identical, this gate prints "no drift",
		// and the committed artifact silently stops matching the generator.
		//
		// Measured: adding the tgMTLS security scheme to the emitter changed the generated document by
		// two lines and this check still reported `no drift — source f4585054ae3d`. The published
		// contract was missing a scheme the generator emits, and nothing said so. That is the same shape
		// as TG-249 item 1 — a gate that verifies something narrower than it appears to.
		//
		// Compare the BODY. generated_at is excluded because it is a timestamp that differs on every
		// run by design; everything else must match byte for byte.
		if err := documentMatchesGenerator(string(committed), model); err != nil {
			fatal("%v", err)
		}
		// ...and the SAME comparison over every other artifact the generator writes (asyncapi + each JSON
		// schema). Without this the check covered 1 of 18 committed contracts.
		if err := allArtifactsMatchGenerator(root, model); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("gencontracts: no drift — %d routes, %d entities, source %s, all %d artifact(s) match\n",
			len(model.Routes), len(model.Entities), model.SourceHash()[:12], 2+len(gc.Generate(model, "").JSONSchemas))
		return
	}

	art := gc.Generate(model, time.Now().UTC().Format(time.RFC3339))
	if err := gc.VerifyCoverage(model, art); err != nil {
		fatal("coverage: %v", err)
	}
	dir := filepath.Join(root, contractsDir)
	must(os.MkdirAll(filepath.Join(dir, "schemas"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "openapi.yaml"), []byte(art.OpenAPI), 0o644))
	must(os.WriteFile(filepath.Join(dir, "asyncapi.yaml"), []byte(art.AsyncAPI), 0o644))
	for table, s := range art.JSONSchemas {
		must(os.WriteFile(filepath.Join(dir, "schemas", table+".schema.json"), []byte(s), 0o644))
	}
	fmt.Printf("gencontracts: wrote %d routes, %d entities to %s (source %s)\n", art.RouteCount, art.EntityCount, contractsDir, art.SourceHash[:12])
}

// documentMatchesGenerator compares the COMMITTED contract body against what the generator emits right
// now. Extracted from main() so the behaviour is testable: guarding normaliseContract alone left the
// mutation "delete the comparison from main()" GREEN, which is the resolver-guarded/wiring-unguarded
// shape this repo keeps hitting.
func documentMatchesGenerator(committed string, model gc.Model) error {
	fresh := gc.Generate(model, "").OpenAPI
	if normaliseContract(committed) == normaliseContract(fresh) {
		return nil
	}
	return fmt.Errorf("CONTRACT DRIFT: docs/contracts/openapi.yaml does not match what the generator " +
		"emits, even though the route/entity source hash is unchanged. Something in the EMITTER changed " +
		"(a security scheme, a description, the scaffolding). Run " +
		"`go run ./tools/gencontracts/cmd/gencontracts` and commit the result.")
}

// allArtifactsMatchGenerator is the SAME body comparison, over EVERY artifact the generator writes — not
// just openapi.yaml.
//
// THE GATE WAS NARROWER THAN IT LOOKED, one level up from the defect its own comment describes. Generate
// emits openapi.yaml, asyncapi.yaml AND one JSON schema per entity (18 committed artifacts); the check
// compared exactly one of them. So an emitter change to the AsyncAPI scaffolding or to any schema left the
// committed artifact stale while this gate printed "document matches" — the published contract silently
// stops matching the generator, which is precisely what INV-15 exists to prevent. (Measured: mutating the
// schema emitter left `gencontracts -check` green.)
//
// It also fails on a MISSING artifact (the generator emits it, nothing is committed) and on an ORPHAN
// schema (a committed schema the generator no longer emits — a retired entity whose contract lingers,
// the "retired-but-present" shape). Every artifact is normalised the same way, so a timestamp is still
// never drift.
func allArtifactsMatchGenerator(root string, model gc.Model) error {
	art := gc.Generate(model, "")
	dir := filepath.Join(root, contractsDir)

	want := map[string]string{
		"openapi.yaml":  art.OpenAPI,
		"asyncapi.yaml": art.AsyncAPI,
	}
	for table, s := range art.JSONSchemas {
		want[filepath.Join("schemas", table+".schema.json")] = s
	}

	for rel, fresh := range want {
		committed, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return fmt.Errorf("CONTRACT DRIFT: the generator emits %s/%s but it is not committed (%v). Run "+
				"`go run ./tools/gencontracts/cmd/gencontracts` and commit the result", contractsDir, rel, err)
		}
		if normaliseContract(string(committed)) != normaliseContract(fresh) {
			return fmt.Errorf("CONTRACT DRIFT: %s/%s does not match what the generator emits, even though the "+
				"route/entity source hash is unchanged. Something in the EMITTER changed. Run "+
				"`go run ./tools/gencontracts/cmd/gencontracts` and commit the result", contractsDir, rel)
		}
	}

	// Orphans: a committed schema the generator no longer emits.
	entries, err := os.ReadDir(filepath.Join(dir, "schemas"))
	if err != nil {
		return fmt.Errorf("read %s/schemas: %w", contractsDir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".schema.json") {
			continue
		}
		table := strings.TrimSuffix(e.Name(), ".schema.json")
		if _, ok := art.JSONSchemas[table]; !ok {
			return fmt.Errorf("CONTRACT DRIFT: %s/schemas/%s is committed but the generator no longer emits a "+
				"schema for %q — a retired entity whose published contract is still shipping. Delete it (and "+
				"regenerate) if the entity is gone", contractsDir, e.Name(), table)
		}
	}
	return nil
}

// normaliseContract drops the one line that legitimately differs between two runs of the generator, so
// the comparison above is about CONTENT. It removes exactly `generated_at` and nothing else — a
// normaliser that stripped more would hide the very drift this gate exists to find.
func normaliseContract(doc string) string {
	var keep []string
	for _, line := range strings.Split(doc, "\n") {
		if strings.Contains(line, "generated_at:") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

func repoRoot() string {
	d, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		p := filepath.Dir(d)
		if p == d {
			return "."
		}
		d = p
	}
}

func must(err error) {
	if err != nil {
		fatal("%v", err)
	}
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "gencontracts: "+f+"\n", a...)
	os.Exit(1)
}
