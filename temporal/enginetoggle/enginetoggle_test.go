package enginetoggle

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/policy"
)

// The activity fails closed when the worker has no bound toggle (TG_POLICY_ENGINE_TOGGLE unset) — the grounder
// surface then reports "not armed" rather than silently succeeding.
func TestActivityFailsClosedWithoutToggle(t *testing.T) {
	a := &Activities{D: Deps{Toggle: nil}}
	_, err := a.ApplyEngineToggleActivity(context.Background(), Request{Enable: false, Actor: "op", Reason: "r", Acknowledged: true})
	if !errors.Is(err, ErrNoToggle) {
		t.Fatalf("a nil bound toggle must return ErrNoToggle, got %v", err)
	}
}

// A blank rationale is refused BEFORE any effect — the reason doubles as the audited acknowledgement text, so a
// blank one could never confirm.
func TestActivityRequiresRationale(t *testing.T) {
	// A non-nil toggle gets us past the ErrNoToggle guard to the rationale check.
	a := &Activities{D: Deps{Toggle: policy.NewEngineToggle(nil, nil)}}
	_, err := a.ApplyEngineToggleActivity(context.Background(), Request{Enable: false, Actor: "op", Reason: "   "})
	if !errors.Is(err, ErrRationaleRequired) {
		t.Fatalf("a blank reason must return ErrRationaleRequired, got %v", err)
	}
}

// With a valid request the activity routes THROUGH policy.EngineToggle.Override, not around it: a toggle with a
// nil AuthorityChecker fails closed (ErrUnauthorizedEngineToggle), which only Override can produce — proof the
// gated path is the one exercised, and the never-auto floor is never bypassed by this lane.
func TestActivityRoutesThroughOverride(t *testing.T) {
	a := &Activities{D: Deps{Toggle: policy.NewEngineToggle(nil, nil), ModeNow: func() policy.Mode { return policy.ModeShadow }}}
	_, err := a.ApplyEngineToggleActivity(context.Background(), Request{Enable: true, Actor: "op", Reason: "go", Acknowledged: true})
	if !errors.Is(err, policy.ErrUnauthorizedEngineToggle) {
		t.Fatalf("a nil-authz toggle must fail closed with ErrUnauthorizedEngineToggle, got %v", err)
	}
}
