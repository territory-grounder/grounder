package main

import "testing"

// exclusiveGroupsGetter serves one value for TG_PVE_EXCLUSIVE_GROUPS and the default for every other key.
func exclusiveGroupsGetter(v string) func(k, def string) string {
	return func(k, def string) string {
		if k == exclusiveGroupsEnv {
			return v
		}
		return def
	}
}

// TG-654: the declaration arms the policy, an absent one is today's behaviour, and a malformed one is logged
// and DISABLED, never a crash (a typo must not take the worker down).
func TestExclusiveGuestGroupsDeclaration(t *testing.T) {
	if g := exclusiveGuestGroups(exclusiveGroupsGetter(""), "test"); g != nil {
		t.Fatalf("unset ⇒ no groups, got %v", g)
	}
	if g := exclusiveGuestGroups(exclusiveGroupsGetter("runnerA>bigB;runnerA>bigC"), "test"); g != nil {
		t.Fatalf("a declaration that does not parse must disable the policy (nil), got %v", g)
	}
	g := exclusiveGuestGroups(exclusiveGroupsGetter("runnerA>bigB|bigC"), "test")
	if len(g) != 1 || g[0].Default != "runnerA" || len(g[0].Partners) != 2 {
		t.Fatalf("a valid declaration must arm one group runnerA>bigB|bigC, got %v", g)
	}
}

// The detector receives the policy as ONE extra option when declared, and none otherwise (byte-identical).
func TestPVELivenessOptionsCarryTheExclusiveGroups(t *testing.T) {
	if n := len(pveLivenessOptions(exclusiveGroupsGetter("runnerA>bigB"), nil, 3)); n != 2 {
		t.Fatalf("a declared group must add exactly one option beyond the transport, got %d", n)
	}
	if n := len(pveLivenessOptions(exclusiveGroupsGetter("not a group"), nil, 3)); n != 1 {
		t.Fatalf("a malformed declaration must add nothing, got %d", n)
	}
}
