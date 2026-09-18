package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// THE READ-ONLY TRANSPORT FOR ESTATE DISCOVERY (spec/027 plane 2).
//
// modules/discovery/{systemd,docker} each declare a consumer-side Runner — Run(ctx, host, argv) ([]byte,
// error) — and both were linked into NO BINARY. Searching the tree for "modules/discovery" outside those
// packages returned nothing at all, so the two packages that are the ONLY producers of estate.TypeService
// had no path to a composition root.
//
// core/worldmodel/manifest.go routes exclusively TypeService to KindUnit and KindContainer, so two of the
// three adoption kinds could never receive a drafted entry — while the world.discovery seam reported LIVE
// and the boot log said "world discovery: armed every 30m over N source(s)". A lane live in one kind of
// three, with nothing recording the other two. spec/027 tasks.json marks T-027-3 ("Module registration for
// the two discovery sources") as completed against a registration that did not exist.
//
// The transport is the SAME fail-closed path the journal actor-evidence reader uses, deliberately not a new
// one: host allowlist, then the credential engine, then the native host-key-verified SSH runner. Nothing
// here constructs argv — each discovery package holds its enumeration as a package CONSTANT precisely so a
// discovery source can never become "an execution path wearing a reader's name", and this adapter passes
// that constant through untouched.
type discoveryRunner struct {
	allow    map[string]bool
	resolver *credential.AuditedResolver
	runner   syslogng.Runner
	timeout  time.Duration
}

// discoveryHostAllow bounds a host label to the same shape the journal reader accepts (journal.go:141): no
// path traversal, no leading dash, no shell metacharacter. A target can never be read as a flag or a path.
var discoveryHostAllow = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,99}$`)

// newDiscoveryRunner builds the transport over an operator-declared host allowlist.
//
// An empty allowlist yields NIL, and the caller must then construct no source at all. A source built over
// a runner that refuses every host would contribute zero edges and read as "these hosts run nothing" —
// indistinguishable, downstream, from a genuinely serviceless estate.
func newDiscoveryRunner(hosts []string, resolver *credential.AuditedResolver, knownHostsPath string, timeout time.Duration) *discoveryRunner {
	allow := map[string]bool{}
	for _, h := range hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" && discoveryHostAllow.MatchString(h) {
			allow[h] = true
		}
	}
	if len(allow) == 0 || resolver == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &discoveryRunner{
		allow:    allow,
		resolver: resolver,
		// An empty known_hosts path yields a runner that REFUSES every read rather than one that skips
		// host-key verification: the failure direction is a missing observation, never an unverified host.
		runner:  syslogng.NewNativeRunner(strings.TrimSpace(knownHostsPath)),
		timeout: timeout,
	}
}

// hostList returns the allowlisted hosts, sorted, for constructing the discovery sources. Sorted because
// Go randomizes map iteration and a source's edge order should not change between boots.
func (d *discoveryRunner) hostList() []string {
	if d == nil {
		return nil
	}
	out := make([]string, 0, len(d.allow))
	for h := range d.allow {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// Run satisfies both systemd.Runner and docker.Runner — the two consumer-side interfaces are identical, so
// one transport serves both without either package learning about the other.
//
// A non-zero REMOTE exit is a RESULT, not an error: `docker ps` on a host that has no docker exits non-zero,
// and that means "no containers here", not "the read failed". Stdout is returned either way and each
// discovery parser ignores what it cannot parse.
func (d *discoveryRunner) Run(ctx context.Context, host string, argv []string) ([]byte, error) {
	if d == nil {
		return nil, fmt.Errorf("discovery: no read-only runner configured")
	}
	h := strings.ToLower(strings.TrimSpace(host))
	if !discoveryHostAllow.MatchString(h) || strings.Contains(h, "..") {
		return nil, fmt.Errorf("discovery: target %q is not a valid host label", host)
	}
	if !d.allow[h] {
		return nil, fmt.Errorf("discovery: host %q is not in the operator discovery allowlist (fail closed)", h)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("discovery: empty argv")
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	bundle, err := d.resolver.Resolve(ctx, credential.Target{Host: h})
	if err != nil {
		return nil, fmt.Errorf("discovery: no resolvable read-only SSH credential for %q (fail closed): %w", h, err)
	}
	res, err := d.runner.Run(ctx, syslogng.Server{SSHHost: h, SSHUser: bundle.User(), KeyRef: bundle.SSHKeyRef()}, argv)
	if err != nil {
		return nil, fmt.Errorf("discovery: %s: %w", h, err)
	}
	return res.Stdout, nil
}

// yieldingEdgeSource counts a discovery probe's yield AT THE POINT IT RUNS — inside estate.Build — so the
// seam-yield register sees (hosts probed, edges returned) without anyone paying for a second round of SSH.
//
// The alternative considered and rejected: re-invoking Edges() from the refresh loop to count it. That is
// the observer paying the observed cost twice, on every refresh, against real machines.
//
// A probe that reaches every host and returns nothing is exactly what discovery.service exists to detect,
// and it is invisible in an estate edge total dominated by netbox and librenms.
type yieldingEdgeSource struct {
	inner   estate.EdgeSource
	hosts   int
	observe func(hosts, edges int)
}

func (y yieldingEdgeSource) Source() estate.Source { return y.inner.Source() }

func (y yieldingEdgeSource) Edges(ctx context.Context) ([]estate.Edge, error) {
	edges, err := y.inner.Edges(ctx)
	// Observed on the ERROR path too: a probe that fails every host has produced nothing, and that is the
	// finding — not a reason to skip recording it.
	if y.observe != nil {
		y.observe(y.hosts, len(edges))
	}
	return edges, err
}
