package estate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Tunnel is one declared network tunnel: an Endpoint (a gateway/firewall terminating the tunnel) and the
// remote hosts that Route through it. A tunnel is GROUND TRUTH — it exists in the network configuration or it
// does not — so its edges carry the highest confidence tier (SourceTunnel 1.0), above every discovered
// source. If the endpoint fails, every host routing through it is unreachable, so each remote host
// `routes_via` the endpoint (the remote depends on the tunnel).
type Tunnel struct {
	Endpoint       string   `json:"endpoint"`
	Routes         []string `json:"routes"`
	ExpectedAlerts []string `json:"expected_alerts,omitempty"`
}

// TunnelSource is an estate.EdgeSource over declared tunnels — the TOP confidence tier. It emits a
// `routes_via` edge (remote → endpoint) for every host that routes through a tunnel endpoint, so a cross-site
// VPS whose only path is a firewall tunnel is correctly placed in that firewall's blast radius (the reason
// the verifier must NOT exclude an unknown/empty-site cascade — a genuine tunnel cascade would be lost).
type TunnelSource struct{ tunnels []Tunnel }

// NewTunnelSource wraps declared tunnels as an edge source.
func NewTunnelSource(tunnels []Tunnel) *TunnelSource { return &TunnelSource{tunnels: tunnels} }

// Source implements EdgeSource.
func (s *TunnelSource) Source() Source { return SourceTunnel }

// Edges implements EdgeSource: each (route → endpoint) pair becomes a routes_via edge. Confidence is left 0
// so Build stamps the SourceTunnel policy default (1.0). A self-route or a nameless endpoint/route is skipped.
func (s *TunnelSource) Edges(context.Context) ([]Edge, error) {
	var edges []Edge
	for _, t := range s.tunnels {
		endpoint := strings.TrimSpace(t.Endpoint)
		if endpoint == "" {
			continue
		}
		for _, route := range t.Routes {
			route = strings.TrimSpace(route)
			if route == "" || route == endpoint {
				continue
			}
			edges = append(edges, Edge{
				From:           Entity{Type: TypeHost, Name: route},
				To:             Entity{Type: TypeTunnel, Name: endpoint},
				Rel:            RelRoutesVia,
				Source:         SourceTunnel,
				ExpectedAlerts: t.ExpectedAlerts,
			})
		}
	}
	return edges, nil
}

// ParseTunnels reads a JSON array of Tunnel definitions (operator-declared network paths). A tunnel with no
// endpoint or no routes is rejected loudly — a half-declared tunnel is a config error, not a silent gap.
func ParseTunnels(r io.Reader) ([]Tunnel, error) {
	var tunnels []Tunnel
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tunnels); err != nil {
		return nil, fmt.Errorf("estate: malformed tunnel JSON: %w", err)
	}
	for i, t := range tunnels {
		if strings.TrimSpace(t.Endpoint) == "" {
			return nil, fmt.Errorf("estate: tunnel %d: endpoint is required", i)
		}
		if len(t.Routes) == 0 {
			return nil, fmt.Errorf("estate: tunnel %d (%s): at least one route is required", i, t.Endpoint)
		}
	}
	return tunnels, nil
}

// compile-time proof the tunnel source satisfies the edge-source seam.
var _ EdgeSource = (*TunnelSource)(nil)
