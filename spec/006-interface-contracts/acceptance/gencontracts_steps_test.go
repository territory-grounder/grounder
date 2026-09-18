package acceptance

import (
	"fmt"

	"github.com/cucumber/godog"

	gc "github.com/territory-grounder/grounder/tools/gencontracts"
)

// registerGenContractsSteps binds the REQ-501b generated-contracts scenario to the real gencontracts
// library (BuildModel/Generate/VerifyCoverage) — the same code the CI drift gate runs.
func registerGenContractsSteps(sc *godog.ScenarioContext) {
	var model gc.Model
	var art gc.Artifacts

	sc.Step(`^the router's registered routes and the canonical Postgres entities$`, func() error {
		m, err := gc.BuildModel()
		model = m
		return err
	})
	sc.Step(`^the contract generator emits openapi\.yaml, asyncapi\.yaml, and the JSON Schemas$`, func() error {
		art = gc.Generate(model, "2026-07-15T00:00:00Z")
		return nil
	})
	sc.Step(`^every routed endpoint is present with a declared auth and error schema$`, func() error {
		if len(model.Routes) == 0 {
			return fmt.Errorf("no routes enumerated to cover")
		}
		if err := gc.VerifyCoverage(model, art); err != nil {
			return err
		}
		// error schemas: the generated OpenAPI declares 401 + 404 for the surface.
		for _, code := range []string{`"401"`, `"404"`} {
			if !contains(art.OpenAPI, code) {
				return fmt.Errorf("generated OpenAPI is missing the %s error response", code)
			}
		}
		return nil
	})
	sc.Step(`^the artifact carries a non-null generated_at, source hash, and coverage scope$`, func() error {
		if art.GeneratedAt == "" || art.SourceHash == "" || art.CoverageScope == "" {
			return fmt.Errorf("artifact missing provenance: %+v", art)
		}
		return nil
	})
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
