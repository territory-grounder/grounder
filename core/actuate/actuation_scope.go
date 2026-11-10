package actuate

import (
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/manifest"
)

func manifestTarget(host string) manifest.Action { return manifest.Action{Target: host} }

// ActuationScope is an optional host allowlist enforced on the actuation plane at the effect path. It is read
// from that plane's own environment, so nothing the triage plane writes can widen it. It bounds every lane the
// same way because the interceptor is the one chokepoint they all pass through. An action whose host cannot be
// named is outside the scope.
type ActuationScope struct {
	hosts map[string]struct{}
}

// ParseActuationScope reads a comma, semicolon or whitespace separated host list. An empty list returns nil,
// which leaves the chain unchanged.
func ParseActuationScope(raw string) *ActuationScope {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' })
	hosts := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if h := ResourceHost(ResourcePath(manifestTarget(f))); h != "" && h != ResourceKeyNoTarget {
			hosts[h] = struct{}{}
		}
	}
	if len(hosts) == 0 {
		return nil
	}
	return &ActuationScope{hosts: hosts}
}

// RefuseAllScope admits no host. The worker installs it when a scope is required and none was given.
func RefuseAllScope() *ActuationScope {
	return &ActuationScope{hosts: map[string]struct{}{}}
}

// Admits reports whether the resource host is inside the scope.
func (s *ActuationScope) Admits(host string) bool {
	if s == nil {
		return false
	}
	_, ok := s.hosts[host]
	return ok
}

// Len is the number of hosts in the scope.
func (s *ActuationScope) Len() int {
	if s == nil {
		return 0
	}
	return len(s.hosts)
}

// Hosts lists the scope in sorted order for the boot line.
func (s *ActuationScope) Hosts() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.hosts))
	for h := range s.hosts {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}
