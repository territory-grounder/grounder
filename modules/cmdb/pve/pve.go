// Package pve is a read-only Proxmox VE topology source for the causal estate graph (spec/008, P0-1).
//
// It reads guest placement from the PVE cluster API and emits `runs_on` edges — an LXC/VM depends on the
// hypervisor node it is placed on — the highest-confidence estate relationship (SourcePVE 0.95: the live
// hypervisor is the source of truth for what runs where). It is DISTINCT from the proxmox ACTUATION module
// (which drives reboots via a Runner and ships OFF): this is a GET-only reader behind an injectable Doer, so
// the oracle drives the real code path against a fake PVE, and the API token is a secret reference resolved
// per request, never a literal (INV-13).
//
// Provenance: [O] INV-13, spec/008 · [F] the predecessor pve-placement estate seed.
package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/estate"
)

// SourceType is the vendor slug this source serves.
const SourceType = "pve"

// Doer is the minimal HTTP contract; *http.Client satisfies it, and tests inject a fake PVE cluster.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// EstateSource reads PVE guest placement and contributes `runs_on` edges. Construct with New.
type EstateSource struct {
	baseURL  string
	tokenRef config.SecretRef
	http     Doer
	expected []string
}

// Option configures an EstateSource.
type Option func(*EstateSource)

// WithHTTPClient injects the HTTP transport (a fake in tests, an *http.Client in production — the caller
// supplies the TLS policy for PVE's self-signed endpoints).
func WithHTTPClient(d Doer) Option { return func(s *EstateSource) { s.http = d } }

// WithExpectedAlerts stamps the given cascade alerts on every emitted edge (per-edge verifier content).
func WithExpectedAlerts(alerts ...string) Option { return func(s *EstateSource) { s.expected = alerts } }

// New builds a PVE topology source for a base URL (e.g. "https://dc1pve01:8006") and an API-token secret
// reference resolving to a full `user@realm!tokenid=secret` value.
func New(baseURL string, tokenRef config.SecretRef, opts ...Option) *EstateSource {
	s := &EstateSource{baseURL: strings.TrimRight(baseURL, "/"), tokenRef: tokenRef, http: http.DefaultClient}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Source implements estate.EdgeSource.
func (s *EstateSource) Source() estate.Source { return estate.SourcePVE }

type clusterResources struct {
	Data []struct {
		Type string `json:"type"` // "lxc" | "qemu" | "node" | "storage" | ...
		Node string `json:"node"` // the hypervisor node the guest is placed on
		Name string `json:"name"`
	} `json:"data"`
}

// Edges implements estate.EdgeSource: one authenticated GET of the cluster resources yields every guest with
// its placement node; each named lxc/qemu guest becomes a `runs_on` edge to its node. A guest without a
// resolvable name or node is skipped (a missing edge is safer than a guessed one).
func (s *EstateSource) Edges(ctx context.Context) ([]estate.Edge, error) {
	body, err := s.get(ctx, "/api2/json/cluster/resources?type=vm")
	if err != nil {
		return nil, err
	}
	var res clusterResources
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("pve: malformed cluster resources: %w", err)
	}
	var edges []estate.Edge
	for _, r := range res.Data {
		name, node := strings.TrimSpace(r.Name), strings.TrimSpace(r.Node)
		if name == "" || node == "" {
			continue
		}
		fromType := estate.TypeVM
		if r.Type == "lxc" {
			fromType = estate.TypeLXC
		}
		edges = append(edges, estate.Edge{
			From:           estate.Entity{Type: fromType, Name: name},
			To:             estate.Entity{Type: estate.TypePVENode, Name: node},
			Rel:            estate.RelRunsOn,
			Source:         estate.SourcePVE,
			ExpectedAlerts: s.expected,
		})
	}
	return edges, nil
}

// get issues an authenticated GET against the PVE API. PVE uses a "PVEAPIToken=<token>" Authorization
// scheme; the token is resolved from its secret reference at call time (INV-13). A non-2xx is an error.
func (s *EstateSource) get(ctx context.Context, path string) ([]byte, error) {
	token, err := s.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("pve: resolve token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+token)
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("pve: GET %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// compile-time proof the topology reader satisfies the estate edge-source seam.
var _ estate.EdgeSource = (*EstateSource)(nil)
