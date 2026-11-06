package estate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
)

// DeclaredEdge is one operator-declared topology edge — the data shape of the declared-estate config an
// administrator maintains (a JSON array). It lets the operator ADD or reinforce dependencies the live
// discovery sources miss (a cross-site tunnel, a service→host relationship no CMDB models). Declared edges
// carry SourceDeclared (0.85), so a LIVE source (PVE 0.95, NetBox/LibreNMS 0.90) ALWAYS out-ranks a declared
// one on the same edge via the MAX-ratchet: "live devices state is the source of truth", and the operator
// declaration fills the gaps rather than overriding observed reality.
type DeclaredEdge struct {
	From           string     `json:"from"`
	FromType       EntityType `json:"from_type"`
	To             string     `json:"to"`
	ToType         EntityType `json:"to_type"`
	Rel            RelType    `json:"rel"`
	ExpectedAlerts []string   `json:"expected_alerts,omitempty"`
}

// DeclaredSource is an estate.EdgeSource over operator-declared edges. The edges are parsed from config at
// the composition root (ParseDeclared) and handed in already-typed, so the source itself does no I/O and is
// pure — the worker owns the file read, the source owns only the contribution.
type DeclaredSource struct{ edges []Edge }

// NewDeclaredSource wraps a set of already-parsed, SourceDeclared-stamped edges as an edge source.
func NewDeclaredSource(edges []Edge) *DeclaredSource { return &DeclaredSource{edges: edges} }

// Source implements EdgeSource.
func (s *DeclaredSource) Source() Source { return SourceDeclared }

// Edges implements EdgeSource.
func (s *DeclaredSource) Edges(context.Context) ([]Edge, error) { return s.edges, nil }

// knownEntityTypes / knownRelTypes are the accepted vocabularies for a declared edge. A declaration naming a
// type or relation outside these is REJECTED (loud), not silently coerced — a typo'd declared edge must fail
// the load, never seed a phantom dependency.
var knownEntityTypes = map[EntityType]struct{}{
	TypePhysicalHost: {}, TypePVENode: {}, TypeVM: {}, TypeLXC: {}, TypeNetworkDevice: {}, TypeStorageAppliance: {},
	TypeTunnel: {}, TypeSite: {}, TypeService: {}, TypeHost: {},
}

var knownRelTypes = map[RelType]struct{}{
	RelRunsOn: {}, RelMemberOf: {}, RelDependsOn: {}, RelRoutesVia: {},
}

// unknownRelationTotal counts, process-wide, every time a NON-EMPTY relation string OUTSIDE the declared
// vocabulary reaches the shared parser — an ontology boundary violation. It is OBSERVE-ONLY and monotonic:
// counting a violation NEVER changes whether the caller then rejects it (ParseDeclared, the eval snapshot
// loader in eval/discovery.go) or coerces it to the generic depends_on; it only makes the violation visible.
// It is surfaced live on the worker as the Prometheus counter tg_estate_unknown_relation_total (see
// cmd/worker/estate_size.go). This is the carved-out prerequisite of the Siblings edge-type discovery loop:
// a relation the co-failure data implies but the causal ontology (runs_on/depends_on, plus declared
// member_of/routes_via) cannot represent shows up here first, so the residual starts accumulating now rather
// than only once the loop is built. (TG-179, epic TG-175 — ontology losability.)
var unknownRelationTotal atomic.Int64

// UnknownRelationCount returns the process-lifetime total of unrecognised-relation encounters at the declared
// vocabulary parser. Monotonic; safe to read concurrently against the parser's writes.
func UnknownRelationCount() int64 { return unknownRelationTotal.Load() }

// ParseRelType maps a relation string to its declared RelType and reports whether it is recognised. Matching
// is LENIENT: leading/trailing whitespace is trimmed and comparison is case-insensitive. An empty (or
// whitespace-only) string is the legitimate generic default (depends_on, ok=true). A NON-empty string outside
// the declared vocabulary (runs_on, member_of, depends_on, routes_via) returns (RelDependsOn, false) AND is
// COUNTED (unknownRelationTotal, surfaced as tg_estate_unknown_relation_total) so the ontology boundary
// violation is visible; the caller — the eval snapshot loader in eval/discovery.go — still decides whether to
// REJECT it or coerce it to the generic depends_on. Counting does not decide reject-vs-coerce.
//
// ParseDeclared does NOT route its accept/reject decision through here: the operator declared-estate config is
// a LIVE ingest chokepoint, so it keeps a STRICTER, case-sensitive knownRelTypes lookup (byte-equivalent to
// origin/main — a typo like "RUNS_ON" or a whitespace-only rel still HARD-rejects, seeding no phantom edge)
// and increments the SAME counter in its own reject branch. Both paths thus leave a signal on a boundary
// violation while keeping their own deliberate leniency; the shared knownRelTypes set binds both to one
// vocabulary (the fix for the old relOf gap that silently coerced member_of/routes_via into depends_on).
func ParseRelType(s string) (RelType, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return RelDependsOn, true
	}
	for k := range knownRelTypes {
		if strings.EqualFold(t, string(k)) {
			return k, true
		}
	}
	// Ontology boundary violation — a relation the declared vocabulary cannot represent. Count it (observe-only)
	// so it is visible on /metrics whether the caller rejects or coerces it. (TG-179.)
	unknownRelationTotal.Add(1)
	return RelDependsOn, false
}

// ParseDeclared reads the declared-estate JSON (an array of DeclaredEdge) into estate edges stamped with
// SourceDeclared. Endpoints are required; an empty endpoint type defaults to TypeHost (the generic node) and
// an empty relation defaults to RelDependsOn (the generic dependency). A malformed entry — an empty
// endpoint, or a type/relation outside the known vocabulary — is REJECTED with an error rather than silently
// dropped, so a broken operator declaration is loud, never a quiet gap presented as complete truth.
func ParseDeclared(r io.Reader) ([]Edge, error) {
	var decls []DeclaredEdge
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&decls); err != nil {
		return nil, fmt.Errorf("estate: malformed declared-estate JSON: %w", err)
	}
	edges := make([]Edge, 0, len(decls))
	for i, d := range decls {
		from, to := strings.TrimSpace(d.From), strings.TrimSpace(d.To)
		if from == "" || to == "" {
			return nil, fmt.Errorf("estate: declared edge %d: from and to are required", i)
		}
		ft, tt, rel := d.FromType, d.ToType, d.Rel
		if ft == "" {
			ft = TypeHost
		}
		if tt == "" {
			tt = TypeHost
		}
		if rel == "" {
			rel = RelDependsOn
		}
		if _, ok := knownEntityTypes[ft]; !ok {
			return nil, fmt.Errorf("estate: declared edge %d: unknown from_type %q", i, ft)
		}
		if _, ok := knownEntityTypes[tt]; !ok {
			return nil, fmt.Errorf("estate: declared edge %d: unknown to_type %q", i, tt)
		}
		if _, ok := knownRelTypes[rel]; !ok {
			// COUNT the ontology boundary violation on the LIVE declared-estate ingest path (observe-only),
			// then REJECT. This accept/reject decision is byte-equivalent to origin/main's STRICT, case-
			// sensitive knownRelTypes lookup (no TrimSpace; "" is the only default), so a case variant like
			// "RUNS_ON" and a whitespace-only rel still HARD-reject and no phantom edge is seeded — the counter
			// records the violation WITHOUT widening what ingest accepts. (ParseRelType, used by the eval
			// snapshot loader, is deliberately more lenient and counts its own violations; each parser counts
			// what IT would reject.) (TG-179.)
			unknownRelationTotal.Add(1)
			return nil, fmt.Errorf("estate: declared edge %d: unknown rel %q", i, rel)
		}
		edges = append(edges, Edge{
			From:           Entity{Type: ft, Name: from},
			To:             Entity{Type: tt, Name: to},
			Rel:            rel,
			Source:         SourceDeclared,
			ExpectedAlerts: d.ExpectedAlerts,
		})
	}
	return edges, nil
}

// compile-time proof the declared source satisfies the edge-source seam.
var _ EdgeSource = (*DeclaredSource)(nil)
