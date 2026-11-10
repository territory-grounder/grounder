package actuate

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
)

// KILLING MUTATIONS: Admits always true - RED on "another guest" and on the drill matrix; Admits always false - RED on
// "the listed guest"; returning a non-nil scope for an
// empty list - RED on TestParseActuationScopeEmptyLeavesTheChainUnchanged; comparing Target instead of the resource host
// - RED on "the guest named by node plus param".
func TestActuationScopeAdmitsOnlyListedResources(t *testing.T) {
	s := ParseActuationScope("pool01, POOL02.example.net;pool03")
	for _, tc := range []struct {
		name string
		act  manifest.Action
		want bool
	}{
		{"the listed guest", manifest.Action{Target: "pool01", OpClass: "start-guest", Params: map[string]string{"guest": "pool01"}}, true},
		{"a listed guest in another spelling", manifest.Action{Target: "pool02", OpClass: "start-guest", Params: map[string]string{"guest": "pool02"}}, true},
		{"another guest", manifest.Action{Target: "prod01", OpClass: "start-guest", Params: map[string]string{"guest": "prod01"}}, false},
		{"the guest named by node plus param", manifest.Action{Target: "pve03", OpClass: "start-guest", Params: map[string]string{"guest": "pool01"}}, true},
		{"another guest behind a listed node name", manifest.Action{Target: "pool01", OpClass: "start-guest", Params: map[string]string{"guest": "prod01"}}, false},
		{"a service on a listed host", manifest.Action{Target: "pool03", OpClass: "restart-service", Params: map[string]string{"unit": "nginx"}}, true},
		{"a service on another host", manifest.Action{Target: "prod01", OpClass: "restart-service", Params: map[string]string{"unit": "nginx"}}, false},
		{"a gitops change naming a cluster object", manifest.Action{Target: "deployment/app", OpClass: "k8s-set-replicas"}, false},
		{"an action with no host", manifest.Action{OpClass: "start-guest"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.Admits(ResourceHost(ResourcePath(tc.act))); got != tc.want {
				t.Fatalf("Admits = %v, want %v (%s)", got, tc.want, ResourcePath(tc.act))
			}
		})
	}
}

func TestParseActuationScopeEmptyLeavesTheChainUnchanged(t *testing.T) {
	for _, raw := range []string{"", "  ", ",;,"} {
		if s := ParseActuationScope(raw); s != nil {
			t.Fatalf("ParseActuationScope(%q) = %v, want nil", raw, s.Hosts())
		}
	}
}

func TestInterceptorRunsAnEffectInsideTheScope(t *testing.T) {
	act := &fakeActuator{}
	r := goodRequest(t)
	i := wired(safety.NewActuatingChokepoint(), act).WithActuationScope(ParseActuationScope(ResourceHost(ResourcePath(r.Manifest.Action))))
	if _, err := i.Do(context.Background(), r); err != nil {
		t.Fatalf("an in-scope action was refused: %v", err)
	}
	if act.execs == 0 {
		t.Fatal("the effect leaf was not reached for an in-scope action")
	}
}
