package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/territory-grounder/grounder/core/estate"
)

// EstateSource adapts the NetBox module into an estate.EdgeSource. It reads virtual-machine placement from
// NetBox and emits `runs_on` edges — a VM depends on the hypervisor host it is placed on — which is the
// primary topology the estate blast-radius reasons over. This closes PORT-FIDELITY-AUDIT P0-1's NetBox arm:
// the module previously resolved entities by id ONLY and contributed no edges, so the causal graph stayed
// empty and every blast radius was vacuous.
//
// The reader is READ-ONLY (a GET against the NetBox REST API through the same injectable Doer + secret-ref
// token the module already uses) and per-source-isolated: a fetch/parse error is returned to estate.Build,
// which reports it without aborting the other sources. Placement without a resolvable host is skipped rather
// than guessed — a missing edge is safer than a wrong one.
type EstateSource struct {
	m              *Module
	expectedAlerts []string // the alerts a cascade from host→VM is expected to fire (e.g. "HostDown")
}

// NewEstateSource wraps a NetBox Module as an estate edge source. expectedAlerts are stamped on every emitted
// edge so the verifier's "partial" branch has per-edge content (the estate carries per-edge expected alerts).
func NewEstateSource(m *Module, expectedAlerts ...string) *EstateSource {
	return &EstateSource{m: m, expectedAlerts: expectedAlerts}
}

// Source implements estate.EdgeSource.
func (s *EstateSource) Source() estate.Source { return estate.SourceNetbox }

// vmPage is the subset of the NetBox virtual-machines list response the topology reader needs. NetBox
// models VM placement two ways: `device` (a specific physical host) is the precise runs_on target; when a
// deployment places VMs on a Proxmox/virtualization CLUSTER instead (the common case — every VM carries a
// `cluster` but no per-VM `device`), the cluster is the placement group the VM depends on. Both are read;
// device wins when present. Emitting the cluster edge closes the gap where a cluster-modelled estate
// (VMs with cluster, no device) produced ZERO edges and left every blast radius vacuous.
type vmPage struct {
	Next    string `json:"next"`
	Results []struct {
		Name   string `json:"name"`
		Device *struct {
			Name string `json:"name"`
		} `json:"device"`
		Cluster *struct {
			Name string `json:"name"`
		} `json:"cluster"`
	} `json:"results"`
}

// Edges implements estate.EdgeSource: it pages through the VM list and emits one `runs_on` edge per VM that
// has a resolvable host. The confidence is left 0 so estate.Build stamps the NetBox source default (0.90).
func (s *EstateSource) Edges(ctx context.Context) ([]estate.Edge, error) {
	var edges []estate.Edge
	// brief=false so `device` is populated; limit keeps each page bounded. Follow `next` for full coverage.
	path := "/api/virtualization/virtual-machines/?limit=200"
	for path != "" {
		body, err := s.m.do(ctx, path)
		if err != nil {
			return nil, err
		}
		var page vmPage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("netbox: malformed virtual-machines page: %w", err)
		}
		for _, vm := range page.Results {
			name := strings.TrimSpace(vm.Name)
			if name == "" {
				continue // unnamed — a missing edge is safer than a guessed one
			}
			// Prefer the precise per-VM host; fall back to the placement cluster. A VM with neither is
			// unplaced and skipped (never guessed). The two fallbacks are NOT the same claim about the world:
			// a per-VM device is a physical placement (runs_on a host); a cluster is a LOGICAL grouping.
			var edge estate.Edge
			switch {
			case vm.Device != nil && strings.TrimSpace(vm.Device.Name) != "":
				edge = estate.Edge{
					From:           estate.Entity{Type: estate.TypeVM, Name: name},
					To:             estate.Entity{Type: estate.TypePhysicalHost, Name: strings.TrimSpace(vm.Device.Name)},
					Rel:            estate.RelRunsOn,
					Source:         estate.SourceNetbox,
					ExpectedAlerts: s.expectedAlerts,
				}
			case vm.Cluster != nil && strings.TrimSpace(vm.Cluster.Name) != "":
				// TG-390: a NetBox virtualization cluster is a LOGICAL placement group, not a physical
				// hypervisor. Emitting it as (vm runs_on pve_node) let a cluster — even a Synology DSM cluster —
				// impersonate a Proxmox node (133 children, more than any real hypervisor) and kept
				// HasGroundTruth true when the real per-node edge was gone. Emit it as MEMBERSHIP in a TypeCluster
				// grouping instead: it still records "this VM belongs to this cluster" without asserting a
				// placement fact, without manufacturing common-cause siblings, and without counting as ground truth.
				edge = estate.Edge{
					From:           estate.Entity{Type: estate.TypeVM, Name: name},
					To:             estate.Entity{Type: estate.TypeCluster, Name: strings.TrimSpace(vm.Cluster.Name)},
					Rel:            estate.RelMemberOf,
					Source:         estate.SourceNetbox,
					ExpectedAlerts: s.expectedAlerts,
				}
			default:
				continue
			}
			edges = append(edges, edge)
		}
		path = nextPath(page.Next)
	}
	return edges, nil
}

// nextPath reduces NetBox's absolute `next` pagination URL to the path+query the module's do() expects
// (do prepends the configured base URL). An empty or unparseable next ends pagination.
func nextPath(next string) string {
	next = strings.TrimSpace(next)
	if next == "" {
		return ""
	}
	u, err := url.Parse(next)
	if err != nil {
		return ""
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

// compile-time proof the topology reader satisfies the estate edge-source seam.
var _ estate.EdgeSource = (*EstateSource)(nil)
