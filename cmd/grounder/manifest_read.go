package main

import (
	"context"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/worldmodel"
)

// manifestLister is the narrow slice of db.WorldManifestStore the review surface consumes (consumer-side
// interface, the repo idiom) so the composition oracle can drive the adapter with a fake and stay
// always-on.
//
// THE STAGE-1 LESSON, APPLIED: /v1/proposals shipped fully tested and permanently 503 because Deps was
// never populated at the composition root — every unit oracle green while the product surface was dead,
// and the fail-closed design made the deadness look intentional.
//
// The compiler only catches HALF of that: dropping the argument from the buildPublicAPI call is an arity
// error, but threading it in and then never assigning it into the Deps literal compiles clean and serves a
// permanent 503. Go does not flag an unused parameter. That surviving half is caught by an oracle that
// serves a signed request through the router main builds (see manifest_read_test.go) — proven falsifiable
// by deleting the Deps.Manifest assignment, which reproduces the 503 verbatim.
type manifestLister interface {
	AllEntries(ctx context.Context, limit int) ([]worldmodel.Entry, int, int, error)
}

// manifestReadStore adapts the world-manifest store to the console read surface (spec/027 REQ-2703). The
// principal is accepted and ignored, like every other read adapter here: the surface is read-only and auth
// happens at the router.
type manifestReadStore struct{ s manifestLister }

func (r manifestReadStore) ManifestEntries(ctx context.Context, _ auth.Principal, limit int) ([]worldmodel.Entry, int, int, error) {
	return r.s.AllEntries(ctx, limit)
}

// compile-time proof the pgx store satisfies what the adapter needs.
var _ manifestLister = (*db.WorldManifestStore)(nil)

// compile-time proof the adapter satisfies the surface's contract.
var _ httpapi.ManifestReader = manifestReadStore{}
