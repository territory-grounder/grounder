package main

import (
	"log"

	"github.com/territory-grounder/grounder/core/buildinfo"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// harnessRenderStore is the rendering behind each harness fingerprint (TG-600, migration 0117), set by
// newStepStores where the pool is live and read by wireHarnessRender once deps exists — the same
// package-level-var handoff newStepStores uses for experienceStore, because the pool's scope and deps's
// scope do not overlap in main(). Nil when no pool is connected on this plane.
var harnessRenderStore *db.HarnessRenderStore

// wireHarnessRender attaches the render store to the activity deps on a DB-bearing plane and logs the
// posture (TG-600, Harness² H2-1c). Observability only; nothing on the decision path reads the record.
func wireHarnessRender(deps *runner.Deps) {
	if harnessRenderStore == nil {
		log.Printf("harness render: OFF — no database pool on this plane; fingerprints still persist, renderings do not (build %s)", buildinfo.Describe())
		return
	}
	deps.HarnessRenders = harnessRenderStore
	log.Printf("harness render: ON (TG-600) — build %s; each configuration's system half is recorded on its first session (harness_render + build lineage); read/diff with tools/harnessdiff", buildinfo.Describe())
}
