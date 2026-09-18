// Package docker is a READ-ONLY container discovery source for the auto-drafted world model
// (spec/027 REQ-2701, epic TG-227 plane 2).
//
// It enumerates the containers a host actually runs and contributes `runs_on` edges (container depends on
// its host), so an operator can ADOPT a container as an actuation target instead of hand-typing it into
// TG_ACTUATION_ALLOWED_CONTAINERS. Discovery proposes; adoption grants; the leaf default-deny gate still
// refuses everything it was not handed — this source cannot widen actuation.
//
// The transport is an injected read-only command runner: exactly one non-mutating `docker ps` per host, no
// gate, no argv construction from input, no execution path.
package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/estate"
)

// SourceType is the vendor slug this source serves (INV-17 registration).
const SourceType = "docker-discovery"

// The fixed, non-mutating enumeration this source is allowed to issue — a constant, never a parameter.
// `--format {{.Names}}` keeps the parse honest: one name per line, no column-width guessing.
var listContainersArgv = []string{"docker", "ps", "--format", "{{.Names}}"}

// Runner is the minimal READ-ONLY command transport this source needs (consumer-side interface).
type Runner interface {
	Run(ctx context.Context, host string, argv []string) ([]byte, error)
}

// Source enumerates docker containers across the declared hosts. Construct with New.
type Source struct {
	hosts []string
	run   Runner
}

// Option configures a Source.
type Option func(*Source)

// WithRunner injects the read-only command transport.
func WithRunner(r Runner) Option { return func(s *Source) { s.run = r } }

// New builds a docker discovery source over the declared hosts (config-not-code).
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

// Source implements estate.EdgeSource. Like the systemd source, discovery contributes at the DECLARED tier
// (0.85) — an observed container is a strong fact but never outranks the hypervisor's placement record.
func (s *Source) Source() estate.Source { return estate.SourceDeclared }

// Edges implements estate.EdgeSource: one read-only `docker ps` per host; each running container becomes a
// `runs_on` edge to its host. A host that fails to enumerate is reported loudly while the others still
// contribute — one unreachable machine must never read as "this host runs nothing".
func (s *Source) Edges(ctx context.Context) ([]estate.Edge, error) {
	if s.run == nil {
		return nil, fmt.Errorf("docker discovery: no runner injected")
	}
	var edges []estate.Edge
	var failures []string
	for _, host := range s.hosts {
		out, err := s.run.Run(ctx, host, listContainersArgv)
		if err != nil {
			failures = append(failures, host+": "+err.Error())
			continue
		}
		for _, name := range ParseContainers(string(out)) {
			edges = append(edges, estate.Edge{
				From:       estate.Entity{Type: estate.TypeService, Name: name},
				To:         estate.Entity{Type: estate.TypeHost, Name: host},
				Rel:        estate.RelRunsOn,
				Source:     estate.SourceDeclared,
				Confidence: estate.SourceConfidence[estate.SourceDeclared],
			})
		}
	}
	if len(failures) > 0 {
		return edges, fmt.Errorf("docker discovery: %d host(s) failed: %s", len(failures), strings.Join(failures, "; "))
	}
	return edges, nil
}

// ParseContainers extracts container names from `docker ps --format {{.Names}}` output — one per line.
// Exported so the oracle asserts against the real output shape.
func ParseContainers(out string) []string {
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		// `--format {{.Names}}` emits exactly one field; anything with whitespace is not a container name
		// (a stray header or an error line leaking into stdout) and is skipped rather than drafted.
		if name == "" || seen[name] || len(strings.Fields(name)) != 1 {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}
