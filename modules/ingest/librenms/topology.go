package librenms

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/estate"
)

// Doer is the minimal HTTP contract; *http.Client satisfies it, and tests inject a fake LibreNMS API.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// EstateSource adapts the configured LibreNMS deployments into an estate.EdgeSource. It reads each device's
// declared `dependency_parent_hostname` — LibreNMS's explicit network-reachability parent — and emits a
// `depends_on` edge (a device depends on the upstream parent it is reached through). This closes
// PORT-FIDELITY-AUDIT P0-1's LibreNMS arm: the module ignored `dependency_parent_hostname` entirely, so this
// authoritative, operator-maintained dependency topology never reached the causal graph.
//
// Both endpoints are typed TypeHost (the generic node): a LibreNMS device may be a server or a switch, and
// name-based Resolve merges it with a more-specific NetBox/PVE node of the same name. The reader is
// READ-ONLY, per-deployment, and per-source-isolated — a deployment that fails to fetch aborts the whole
// LibreNMS contribution (returned to estate.Build as one SourceError), never a silent partial topology.
type EstateSource struct {
	deployments []Deployment
	http        Doer
	expected    []string // alerts a cascade along a dependency edge is expected to fire
}

// TopoOption configures an EstateSource.
type TopoOption func(*EstateSource)

// WithTopologyHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithTopologyHTTPClient(d Doer) TopoOption { return func(s *EstateSource) { s.http = d } }

// NewEstateSource builds a LibreNMS topology source over the configured deployments. expectedAlerts are
// stamped on every emitted edge so the verifier's "partial" branch has per-edge content.
func NewEstateSource(deployments []Deployment, opts ...TopoOption) *EstateSource {
	s := &EstateSource{deployments: deployments, http: http.DefaultClient}
	for _, o := range opts {
		o(s)
	}
	return s
}

// WithExpectedAlerts stamps the given cascade alerts on every emitted edge.
func WithExpectedAlerts(alerts ...string) TopoOption {
	return func(s *EstateSource) { s.expected = alerts }
}

// Source implements estate.EdgeSource.
func (s *EstateSource) Source() estate.Source { return estate.SourceLibreNMS }

type deviceList struct {
	Devices []struct {
		Hostname                 string `json:"hostname"`
		DependencyParentHostname string `json:"dependency_parent_hostname"`
	} `json:"devices"`
}

// Edges implements estate.EdgeSource: it fetches each deployment's device list and emits a `depends_on` edge
// from every device that declares one or more dependency parents (LibreNMS may store a CSV of parents).
func (s *EstateSource) Edges(ctx context.Context) ([]estate.Edge, error) {
	var edges []estate.Edge
	for _, d := range s.deployments {
		list, err := s.fetchDevices(ctx, d)
		if err != nil {
			return nil, fmt.Errorf("librenms[%s]: %w", d.Site, err)
		}
		for _, dev := range list.Devices {
			child := strings.TrimSpace(dev.Hostname)
			if child == "" {
				continue
			}
			for _, parent := range strings.Split(dev.DependencyParentHostname, ",") {
				parent = strings.TrimSpace(parent)
				if parent == "" || parent == child || isIPLiteral(parent) {
					// no parent, a self-loop, or an IP-literal parent. An IP-literal dependency_parent is dead
					// weight (a phantom host node + cascade edges to a bare address the estate never triages);
					// the predecessor drops these at seed time (infragraph-seed.py's `re.fullmatch(r"[\d.]+")`).
					continue
				}
				edges = append(edges, estate.Edge{
					From:           estate.Entity{Type: estate.TypeHost, Name: child},
					To:             estate.Entity{Type: estate.TypeHost, Name: parent},
					Rel:            estate.RelDependsOn,
					Source:         estate.SourceLibreNMS,
					ExpectedAlerts: s.expected,
				})
			}
		}
	}
	return edges, nil
}

// fetchDevices issues an authenticated GET against a deployment's LibreNMS API. LibreNMS uses an
// X-Auth-Token header; the token is resolved from its secret reference at call time (INV-13).
func (s *EstateSource) fetchDevices(ctx context.Context, d Deployment) (deviceList, error) {
	token, err := config.SecretRef(d.TokenRef).Resolve()
	if err != nil {
		return deviceList{}, fmt.Errorf("resolve token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(d.BaseURL, "/")+"/api/v0/devices", nil)
	if err != nil {
		return deviceList{}, err
	}
	req.Header.Set("X-Auth-Token", token)
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return deviceList{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return deviceList{}, fmt.Errorf("GET devices: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var list deviceList
	if err := json.Unmarshal(body, &list); err != nil {
		return deviceList{}, fmt.Errorf("malformed devices response: %w", err)
	}
	return list, nil
}

// ipLiteralRE matches a bare numeric/IP-literal parent (digits and dots only) — the predecessor's
// `re.fullmatch(r"[\d.]+", parent)` seed-time guard.
var ipLiteralRE = regexp.MustCompile(`^[0-9.]+$`)

// isIPLiteral reports whether a dependency-parent hostname is a bare IP literal (or otherwise all-numeric),
// which LibreNMS sometimes reports when a device's parent is only known by address. Such a parent is dead
// weight in the estate graph (a phantom node the estate can never triage), so its edges are dropped.
func isIPLiteral(parent string) bool { return ipLiteralRE.MatchString(parent) }

// compile-time proof the topology reader satisfies the estate edge-source seam.
var _ estate.EdgeSource = (*EstateSource)(nil)
