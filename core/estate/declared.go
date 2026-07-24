package estate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
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
	TypePhysicalHost: {}, TypePVENode: {}, TypeVM: {}, TypeLXC: {}, TypeNetworkDevice: {},
	TypeTunnel: {}, TypeSite: {}, TypeService: {}, TypeHost: {},
}

var knownRelTypes = map[RelType]struct{}{
	RelRunsOn: {}, RelMemberOf: {}, RelDependsOn: {}, RelRoutesVia: {},
}

// ParseRelType maps a relation string to its declared RelType and reports whether it is recognised. An empty
// string is the legitimate generic default (depends_on, ok=true) — the same convention ParseDeclared uses. A
// NON-empty string outside the declared vocabulary (runs_on, member_of, depends_on, routes_via) returns
// (RelDependsOn, false): the caller decides whether to REJECT it (as ParseDeclared does) or coerce+count it.
// Matching is case-insensitive. Centralising the vocabulary here binds every relation parser — declared
// config AND the eval snapshot loader — to the same knownRelTypes set, so an ontology boundary violation
// cannot be silently coerced in one path while it is rejected in another (the eval/discovery.go relOf gap).
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
