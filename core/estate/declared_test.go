package estate

import "testing"

// TestParseRelType pins the declared relation vocabulary that both the declared-config parser and the eval
// snapshot loader bind to. The member_of / routes_via cases are the regression: the old eval/discovery.go
// relOf recognised only runs_on and silently coerced these DECLARED types into depends_on (TG-179a, TG-175).
func TestParseRelType(t *testing.T) {
	cases := []struct {
		in   string
		want RelType
		ok   bool
	}{
		{"runs_on", RelRunsOn, true},
		{"RUNS_ON", RelRunsOn, true},          // case-insensitive, matching the old EqualFold behaviour
		{"member_of", RelMemberOf, true},      // regression: was silently coerced to depends_on
		{"routes_via", RelRoutesVia, true},    // regression: was silently coerced to depends_on
		{"depends_on", RelDependsOn, true},
		{"", RelDependsOn, true},              // empty is the legitimate generic default
		{"   ", RelDependsOn, true},           // whitespace trims to empty
		{"peers_with", RelDependsOn, false},   // boundary violation: unknown rel, ok=false
		{"member of", RelDependsOn, false},    // near-miss typo is NOT silently accepted
	}
	for _, c := range cases {
		got, ok := ParseRelType(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseRelType(%q) = (%v, %v); want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestParseRelType_KnownSetComplete guards against a new RelType being added to the const block without
// being taught to the parser (and vice versa) — every knownRelTypes entry must round-trip through ParseRelType.
func TestParseRelType_KnownSetComplete(t *testing.T) {
	for k := range knownRelTypes {
		got, ok := ParseRelType(string(k))
		if !ok || got != k {
			t.Errorf("declared rel %q does not round-trip through ParseRelType: got (%v, %v)", k, got, ok)
		}
	}
}
