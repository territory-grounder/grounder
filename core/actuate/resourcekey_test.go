package actuate

// TG-664: the resource path names the OBJECT an action mutates, not the spelling of its target, and two claims
// conflict by CONTAINMENT, not by string equality.

import (
	"testing"

	"github.com/territory-grounder/grounder/core/manifest"
)

// KILLING MUTATIONS: in ResourcePath's guest-lifecycle case, ignore params.guest — RED ("one guest, two spellings");
// make hostSegment keep the domain — RED ("an FQDN target is its short host").
func TestResourcePathNamesTheObjectNotTheSpelling(t *testing.T) {
	cases := []struct {
		name string
		a    manifest.Action
		want string
	}{
		{"one guest, node spelling", manifest.Action{OpClass: "start-guest", Op: "start", Target: "pvenode01", Params: map[string]string{"guest": "Guest01"}}, "guest01"},
		{"one guest, guest spelling", manifest.Action{OpClass: "start-guest", Op: "start", Target: "guest01"}, "guest01"},
		{"unit scoped to its host", manifest.Action{OpClass: "restart-service", Op: "restart", Target: "Web01", Params: map[string]string{"unit": "nginx"}}, "web01/unit/nginx"},
		{"same unit, other host", manifest.Action{OpClass: "restart-service", Op: "restart", Target: "web02", Params: map[string]string{"unit": "nginx"}}, "web02/unit/nginx"},
		{"unit action without a unit claims the host", manifest.Action{OpClass: "restart-service", Op: "restart", Target: "web01"}, "web01"},
		{"container scoped to its host", manifest.Action{OpClass: "restart-container", Op: "restart", Target: "app01", Params: map[string]string{"container": "mealie"}}, "app01/container/mealie"},
		{"start-container shares the path with restart-container", manifest.Action{OpClass: "start-container", Op: "start", Target: "app01", Params: map[string]string{"container": "mealie"}}, "app01/container/mealie"},
		{"a slash in a name cannot forge a segment", manifest.Action{OpClass: "restart-service", Op: "restart", Target: "web01", Params: map[string]string{"unit": "a/b"}}, "web01/unit/a%2fb"},
		{"unknown class keeps the whole target", manifest.Action{OpClass: "no-such-class", Op: "x", Target: "db01"}, "db01"},
		{"an FQDN target is its short host", manifest.Action{OpClass: "restart-service", Op: "restart", Target: "Web01.Example.NET.", Params: map[string]string{"unit": "nginx"}}, "web01/unit/nginx"},
		{"a unit's .service suffix is the same unit", manifest.Action{OpClass: "restart-service", Op: "restart", Target: "web01", Params: map[string]string{"unit": "nginx.service"}}, "web01/unit/nginx"},
		{"a guest named by its FQDN", manifest.Action{OpClass: "start-guest", Op: "start", Target: "pve03", Params: map[string]string{"guest": "web01.example.net"}}, "web01"},
		{"an IP target is kept whole", manifest.Action{OpClass: "restart-service", Op: "restart", Target: "192.168.1.10", Params: map[string]string{"unit": "nginx"}}, "192.168.1.10/unit/nginx"},
		{"nothing named is the shared bucket", manifest.Action{OpClass: "start-guest", Op: "start"}, ResourceKeyNoTarget},
		{"unknown class, nothing named", manifest.Action{OpClass: "no-such-class", Op: "x"}, ResourceKeyNoTarget},
	}
	for _, tc := range cases {
		if got := ResourcePath(tc.a); got != tc.want {
			t.Errorf("%s: ResourcePath = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// KILLING MUTATION: make Overlaps an equality test (a == b) — RED (a host claim no longer covers its units).
func TestResourceClaimsConflictByContainment(t *testing.T) {
	guestStart := ResourcePath(manifest.Action{OpClass: "start-guest", Op: "start", Target: "pve03", Params: map[string]string{"guest": "web01"}})
	unitHeal := ResourcePath(manifest.Action{OpClass: "restart-service", Op: "restart", Target: "web01", Params: map[string]string{"unit": "nginx"}})
	otherUnit := ResourcePath(manifest.Action{OpClass: "start-service", Op: "start", Target: "web01", Params: map[string]string{"unit": "apache2"}})
	wholeHost := ResourcePath(manifest.Action{OpClass: "restart-service", Op: "restart", Target: "web01"})
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"a guest claim covers a unit inside it", guestStart, unitHeal, true},
		{"and the reverse", unitHeal, guestStart, true},
		{"a whole-host claim covers a unit", wholeHost, unitHeal, true},
		{"the same unit", unitHeal, unitHeal, true},
		{"two units on one host do not conflict", unitHeal, otherUnit, false},
		{"a name prefix is not a path prefix", "web01", "web010/unit/nginx", false},
		{"different hosts", unitHeal, "web02/unit/nginx", false},
		{"the no-target bucket overlaps itself", ResourceKeyNoTarget, ResourceKeyNoTarget, true},
		{"and nothing else", ResourceKeyNoTarget, "web01", false},
	}
	for _, tc := range cases {
		if got := Overlaps(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: Overlaps(%q, %q) = %v, want %v", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
	if h := ResourceHost(unitHeal); h != "web01" {
		t.Fatalf("a unit path's host is its first segment, got %q", h)
	}
}
