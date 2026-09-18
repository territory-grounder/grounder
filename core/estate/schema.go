package estate

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// EDGE-TRIPLE SCHEMA VALIDATION (TG-207).
//
// Upsert accepted ANY (From.Type, Rel, To.Type). The only type-legality logic in the package ran at READ
// time, inside the Siblings walk (siblingParentEligible), so a malformed adapter could silently install an
// illegal edge that then participates in blast-radius prediction — the deterministic model every autonomous
// actuation depends on.
//
// OBSERVE-ONLY BY DEFAULT, AND THAT IS THE WHOLE DESIGN. Refusing unknown triples on day one would be a
// fail-closed gate switched on against a config nobody has migrated: the adjacency table below is derived
// from what the running estate actually contains PLUS what the adapters can emit, and neither source is
// proof of completeness. Measured 2026-08-06, the live graph holds exactly three triples —
// host/depends_on/host (1541), vm/runs_on/pve_node (211), lxc/runs_on/pve_node (112) — while the adapters
// also construct member_of and routes_via edges that happen to be absent from that snapshot. A table built
// from the live graph alone would have started dropping site membership and tunnel routing on the first
// deploy, silently, inside the blast-radius input.
//
// So: an unknown triple is COUNTED and reported, never dropped. Enforcement is a separate, deliberate flip
// once the observed-illegal count has sat at zero long enough to trust the table.
type EdgeSchema struct {
	mu      sync.RWMutex
	allowed map[string]bool
	// unknown records every triple seen that the table does not list, with a count. It is the evidence the
	// enforce flip needs, and it is why this type refuses nothing yet.
	unknown map[string]int
}

func tripleKey(from EntityType, rel RelType, to EntityType) string {
	return string(from) + "|" + string(rel) + "|" + string(to)
}

// DefaultEdgeSchema is the union of what the running estate contains and what the adapters construct.
//
// It is a DEFAULT, not a law: NewEdgeSchema takes an operator-supplied table (loadable-not-hardcoded, the
// standing rule), and this is what a deployment that supplies nothing gets.
func DefaultEdgeSchema() []string {
	return []string{
		// Measured in the live graph 2026-08-06.
		tripleKey(TypeHost, RelDependsOn, TypeHost),
		tripleKey(TypeVM, RelRunsOn, TypePVENode),
		tripleKey(TypeLXC, RelRunsOn, TypePVENode),
		// Constructed by adapters, absent from that particular snapshot. Listing them from the CODE rather
		// than from the data is the point: a table derived from one snapshot silently outlaws whatever the
		// estate happened not to be doing that day.
		tripleKey(TypeHost, RelMemberOf, TypeSite),
		tripleKey(TypeNetworkDevice, RelMemberOf, TypeSite),
		tripleKey(TypePVENode, RelMemberOf, TypeSite),
		tripleKey(TypeVM, RelMemberOf, TypeSite),
		tripleKey(TypeLXC, RelMemberOf, TypeSite),
		tripleKey(TypeService, RelMemberOf, TypeSite),
		// A VM's membership in a NetBox virtualization cluster grouping (TG-390) — the fallback when a VM
		// carries no per-VM device, emitted as member_of a TypeCluster rather than runs_on a fake pve_node.
		tripleKey(TypeVM, RelMemberOf, TypeCluster),
		tripleKey(TypeHost, RelRoutesVia, TypeTunnel),
		tripleKey(TypeNetworkDevice, RelRoutesVia, TypeTunnel),
		tripleKey(TypeSite, RelRoutesVia, TypeTunnel),
		tripleKey(TypeTunnel, RelRoutesVia, TypeSite),
		// Physical hosting and service placement.
		tripleKey(TypePVENode, RelRunsOn, TypePhysicalHost),
		tripleKey(TypeHost, RelRunsOn, TypePhysicalHost),
		// vSphere emits VM→host directly: an ESXi host IS the physical hypervisor, so a vCenter VM runs on a
		// physical_host with no intermediate pve_node layer (TG-91).
		tripleKey(TypeVM, RelRunsOn, TypePhysicalHost),
		tripleKey(TypeService, RelRunsOn, TypeHost),
		tripleKey(TypeService, RelDependsOn, TypeHost),
		tripleKey(TypeService, RelDependsOn, TypeService),
		tripleKey(TypeHost, RelDependsOn, TypeNetworkDevice),
		tripleKey(TypeNetworkDevice, RelDependsOn, TypeNetworkDevice),
		tripleKey(TypeStorageAppliance, RelDependsOn, TypeNetworkDevice), // the NAS hangs off the switch (TG-78 storage slice)
		tripleKey(TypeHost, RelDependsOn, TypeStorageAppliance),          // hosts mount its shares — the blast radius edge
	}
}

// NewEdgeSchema builds the validator. An empty table yields a schema that allows everything and says so —
// a schema nobody configured must not silently become a schema that rejects everything.
func NewEdgeSchema(triples []string) *EdgeSchema {
	s := &EdgeSchema{allowed: make(map[string]bool, len(triples)), unknown: map[string]int{}}
	for _, t := range triples {
		if t = strings.TrimSpace(t); t != "" {
			s.allowed[t] = true
		}
	}
	return s
}

// Check records the triple and reports whether the table lists it.
//
// It NEVER refuses — the caller admits the edge either way. The bool is for the observer, and for the
// enforce flip that will one day read it.
func (s *EdgeSchema) Check(from EntityType, rel RelType, to EntityType) bool {
	if s == nil {
		return true
	}
	k := tripleKey(from, rel, to)
	s.mu.RLock()
	empty := len(s.allowed) == 0
	ok := s.allowed[k]
	s.mu.RUnlock()
	if empty || ok {
		return true
	}
	s.mu.Lock()
	s.unknown[k]++
	s.mu.Unlock()
	return false
}

// UnknownTriples returns what was seen and not listed, sorted, with counts. This is the evidence the
// enforce flip needs: it must read zero, over a real estate, for a while, before rejection is safe.
func (s *EdgeSchema) UnknownTriples() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.unknown))
	for k, n := range s.unknown {
		out = append(out, fmt.Sprintf("%s (%d)", k, n))
	}
	sort.Strings(out)
	return out
}

// UnknownCount is the gauge value: how many DISTINCT unlisted triples have been seen.
func (s *EdgeSchema) UnknownCount() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.unknown)
}

// WithSchema attaches the triple validator to a graph. Observe-only — see EdgeSchema.
func (g *Graph) WithSchema(s *EdgeSchema) *Graph {
	g.schema = s
	return g
}

// Schema exposes the attached validator so an observer can publish UnknownCount without reaching into the
// graph's internals.
func (g *Graph) Schema() *EdgeSchema { return g.schema }
