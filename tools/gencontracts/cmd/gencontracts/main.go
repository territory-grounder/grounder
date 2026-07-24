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
		fmt.Printf("gencontracts: no drift — %d routes, %d entities, source %s\n", len(model.Routes), len(model.Entities), model.SourceHash()[:12])
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
