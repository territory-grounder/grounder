package main

// THE REGISTRY LEARNS THE WHOLE CATALOG (TG-267).
//
// TG-251 built the channel — the worker publishes its module enablement and the API reads it through a
// staleness cutoff — and the first live publish exposed the deeper gap: the registry held 18 pairs while
// the catalog describes 29, because most connectors (every credsource, both discovery probes, the
// knowledge sources, cmdb/pve, syslogng, pve-liveness…) are constructed in this file's composition root
// without ever registering. The projection can only be as complete as the registry it projects, so 25 of
// 29 modules still rendered "state not reported here".
//
// Two halves, both catalog-named (the descriptor is the naming authority):
//
//   - CONSTRUCTED ⇒ ENABLED: the probe registry's offer() is the one chokepoint that already sees every
//     construction with its (surface, sourceType, instance) — TG-252 put it at all the sites this ticket
//     would otherwise have to edit one by one. A declare hook rides it and registers each offered pair
//     as enabled.
//   - DESCRIBED-BUT-NOT-CONSTRUCTED ⇒ DECLARED OFF: after assembly, every catalog pair still absent is
//     registered disabled. "The worker looked and it is off" and "nobody can see it" stay distinct
//     states — which was the entire point of the three-state render.
//
// ORDERING IS DELIBERATE: both halves run AFTER bootstrap.Reconcile. The reconcile pin
// (TG_EXPECTED_CAPABILITIES) governs the bootstrap families it has always governed; declaring the full
// catalog BEFORE it would refuse boot on any deployment that pins today's 18.

import (
	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/catalog"
)

// catalogDeclared is the sentinel adapter for a described-but-not-constructed module. Registry.Register
// refuses a nil adapter (an execution path must be a real thing), and this type is deliberately
// incapable of anything — it exists so absence can be REPRESENTED without being executable.
type catalogDeclared struct{}

// declareConstructed registers an offered construction as an enabled module under its catalog pair.
//
// Pairs the bootstrap families already registered (notifier/matrix, tracker/youtrack, cmdb/netbox, …)
// hit ErrDuplicateSource here — correct and ignored: the bootstrap registration carries the accurate
// enablement the operator configured, and a construction-time offer must not overwrite it. SetEnabled is
// NOT called on duplicates for the same reason.
func declareConstructed(reg *modules.Registry) func(surface, sourceType string, v any) {
	return func(surface, sourceType string, v any) {
		if v == nil {
			v = catalogDeclared{}
		}
		_ = reg.Register(modules.Registration{
			Surface: surface, SourceType: sourceType,
			Capability: surface + "." + sourceType,
			Enabled:    true,
			Adapter:    v,
		})
	}
}

// declareOffered replays the probe registry's identity set into the module registry — the constructions
// that were offered BEFORE the declare hook was wired (main builds syslogng and the librenms tool set at
// the top, long before moduleReg exists). Replaying the set rather than only hooking future offers is
// what keeps this immune to construction-order: a module moved earlier in main cannot fall out of the
// projection because of where it sits.
func declareOffered(reg *modules.Registry, probes *probeRegistry) int {
	if probes == nil {
		return 0
	}
	declare := declareConstructed(reg)
	before := len(reg.Capabilities())
	for key := range probes.seen {
		surface, sourceType, ok := splitPair(key)
		if !ok {
			continue
		}
		declare(surface, sourceType, probes.probers[key])
	}
	return len(reg.Capabilities()) - before
}

// splitPair splits a "surface/source_type" identity. Source types contain no slash (they are slugs), so
// the FIRST separator is the boundary.
func splitPair(key string) (string, string, bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' && i > 0 && i+1 < len(key) {
			return key[:i], key[i+1:], true
		}
	}
	return "", "", false
}

// declareCatalogAbsent registers every catalog descriptor pair the registry still lacks as
// declared-but-disabled, and returns how many it added. Runs once, after module assembly, so the first
// capability-projection publish carries the whole catalog.
func declareCatalogAbsent(reg *modules.Registry) int {
	all, err := catalog.All()
	if err != nil {
		return 0 // a malformed catalog is a test failure elsewhere; never a boot failure here
	}
	present := map[string]bool{}
	for _, c := range reg.Capabilities() {
		present[c.Surface+"/"+c.SourceType] = true
	}
	added := 0
	for _, d := range all {
		if present[d.Surface+"/"+d.SourceType] {
			continue
		}
		if reg.Register(modules.Registration{
			Surface: d.Surface, SourceType: d.SourceType,
			Capability: d.Surface + "." + d.SourceType,
			Enabled:    false,
			Adapter:    catalogDeclared{},
		}) == nil {
			added++
		}
	}
	return added
}
