package actuate

// RESOURCE PATH (TG-664, G4A finding F6). The durable target admission (gate 4h2) and the commit-confirm window
// used to key on the action's free-text Target. Two proposals for ONE guest — "start guest X on node N" expressed
// as Target=N,params.guest=X and as Target=X — held two different claims, so two heals of the same object could
// overlap, and a window could never tell that a later execution had moved the resource under its armed inverse.
//
// A resource is named by a PATH derived from the sealed action's op-class FAMILY and its canonical params:
//
//	guest-lifecycle      → <params.guest | target>              (a guest IS a host: its name is its hostname)
//	service-lifecycle    → <target>/unit/<params.unit>
//	container-lifecycle  → <target>/container/<params.container>
//	anything else        → <target>
//
// Two paths CONFLICT when one is a prefix of the other (Overlaps): a host or guest claim covers every unit and
// container on it, so a stale stop-guest inverse can never undo a later unit heal inside that guest, and a
// whole-host action waits for a unit heal on the same host. Two different units on one host do not conflict.
// Names are lower-cased and trimmed so "Web01" and "web01" are one resource, and a "/" inside a name is escaped so
// a name can never forge an extra path segment. Pure and deterministic: the same sealed action always yields the
// same path; an action the registry does not know keeps the whole-target path (the widest, safest claim).

import (
	"net"
	"strings"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/manifest"
)

// ResourceKeyNoTarget is the shared bucket for an action that names no resource at all (the admission's existing
// behaviour for an empty target: one claim for every such action, which is the safe direction). It overlaps only
// itself.
const ResourceKeyNoTarget = "(no-target)"

// ResourcePath derives the admission/claim path of a sealed action (see the package comment above).
func ResourcePath(a manifest.Action) string {
	target := hostSegment(a.Target)
	family := ""
	if spec, ok := opschema.Lookup(a.OpClass); ok {
		family = spec.Family
	}
	switch family {
	case opschema.FamilyGuestLifecycle:
		if g := hostSegment(a.Params[opschema.ParamGuest]); g != "" {
			return g
		}
	case opschema.FamilyServiceLifecycle:
		// "nginx" and "nginx.service" name one systemd unit: the default .service suffix is dropped.
		return scopedPath(target, "unit", strings.TrimSuffix(strings.ToLower(strings.TrimSpace(a.Params[opschema.ParamUnit])), ".service"))
	case opschema.FamilyContainerLifecycle:
		return scopedPath(target, "container", a.Params[opschema.ParamContainer])
	}
	if target == "" {
		return ResourceKeyNoTarget
	}
	return target
}

// Overlaps reports whether two resource paths name the same resource or one contains the other. The no-target
// bucket overlaps only itself.
func Overlaps(a, b string) bool {
	if a == ResourceKeyNoTarget || b == ResourceKeyNoTarget {
		return a == b
	}
	return a == b || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

// ResourceHost is a path's first segment: the host (or guest) every overlapping claim shares, so the durable
// admission can serialize all of them on one lock.
func ResourceHost(path string) string {
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return path
}

func scopedPath(target, kind, name string) string {
	if target == "" {
		return ResourceKeyNoTarget
	}
	if n := segment(name); n != "" {
		return target + "/" + kind + "/" + n
	}
	return target // the whole host is the resource — the widest, safest claim
}

// hostSegment normalizes a HOST (or guest) segment: a fully-qualified name is cut to its first label, so
// "web01.example.net" and "web01" are one host (a guest's name is its short hostname). An IP address is kept whole.
func hostSegment(s string) string {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	if net.ParseIP(h) == nil {
		if i := strings.IndexByte(h, '.'); i > 0 {
			h = h[:i]
		}
	}
	return segment(h)
}

// segment normalizes one path segment: trimmed, lower-cased, and with "/" escaped so a name cannot add a segment.
func segment(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "/", "%2f")
}
