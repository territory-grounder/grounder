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
	"sync"
	"time"

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
	now      func() time.Time // wall clock for the guest-state observation stamp (TG-496); time.Now in prod

	// The last successfully-parsed guest POWER STATES (TG-378). The estate graph is placement-only by
	// construction — no node state lives there — yet the very response this source already reads carries
	// each guest's `status`, and discarding it left "is guest X running?" unanswerable estate-wide while
	// TG proposed `start` on guests with 2,000-hour uptimes. The states ride the SAME fetch (no second
	// HTTP call, no second credential) and are exposed via GuestStates for the liveness projection.
	mu         sync.Mutex
	lastStates []GuestState
	statesSeen bool
}

// GuestState is one guest's power state as the hypervisor reported it, verbatim ("running", "stopped",
// "paused", ...). An empty Status is a guest the API listed without a status field — recorded as observed
// with an unknown state, never guessed into a state.
type GuestState struct {
	Guest  string
	Node   string
	Status string
	// ObservedAt is when this source fetched /cluster/resources (the observation time). It is threaded into
	// the guest_liveness projection so the store's monotone upsert guard can order this 5-min sweep against
	// the ~37s pve-liveness detector's writes (TG-496): without a true observation time the slower sweep's
	// LATER write would clobber the detector's fresher STOPPED during a down-transition.
	ObservedAt time.Time
}

// Option configures an EstateSource.
type Option func(*EstateSource)

// WithHTTPClient injects the HTTP transport (a fake in tests, an *http.Client in production — the caller
// supplies the TLS policy for PVE's self-signed endpoints).
func WithHTTPClient(d Doer) Option { return func(s *EstateSource) { s.http = d } }

// WithExpectedAlerts stamps the given cascade alerts on every emitted edge (per-edge verifier content).
func WithExpectedAlerts(alerts ...string) Option {
	return func(s *EstateSource) { s.expected = alerts }
}

// WithClock overrides the wall clock so the guest-state observation stamp (TG-496 monotone liveness guard)
// is deterministic under test. Production uses time.Now.
func WithClock(now func() time.Time) Option { return func(s *EstateSource) { s.now = now } }

// New builds a PVE topology source for a base URL (e.g. "https://dc1pve01:8006") and an API-token secret
// reference resolving to a full `user@realm!tokenid=secret` value.
func New(baseURL string, tokenRef config.SecretRef, opts ...Option) *EstateSource {
	s := &EstateSource{baseURL: strings.TrimRight(baseURL, "/"), tokenRef: tokenRef, http: http.DefaultClient, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Source implements estate.EdgeSource.
func (s *EstateSource) Source() estate.Source { return estate.SourcePVE }

// clusterResourcesPath is the ONE read this source issues. It is a constant rather than a literal at the
// call site so the estate refresh and the console's TEST button cannot drift onto different endpoints: a
// probe that exercised a different path from the reader would certify a permission the reader never uses.
const clusterResourcesPath = "/api2/json/cluster/resources?type=vm"

// clusterResources is the /cluster/resources envelope, and its Data field is a POINTER on purpose.
//
// Every PVE API response carries a `data` key; a body that parses as JSON but has no `data` at all is not a
// PVE answer, and that is a DIFFERENT fault from a cluster this token may see nothing in — it is the shape a
// reverse proxy's JSON error, an SSO redirect rendered as JSON, or an entirely different product returns. A
// plain slice would decode all of those to nil and report them, indistinguishably, as an empty cluster.
type clusterResources struct {
	Data *[]struct {
		Type   string `json:"type"` // "lxc" | "qemu" | "node" | "storage" | ...
		Node   string `json:"node"` // the hypervisor node the guest is placed on
		Name   string `json:"name"`
		Status string `json:"status"` // "running" | "stopped" | ... — the guest power state (TG-378)
	} `json:"data"`
}

// parseClusterResources decodes the cluster-resources envelope and REFUSES a body that is not one.
//
// The refusal is the point. An endpoint that answers 2xx with JSON carrying no `data` key — a gateway, an
// SSO portal, another product entirely — decodes to zero guests, and zero guests is exactly what a real
// cluster this token may not see returns. Reporting the two identically would let a base URL pointed at the
// wrong system read as an authorised but empty cluster: silently no edges in the refresh loop, and a
// qualified PASS on the console's TEST button telling the operator to grant PVEAuditor on a machine that is
// not Proxmox.
func parseClusterResources(body []byte) (clusterResources, error) {
	var res clusterResources
	if err := json.Unmarshal(body, &res); err != nil {
		return clusterResources{}, fmt.Errorf("pve: malformed cluster resources: %w", err)
	}
	if res.Data == nil {
		return clusterResources{}, fmt.Errorf("pve: malformed cluster resources: no `data` envelope in %d bytes", len(body))
	}
	return res, nil
}

// Edges implements estate.EdgeSource: one authenticated GET of the cluster resources yields every guest with
// its placement node; each named lxc/qemu guest becomes a `runs_on` edge to its node. A guest without a
// resolvable name or node is skipped (a missing edge is safer than a guessed one).
func (s *EstateSource) Edges(ctx context.Context) ([]estate.Edge, error) {
	body, err := s.get(ctx, clusterResourcesPath)
	if err != nil {
		return nil, err
	}
	return s.edgesFrom(body)
}

// edgesFrom is the parse Edges and the self-test SHARE, so the probe reports what the refresh loop would
// actually draft rather than a second opinion about the same bytes.
func (s *EstateSource) edgesFrom(body []byte) ([]estate.Edge, error) {
	res, err := parseClusterResources(body)
	if err != nil {
		return nil, err
	}
	observedAt := s.now() // one fetch/observation time stamped on every state (TG-496 monotone guard)
	var edges []estate.Edge
	var states []GuestState
	for _, r := range *res.Data {
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
		states = append(states, GuestState{Guest: name, Node: node, Status: strings.TrimSpace(r.Status), ObservedAt: observedAt})
	}
	// Cache the states only on a successful parse — a refused body must not blank the last good reading
	// (the reader's staleness bound, not a parse failure here, is what retires it).
	s.mu.Lock()
	s.lastStates, s.statesSeen = states, true
	s.mu.Unlock()
	return edges, nil
}

// GuestStates returns the guest power states from the last successful cluster-resources parse, and whether
// a sweep has completed at all. ok=false means "this source has not yet read the cluster" — the caller must
// treat that as unknown, never as "no guests" (TG-378: absent is not stopped).
func (s *EstateSource) GuestStates() ([]GuestState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.statesSeen {
		return nil, false
	}
	out := make([]GuestState, len(s.lastStates))
	copy(out, s.lastStates)
	return out, true
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
