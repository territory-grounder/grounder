// Package systemd is a READ-ONLY systemd-unit discovery source for the auto-drafted world model
// (spec/027 REQ-2701, epic TG-227 plane 2).
//
// It enumerates the services a host actually runs and contributes `runs_on` edges (service depends on its
// host), so an operator can ADOPT a unit as an actuation target instead of hand-typing it into
// TG_ACTUATION_ALLOWED_UNITS. Discovery proposes; adoption grants; the leaf default-deny gate in
// modules/actuation/ssh still refuses everything it was not handed — this source cannot widen actuation.
//
// It is DISTINCT from the ssh ACTUATION module: the transport here is an injected read-only command runner
// (a fake in oracles, a read-only ssh runner in production), it issues exactly one non-mutating
// `systemctl list-units` per host, and it has no gate, no argv construction, and no execution path.
package systemd

import (
	"context"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/estate"
)

// SourceType is the vendor slug this source serves (INV-17 registration).
const SourceType = "systemd-discovery"

// The fixed, non-mutating enumeration this source is allowed to issue. It is a CONSTANT, not a parameter:
// a discovery source that could be handed an arbitrary command would be an execution path wearing a
// reader's name.
var listUnitsArgv = []string{
	"systemctl", "list-units", "--type=service", "--state=loaded",
	"--no-legend", "--no-pager", "--plain",
}

// Runner is the minimal READ-ONLY command transport this source needs (consumer-side interface, the repo
// idiom). Production injects a read-only ssh runner; oracles inject a fake. It returns the command's
// stdout; a non-zero exit is an error, which Build isolates per-source.
type Runner interface {
	Run(ctx context.Context, host string, argv []string) ([]byte, error)
}

// Source enumerates systemd units across the declared hosts. Construct with New.
type Source struct {
	hosts []string
	run   Runner
}

// Option configures a Source.
type Option func(*Source)

// WithRunner injects the read-only command transport.
func WithRunner(r Runner) Option { return func(s *Source) { s.run = r } }

// New builds a systemd discovery source over the declared hosts (config-not-code: no host is compiled in).
func New(hosts []string, opts ...Option) *Source {
	s := &Source{}
	for _, h := range hosts {
		if h = strings.TrimSpace(h); h != "" {
			s.hosts = append(s.hosts, h)
		}
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Source implements estate.EdgeSource. Discovery contributes at the DECLARED tier (0.85): a unit observed
// running is a strong fact, but it is weaker than the hypervisor's own placement record (pve 0.95) and it
// must never outrank ground truth on a shared edge (REQ-2706, the MAX-ratchet decides).
func (s *Source) Source() estate.Source { return estate.SourceDeclared }

// Edges implements estate.EdgeSource: one read-only `systemctl list-units` per host yields the loaded
// services; each becomes a `runs_on` edge from the service to its host.
//
// A host that fails to enumerate is reported and the OTHERS still contribute — the per-source isolation
// estate.Build provides, applied per-host inside the source, so one unreachable machine degrades to
// "its units are missing" and never to a silent empty estate that would read as "this host runs nothing".
func (s *Source) Edges(ctx context.Context) ([]estate.Edge, error) {
	if s.run == nil {
		return nil, fmt.Errorf("systemd discovery: no runner injected")
	}
	var edges []estate.Edge
	var failures []string
	for _, host := range s.hosts {
		out, err := s.run.Run(ctx, host, listUnitsArgv)
		if err != nil {
			failures = append(failures, host+": "+err.Error())
			continue
		}
		for _, unit := range ParseUnits(string(out)) {
			edges = append(edges, estate.Edge{
				From:       estate.Entity{Type: estate.TypeService, Name: unit},
				To:         estate.Entity{Type: estate.TypeHost, Name: host},
				Rel:        estate.RelRunsOn,
				Source:     estate.SourceDeclared,
				Confidence: estate.SourceConfidence[estate.SourceDeclared],
			})
		}
	}
	if len(failures) > 0 {
		// Loud, and NOT swallowed: the edges gathered from healthy hosts are returned alongside the error
		// so the caller can report the gap while still using what was observed.
		return edges, fmt.Errorf("systemd discovery: %d host(s) failed: %s", len(failures), strings.Join(failures, "; "))
	}
	return edges, nil
}

// ParseUnits extracts unit names from `systemctl list-units --no-legend --plain` output. Exported so the
// oracle asserts the parse against REAL systemctl output shape rather than a paraphrase of it.
//
// Only names ending in `.service` are taken: a discovery source that guessed at sockets, timers, or mounts
// would draft targets an operator could adopt into an allowlist whose leaf only ever restarts services.
func ParseUnits(out string) []string {
	var units []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		// `list-units` marks degraded units with a leading bullet; strip it rather than drafting "●".
		if name == "●" || name == "*" {
			if len(fields) < 2 {
				continue
			}
			name = fields[1]
		}
		if !strings.HasSuffix(name, ".service") || seen[name] {
			continue
		}
		seen[name] = true
		units = append(units, name)
	}
	return units
}
