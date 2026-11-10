package main

import (
	"context"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/skillstore"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// wireSessionStamps attaches the two per-session observability stamps once the activity deps exist: the harness
// render store (TG-600) and the instrument identity (TG-672). One call from main() so the composition root stays
// within the TG-501 line ratchet; nothing on the decision path reads either.
func wireSessionStamps(deps *runner.Deps, gw *model.Gateway, skillRows func(context.Context) ([]skillstore.ProductionRow, error)) {
	wireHarnessRender(deps)
	wireInstrumentIdentity(deps, gw, skillRows)
}
