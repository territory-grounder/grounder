package actuate

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
)

// groupScopedDecider models the policy engine consuming object-groups (TG-481, spec/016 REQ-1618): it captures
// the Groups it was handed and returns `deny` iff the target is in a named object-group — exactly what a
// group-scoped policy rule does. It lets a test prove the interceptor feeds the resolved object-group
// membership into the policy decision.
type groupScopedDecider struct {
	denyGroup string
	gotGroups []string
	gotHost   string
	calls     int
}

func (d *groupScopedDecider) Decide(_ context.Context, in policy.EvalInput) (policy.PolicyDecision, error) {
	d.calls++
	d.gotGroups = in.Groups
	d.gotHost = in.Host
	v := policy.VerdictAuto
	for _, g := range in.Groups {
		if g == d.denyGroup {
			v = policy.VerdictDeny
		}
	}
	return policy.NewPolicyDecision(v, "group-rule", in.Band, nil, in.Mode, "group-scoped", policy.DecisionAudit{}), nil
}

// TG-481 (REQ-1618): the actuation interceptor feeds the SAME object-group membership the credential resolver
// reads (WithObjectGroupResolver, wired from credEngine.GroupsFor in the worker) into the policy EvalInput, so
// a group-scoped policy rule governs live actuation — one definition, no second. The discriminator is the
// shared resolver: with it wired a group-scoped `deny` bites and refuses the otherwise-executable action;
// unwired, the policy engine sees no groups and the SAME request executes exactly as before TG-481
// (byte-identical feed). Only the resolver wiring differs between the two arms.
//
// KILLING MUTATION: drop `Groups: i.resolveObjectGroups(...)` from the interceptor's EvalInput → ARM A's
// decider sees no groups, the deny never fires, and the action executes → the "wired resolver refuses" arm
// fails. That is the whole point: the policy engine only shares the object-group when the interceptor feeds it.
func TestObjectGroupMembershipFeedsThePolicyDecision(t *testing.T) {
	// ARM A: resolver wired, the target resolves into the "restricted" object-group ⇒ the group-scoped deny bites.
	t.Run("wired resolver: a group-scoped deny bites", func(t *testing.T) {
		act := &fakeActuator{}
		dec := &groupScopedDecider{denyGroup: "restricted"}
		i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
			WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto }).
			WithObjectGroupResolver(func(host string) []string {
				if host == "" {
					t.Fatal("the interceptor must pass the target host to the resolver, got empty")
				}
				return []string{"restricted"}
			})
		out, err := i.Do(context.Background(), goodRequest(t))
		if err != nil {
			t.Fatal(err)
		}
		if dec.gotHost != "web01" {
			t.Fatalf("the resolver/decision must see the target host web01, got %q", dec.gotHost)
		}
		if len(dec.gotGroups) != 1 || dec.gotGroups[0] != "restricted" {
			t.Fatalf("the policy decision must receive the resolved object-group membership, got %v", dec.gotGroups)
		}
		if out.Executed || act.execs != 0 || !contains3(out.Reason, "policy verdict deny") {
			t.Fatalf("a group-scoped deny fed from the shared object-group store must refuse: %+v execs=%d", out, act.execs)
		}
	})

	// ARM B (the byte-identical control): NO resolver wired ⇒ the policy engine sees no groups, the group-scoped
	// deny cannot match, and the SAME request executes exactly as before TG-481.
	t.Run("no resolver: no groups fed, the action executes as before", func(t *testing.T) {
		act := &fakeActuator{}
		dec := &groupScopedDecider{denyGroup: "restricted"}
		i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
			WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto })
		// deliberately no WithObjectGroupResolver
		out, err := i.Do(context.Background(), goodRequest(t))
		if err != nil {
			t.Fatal(err)
		}
		if len(dec.gotGroups) != 0 {
			t.Fatalf("with no resolver wired the policy decision must see NO object groups (byte-identical), got %v", dec.gotGroups)
		}
		if !out.Executed || act.execs != 1 {
			t.Fatalf("with no group fed, the group-scoped deny cannot match and the action executes: %+v execs=%d", out, act.execs)
		}
	})
}
