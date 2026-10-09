package credential

import (
	"sort"
	"testing"
)

func ogSameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

func ogContains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// TestObjectGroupStore_GroupsFor pins the host-glob matching: a host is a member of every object group with a
// pattern it matches (leading/trailing '*' via GlobMatch), across multiple groups and multiple patterns, and
// an unmatched or empty host is a member of nothing (fail-closed additive).
func TestObjectGroupStore_GroupsFor(t *testing.T) {
	o := newObjectGroupStore()
	o.set([]ObjectGroup{
		{Name: "edge-firewalls", Patterns: []string{"dc1demo-fw*", "dc2demo-fw*"}},
		{Name: "nl-hosts", Patterns: []string{"dc1demo-*"}},
		{Name: "exact-web", Patterns: []string{"dc1demo-web01"}},
	})
	cases := []struct {
		host string
		want []string
	}{
		{"dc1demo-fw01", []string{"edge-firewalls", "nl-hosts"}}, // fw glob + nl glob
		{"dc1demo-web01", []string{"nl-hosts", "exact-web"}},     // nl glob + exact literal
		{"dc2demo-fw01", []string{"edge-firewalls"}},             // only the gr fw glob
		{"unrelated-host", nil},                                      // matches nothing
		{"", nil},                                                    // empty host is inert
	}
	for _, c := range cases {
		if got := o.groupsFor(c.host); !ogSameSet(got, c.want) {
			t.Errorf("groupsFor(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// TestSyncEngine_ObjectGroups_GroupsForUnionAndDisarm checks the public seam Resolve shares (GroupsFor): a
// hand-authored object group ADDS the host to that group; a non-member never gets it; and SetObjectGroups(nil)
// disarms the seam so the host resolves exactly as before. This is the same union Resolve applies, so it
// guards the consistency between GroupsFor and Resolve.
func TestSyncEngine_ObjectGroups_GroupsForUnionAndDisarm(t *testing.T) {
	se := NewSyncEngine(nil)

	// disarmed by default (no groups set) — nothing extra.
	if got := se.GroupsFor("dc1demo-web01"); ogContains(got, "webservers") {
		t.Fatalf("with no object groups, GroupsFor must not invent 'webservers': %v", got)
	}

	se.SetObjectGroups([]ObjectGroup{{Name: "webservers", Patterns: []string{"dc1demo-web*"}}})
	if got := se.GroupsFor("dc1demo-web01"); !ogContains(got, "webservers") {
		t.Errorf("a member host should carry the object group 'webservers', got %v", got)
	}
	if got := se.GroupsFor("dc1demo-db01"); ogContains(got, "webservers") {
		t.Errorf("a NON-member host must not carry 'webservers', got %v", got)
	}

	// SetObjectGroups(nil) disarms — the seam adds nothing again.
	se.SetObjectGroups(nil)
	if got := se.GroupsFor("dc1demo-web01"); ogContains(got, "webservers") {
		t.Errorf("after SetObjectGroups(nil) the seam must be inert, got %v", got)
	}
}
