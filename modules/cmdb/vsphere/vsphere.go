// Package vsphere is a read-only VMware vSphere / vCenter topology source for the causal estate graph
// (spec/008, TG-91). It reads virtual-machine placement from a vCenter Server via the official govmomi SDK
// and emits `runs_on` edges — a VM depends on the physical ESXi host it is placed on. This is a hypervisor
// authority ALONGSIDE Proxmox (pve): both read placement from the live hypervisor. vSphere carries a high
// live-hypervisor confidence (0.94) — one notch below pve's 0.95 and DISTINCT from it per the SourceConfidence
// tie-contract, yet well above the 0.80 ground-truth cutoff; vSphere and pve describe DISJOINT guests, so
// their ranks never actually compete on an edge. Unlike Proxmox — where a guest runs on a `pve_node` that in
// turn runs on a physical host — an ESXi host IS the physical hypervisor, so a vSphere VM runs DIRECTLY on a
// TypePhysicalHost (schema triple vm→runs_on→physical_host, DefaultEdgeSchema).
//
// READ-ONLY. It lists inventory and never actuates — Phase-1-safe by construction, distinct in every way
// from an actuation module. The password is a secret reference resolved per refresh (INV-13), never a
// literal, and it is DISTINCT from the username so a credential rotation touches only the sealed half.
//
// A NON-VCENTER ENDPOINT CANNOT READ AS AN EMPTY CLUSTER. The pve source must explicitly refuse a
// 2xx-with-no-`data` body because a gateway or SSO portal decodes to zero guests indistinguishably from an
// authorised-but-empty cluster. govmomi removes that whole failure class for free: NewClient performs a SOAP
// handshake and a SessionManager login, so a base URL pointed at anything that is not a vCenter fails to
// CONNECT — a loud error, never a silent empty refresh and never a false PASS on a console TEST button.
//
// Provenance: [O] INV-13, spec/008 · [F] TG-91 (estate-discovery source family).
package vsphere

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/estate"
)

// SourceType is the vendor slug this source serves. It is the provenance the estate graph stamps; it must be
// distinct from every other source so the graph (and the correlator's cross-source rule) can tell them apart.
const SourceType = "vsphere"

// EstateSource reads vCenter VM placement and contributes `runs_on` edges. Construct with New.
type EstateSource struct {
	baseURL  string
	username string
	passRef  config.SecretRef
	insecure bool
	expected []string
}

// Option configures an EstateSource.
type Option func(*EstateSource)

// WithInsecureTLS skips TLS certificate verification — required for a vCenter (or the vcsim test simulator)
// presenting a self-signed certificate. Default is verify-ON: an operator opts INTO insecure explicitly, so
// a cert-verification hole is a decision on the record, not a silent default.
func WithInsecureTLS(v bool) Option { return func(s *EstateSource) { s.insecure = v } }

// WithExpectedAlerts stamps the given cascade alerts on every emitted edge (per-edge verifier content),
// mirroring the pve source.
func WithExpectedAlerts(alerts ...string) Option {
	return func(s *EstateSource) { s.expected = alerts }
}

// New builds a vSphere topology source for a vCenter base URL (e.g. "https://vcenter.example.com" — govmomi
// appends the /sdk path), a login username (e.g. "svc-tg@vsphere.local"), and a PASSWORD secret reference
// resolved per refresh (INV-13). Username is plain config; only the password is sealed.
func New(baseURL, username string, passRef config.SecretRef, opts ...Option) *EstateSource {
	s := &EstateSource{baseURL: strings.TrimSpace(baseURL), username: strings.TrimSpace(username), passRef: passRef}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Source implements estate.EdgeSource.
func (s *EstateSource) Source() estate.Source { return estate.SourceVsphere }

// Edges implements estate.EdgeSource: it logs in to vCenter, retrieves every VM with its placement host and
// every host's name in one container-view pass, and turns each placed VM into a `runs_on` edge to its ESXi
// host. A connect/login/read failure is RETURNED (never a silent empty result) because an unreadable
// hypervisor is not an empty one — the same fail-loud posture pve takes on a refused body.
func (s *EstateSource) Edges(ctx context.Context) ([]estate.Edge, error) {
	pw, err := s.passRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("vsphere: resolve password: %w", err)
	}
	u, err := soap.ParseURL(s.baseURL)
	if err != nil {
		return nil, fmt.Errorf("vsphere: parse url %q: %w", s.baseURL, err)
	}
	if u == nil {
		return nil, fmt.Errorf("vsphere: empty base URL")
	}
	u.User = url.UserPassword(s.username, pw)

	c, err := govmomi.NewClient(ctx, u, s.insecure)
	if err != nil {
		return nil, fmt.Errorf("vsphere: connect %s: %w", u.Host, err)
	}
	// Log out on a FRESH context so the session is released even when the refresh ctx has already expired.
	defer func() { _ = c.Logout(context.Background()) }()

	m := view.NewManager(c.Client)
	v, err := m.CreateContainerView(ctx, c.Client.ServiceContent.RootFolder, []string{"VirtualMachine", "HostSystem"}, true)
	if err != nil {
		return nil, fmt.Errorf("vsphere: create container view: %w", err)
	}
	defer func() { _ = v.Destroy(context.Background()) }()

	// Hosts first, so every VM's placement reference resolves to a name in one pass. Retrieving ONLY the
	// `name` property keeps the read minimal — the same least-read discipline the credential is scoped for.
	var hosts []mo.HostSystem
	if err := v.Retrieve(ctx, []string{"HostSystem"}, []string{"name"}, &hosts); err != nil {
		return nil, fmt.Errorf("vsphere: retrieve hosts: %w", err)
	}
	hostByRef := make(map[types.ManagedObjectReference]string, len(hosts))
	for _, h := range hosts {
		hostByRef[h.Self] = strings.TrimSpace(h.Name)
	}

	var vms []mo.VirtualMachine
	if err := v.Retrieve(ctx, []string{"VirtualMachine"}, []string{"name", "runtime.host", "config.template"}, &vms); err != nil {
		return nil, fmt.Errorf("vsphere: retrieve vms: %w", err)
	}
	return s.edgesFrom(vms, hostByRef), nil
}

// edgesFrom is the pure VM→host mapping. It is separated from the vCenter I/O so a unit test drives the EXACT
// code the refresh loop runs (mirroring pve's edgesFrom/get split) without needing a live endpoint.
//
// It drops three things rather than guess them: a TEMPLATE (a stamped image that does not run — a runs_on
// dependency for it would be a phantom parent in the blast-radius graph), a VM with no resolvable name, and a
// VM whose placement host is absent or unnamed. A missing edge is always safer than a guessed one.
func (s *EstateSource) edgesFrom(vms []mo.VirtualMachine, hostByRef map[types.ManagedObjectReference]string) []estate.Edge {
	var edges []estate.Edge
	for _, vm := range vms {
		if vm.Config != nil && vm.Config.Template {
			continue
		}
		name := strings.TrimSpace(vm.Name)
		if name == "" || vm.Runtime.Host == nil {
			continue
		}
		host := hostByRef[*vm.Runtime.Host]
		if host == "" {
			continue
		}
		edges = append(edges, estate.Edge{
			From:           estate.Entity{Type: estate.TypeVM, Name: name},
			To:             estate.Entity{Type: estate.TypePhysicalHost, Name: host},
			Rel:            estate.RelRunsOn,
			Source:         estate.SourceVsphere,
			ExpectedAlerts: s.expected,
		})
	}
	return edges
}

// compile-time proof the topology reader satisfies the estate edge-source seam.
var _ estate.EdgeSource = (*EstateSource)(nil)
