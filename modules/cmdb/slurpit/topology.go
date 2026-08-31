package slurpit

import (
	"context"
	"regexp"
	"strings"

	"github.com/territory-grounder/grounder/core/estate"
)

// Edges implements estate.EdgeSource: it reads the (bounded) device inventory and turns each discovered
// device into estate topology. A fetch/parse error is returned to estate.Build, which reports it as one
// SourceError without aborting the other sources (per-source isolation) — never a silent empty topology
// presented as truth.
func (s *EstateSource) Edges(ctx context.Context) ([]estate.Edge, error) {
	devices, err := s.fetchDevices(ctx)
	if err != nil {
		return nil, err
	}
	return s.edgesFrom(devices), nil
}

// edgesFrom is the emission the estate refresh and the console TEST button SHARE (SelfTest counts what this
// would draft), so the probe cannot report an edge yield the refresh loop will not deliver.
//
// TWO EDGE KINDS, and both are grounded in confirmed device fields — no invented endpoint:
//
//   - SITE MEMBERSHIP is the PRIMARY, fully-grounded topology. `site` is a confirmed device field; emitting a
//     `member_of` edge is what makes the device a graph NODE (the estate is edge-centric — a node exists only
//     as an edge endpoint) AND records the site the estate's cross-site reconciliation reads (siteIndex). A
//     TypeSite parent is NOT a common-cause sibling parent (siblingParentEligible excludes it — co-location is
//     not co-failure), so this records membership without manufacturing phantom siblings.
//
//   - DEPENDENCY PARENT is a best-effort, HEAVILY GUARDED enrichment. Slurp'it's `parent` is read as an
//     upstream dependency hostname exactly as LibreNMS reads dependency_parent_hostname (topology.go there). A
//     value that is empty, the device itself, or a bare numeric/IP literal — an internal device id, say — is
//     dead weight and skipped (isDeadParent). The guard makes this FAIL-SAFE: it can only add a correct
//     device→parent `depends_on` edge, never seed a phantom node from a non-hostname parent reference.
//
// Devices are typed TypeHost (the generic node), matching the LibreNMS network-device convention: a Slurp'it
// device may be a switch, router or firewall, and name-based Resolve merges it with a more-specific
// NetBox/PVE/LibreNMS node of the same name rather than leaving a disconnected typed twin. Both emitted
// triples — (host, member_of, site) and (host, depends_on, host) — are in DefaultEdgeSchema, so nothing is
// counted as an unknown triple.
func (s *EstateSource) edgesFrom(devices []slurpitDevice) []estate.Edge {
	var edges []estate.Edge
	for _, d := range devices {
		name := deviceName(d)
		if name == "" {
			continue // no usable identity — a missing node is safer than a nameless one
		}
		if site := strings.TrimSpace(d.Site); site != "" {
			edges = append(edges, estate.Edge{
				From:   estate.Entity{Type: estate.TypeHost, Name: name},
				To:     estate.Entity{Type: estate.TypeSite, Name: site},
				Rel:    estate.RelMemberOf,
				Source: estate.SourceSlurpit,
				// No ExpectedAlerts: a site is a grouping, not a cascading parent, so "a cascade along this edge
				// fires alert X" is not a claim this edge makes.
			})
		}
		if parent := strings.TrimSpace(d.Parent); !isDeadParent(parent, name) {
			edges = append(edges, estate.Edge{
				From:           estate.Entity{Type: estate.TypeHost, Name: name},
				To:             estate.Entity{Type: estate.TypeHost, Name: parent},
				Rel:            estate.RelDependsOn,
				Source:         estate.SourceSlurpit,
				ExpectedAlerts: s.expected,
			})
		}
	}
	return edges
}

// deviceName is the device's canonical identity: its hostname, or its fqdn when the hostname is blank (the
// estate's canonName strips the domain either way, so the two forms resolve to the same node). Empty when the
// record carries neither — such a device has no usable identity and is skipped by the caller.
func deviceName(d slurpitDevice) string {
	if n := strings.TrimSpace(d.Hostname); n != "" {
		return n
	}
	return strings.TrimSpace(d.Fqdn)
}

// numericOrIPRE matches a bare numeric / IP-literal token (digits and dots only) — an internal device id or a
// parent known only by address. It mirrors the LibreNMS seed-time guard (`re.fullmatch(r"[\d.]+")`).
var numericOrIPRE = regexp.MustCompile(`^[0-9.]+$`)

// isDeadParent reports whether a `parent` value must NOT become a dependency edge: it is empty, the device
// itself (a self-loop), or a bare numeric/IP literal (an internal id or bare address the estate could never
// triage — a phantom node). Anything else is treated as a parent hostname. This is what keeps the best-effort
// `parent` read fail-safe: an unresolvable parent reference is dropped, never turned into a phantom node.
func isDeadParent(parent, self string) bool {
	return parent == "" || strings.EqualFold(parent, self) || numericOrIPRE.MatchString(parent)
}
