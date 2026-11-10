package guestgroup

import (
	"reflect"
	"testing"
)

func TestParseExclusiveGroupDeclarations(t *testing.T) {
	got, err := Parse(" runnerA > bigB | bigC ; dflt2>big3 ")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []Group{{Default: "runnerA", Partners: []string{"bigB", "bigC"}}, {Default: "dflt2", Partners: []string{"big3"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse = %#v, want %#v", got, want)
	}
	if got[0].String() != "runnerA>bigB|bigC" {
		t.Errorf("String = %q", got[0].String())
	}
	for _, blank := range []string{"", "  ", ";", " ; "} {
		if g, err := Parse(blank); err != nil || g != nil {
			t.Errorf("Parse(%q) = %v, %v; want no groups and no error", blank, g, err)
		}
	}
}

func TestParseRejectsAmbiguousDeclarations(t *testing.T) {
	for _, bad := range []string{
		"runnerA",             // no '>'
		">bigB",               // no default
		"runnerA>",            // no partner
		"runnerA>|",           // only empty partners
		"runnerA>runnerA",     // default as its own partner
		"runnerA>bigB|bigB",   // partner twice
		"runnerA>bigB;x>bigB", // one guest in two groups
		"runnerA>bigB;runnerA>bigC",
	} {
		if g, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) = %v, want an error (a half-applied declaration is worse than none)", bad, g)
		}
	}
}

func TestLookupGuestRole(t *testing.T) {
	groups := []Group{{Default: "runnerA", Partners: []string{"bigB", "bigC"}}}
	if g, r := Lookup(groups, "runnerA"); r != RoleDefault || g.Default != "runnerA" {
		t.Errorf("default: role %v group %v", r, g)
	}
	if g, r := Lookup(groups, " bigC "); r != RolePartner || g.Default != "runnerA" {
		t.Errorf("partner: role %v group %v", r, g)
	}
	if _, r := Lookup(groups, "other"); r != RoleNone {
		t.Errorf("other: role %v, want RoleNone", r)
	}
	if _, r := Lookup(nil, "runnerA"); r != RoleNone {
		t.Errorf("no groups: role %v, want RoleNone", r)
	}
}
