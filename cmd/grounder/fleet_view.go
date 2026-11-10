package main

// ONE MERGED FLEET VIEW, READ BY EVERY SURFACE THAT CLAIMS TO DESCRIBE THE FLEET (TG-268).
//
// The Modules page told the operator: "11 of 15 declared capabilities are ENABLED. A disabled capability
// has no execution path regardless of what it declares — that is the load-bearing fact on this view." That
// 15 came from /v1/capabilities reading this process's OWN registry: 4 surfaces, blind to every
// worker-resident connector. Directly beneath it the same page rendered 29 module dialogs across 10
// families, because those come from /v1/modules/schema, which TG-251 and TG-267 had already re-pointed at
// the merged view. A safety claim, sourced from a registry that could not see 14 of the things it was
// making the claim about.
//
// THE PATTERN IS THE POINT. This is the third appearance of one defect: the machinery gets fixed and a
// consumer nobody re-pointed keeps serving the partial view (TG-251, TG-267, this). So the merge moves
// here, both surfaces call it, and a test asserts they cannot disagree — rather than a third patch that
// leaves a fourth reader waiting to be found.

import (
	"context"
	"sort"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/modules"
)

// fleetView answers "which modules exist here, and which are switched on" from the local registry UNION
// the worker's published projection, read through a staleness cutoff.
type fleetView struct {
	reg *modules.Registry
	// projection reads the worker-published capability rows (TG-251). Nil ⇒ local registry only, which is
	// exactly the pre-TG-251 behaviour and never a lie: those modules simply stay unknown.
	projection func(ctx context.Context) ([]db.CapabilityProjectionRow, error)
	// staleAfter bounds how long a projected row keeps answering after its publisher stops refreshing.
	staleAfter time.Duration
	now        func() time.Time
}

// fleetEntry is one module's identity and observed state. `known` separates "this process looked and the
// answer was no" from "nobody can see it" — the distinction the console's three-state render depends on.
type fleetEntry struct {
	surface, sourceType, capability string
	enabled, known                  bool
}

func (f fleetView) key(surface, sourceType string) string { return surface + "/" + sourceType }

// entries returns the merged fleet, keyed by surface/source_type.
//
// LOCAL WINS ON OVERLAP: a process's own registry is ground truth for what runs IN it; the projection
// answers only for modules that live elsewhere. A projected row past the cutoff is treated as ABSENT —
// unknown is the honest answer once the publisher has stopped vouching for it.
func (f fleetView) entries(ctx context.Context) map[string]fleetEntry {
	out := map[string]fleetEntry{}
	if f.reg != nil {
		for _, c := range f.reg.Capabilities() {
			out[f.key(c.Surface, c.SourceType)] = fleetEntry{
				surface: c.Surface, sourceType: c.SourceType, capability: c.Capability,
				enabled: c.Enabled, known: true,
			}
		}
	}
	if f.projection == nil {
		return out
	}
	rows, err := f.projection(ctx)
	if err != nil {
		// Degrade to the local view rather than erroring the surface: a config-plane blip must not blank
		// a page whose other content is still true.
		return out
	}
	now := time.Now
	if f.now != nil {
		now = f.now
	}
	cutoff := f.staleAfter
	if cutoff <= 0 {
		cutoff = 3 * time.Minute
	}
	for _, r := range rows {
		k := f.key(r.Surface, r.SourceType)
		if _, local := out[k]; local {
			continue // local wins
		}
		if now().Sub(r.ObservedAt) > cutoff {
			continue // stale ⇒ absent ⇒ unknown
		}
		out[k] = fleetEntry{
			surface: r.Surface, sourceType: r.SourceType, capability: r.Capability,
			enabled: r.Enabled, known: true,
		}
	}
	return out
}

// Capabilities satisfies httpapi.CapabilitiesReader with the MERGED fleet, so /v1/capabilities stops
// reporting a count that contradicts the dialogs rendered beneath it.
//
// The interface takes no context (it is a pure declaration view); a background context with the same
// bounded read the projection store already performs is correct here — this is a read of one small table.
func (f fleetView) Capabilities() []modules.Capability {
	m := f.entries(context.Background())
	out := make([]modules.Capability, 0, len(m))
	for _, e := range m {
		out = append(out, modules.Capability{
			Surface: e.surface, SourceType: e.sourceType, Capability: e.capability, Enabled: e.enabled,
		})
	}
	// Stable order (surface, then source) so the console renders deterministically — the registry's own
	// contract for this view.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Surface != out[j].Surface {
			return out[i].Surface < out[j].Surface
		}
		return out[i].SourceType < out[j].SourceType
	})
	return out
}
