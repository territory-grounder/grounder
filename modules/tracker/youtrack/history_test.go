package youtrack

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// TestQuerySafeCannotRestructureAQuery pins the sanitizer on the one seam where alert-payload text
// reaches a query LANGUAGE. Host and rule arrive from an alert; YouTrack's grammar has its own operators,
// so an unsanitized value could change WHAT is searched rather than merely widen it.
//
// The fail direction is deliberate: a rejected character becomes a SPACE, which at worst broadens a
// search and can never restructure one.
//
// It moved here from cmd/worker with the query-building it protects. Sanitization belongs beside the
// grammar it defends — a single shared sanitizer in the composition root would have to be the
// intersection of every backend's grammar, and would silently stop matching the day a backend with
// different operators was added.
func TestQuerySafeCannotRestructureAQuery(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"dc1mealie01", "dc1mealie01"},
		{"Devices up/down", "Devices up/down"},
		{"host-01_a.b", "host-01_a.b"},
		// Operators and punctuation that carry meaning in the query grammar must not survive.
		{"web01 #Resolved", "web01 Resolved"},
		{"web01 project: SECRET", "web01 project SECRET"},
		{"web01 {braces} (parens)", "web01 braces parens"},
		{`web01 "quoted"`, "web01 quoted"},
		{"web01\nproject: X", "web01 project X"},
		{"  spaced   out  ", "spaced out"},
		{"", ""},
	} {
		if got := querySafe(tc.in); got != tc.want {
			t.Errorf("querySafe(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"a:b", "a#b", "a{b}", `a"b`, "a(b)"} {
		if got := querySafe(bad); strings.ContainsAny(got, `:#{}"()`) {
			t.Errorf("sanitized %q still carries an operator: %q", bad, got)
		}
	}
}

// A blank host would search the ENTIRE tracker and return unrelated incidents as this host's history —
// which reads as evidence, and is worse than returning nothing.
func TestSearchIncidentsRefusesABlankHost(t *testing.T) {
	m := New("https://yt.example/", config.SecretRef("env:TG_TEST_YT_TOKEN"))
	if _, err := m.SearchIncidents(context.Background(), " :#{} ", "", 5); err == nil {
		t.Fatal("a host that sanitizes to nothing was accepted; the search would match the whole tracker")
	}
}
