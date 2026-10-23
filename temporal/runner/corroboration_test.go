package runner

import (
	"context"
	"errors"
	"testing"
)

// TestCorroborateCommonCause covers the LIVE-evidence gate that stops an isolated host-down from fanning a
// speculative common-cause sibling cascade (axis A2 blast-radius precision): keep the sibling expansion only
// when >=2 of the target's estate co-tenants ALSO hold an OPEN incident; otherwise the down is isolated and siblings
// are pure false positives. Every uncertainty path FAILS OPEN (keep) so corroboration can only ever SUPPRESS a
// speculative cascade on positive counter-evidence, never blank a prediction on a plumbing gap.
func TestCorroborateCommonCause(t *testing.T) {
	sibs := []string{"g1", "g2", "g3", "g4"}
	siblingsOf := func(string) []string { return sibs }
	// The dep's signature is the contract under test as much as the threshold: no `since time.Time` exists,
	// so a recency window is inexpressible — the reader answers "which siblings hold an OPEN incident".
	returns := func(active map[string]bool) func(context.Context, []string) (map[string]bool, error) {
		return func(context.Context, []string) (map[string]bool, error) { return active, nil }
	}

	cases := []struct {
		name string
		a    *Activities
		want bool
	}{
		{"nil deps ⇒ fail-open (keep)", &Activities{D: Deps{}}, true},
		{"no siblings ⇒ keep (nothing to gate)",
			&Activities{D: Deps{SiblingsOf: func(string) []string { return nil }, RecentAlertHosts: returns(map[string]bool{})}}, true},
		{"0 co-tenants alerting ⇒ isolated, suppress",
			&Activities{D: Deps{SiblingsOf: siblingsOf, RecentAlertHosts: returns(map[string]bool{})}}, false},
		{"1 co-tenant alerting ⇒ below threshold, suppress",
			&Activities{D: Deps{SiblingsOf: siblingsOf, RecentAlertHosts: returns(map[string]bool{"g1": true})}}, false},
		{"2 co-tenants alerting ⇒ corroborated, keep",
			&Activities{D: Deps{SiblingsOf: siblingsOf, RecentAlertHosts: returns(map[string]bool{"g1": true, "g2": true})}}, true},
		{"lookup error ⇒ fail-open (keep)",
			&Activities{D: Deps{SiblingsOf: siblingsOf, RecentAlertHosts: func(context.Context, []string) (map[string]bool, error) { return nil, errors.New("db down") }}}, true},
		{"alerts outside the sibling set do NOT corroborate",
			&Activities{D: Deps{SiblingsOf: siblingsOf, RecentAlertHosts: returns(map[string]bool{"unrelated1": true, "unrelated2": true})}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.a.corroborateCommonCause(context.Background(), "target01"); got != c.want {
				t.Fatalf("want %v, got %v", c.want, got)
			}
		})
	}
}
