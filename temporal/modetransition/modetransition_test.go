package modetransition

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/policy"
)

// local PreflightChecker fakes (the policy-package fakes are unexported).
type greenPreflight struct{}

func (greenPreflight) PreflightGreen(context.Context) error { return nil }

type redPreflight struct{}

func (redPreflight) PreflightGreen(context.Context) error { return errors.New("preflight not green") }

func newCtl(t *testing.T, led *audit.Ledger, authz policy.AuthorityChecker, pf policy.PreflightChecker) *policy.ModeController {
	t.Helper()
	return policy.NewModeController(context.Background(), policy.NewMemModeStore(), led, authz, pf, nil)
}

// An authorized admin operator flips Shadow->Full-auto with a green preflight, executed ON the worker's
// bound controller and audited (exactly the owner-invoked flip path).
func TestActivity_AuthorizedGreenFlips(t *testing.T) {
	ctx := context.Background()
	led := audit.NewLedger()
	ctl := newCtl(t, led, policy.NewModeTransitionAuthority([]string{"kyriakosp"}), greenPreflight{})
	a := &Activities{D: Deps{Controller: ctl}}

	res, err := a.ApplyModeTransitionActivity(ctx, Request{
		To: "Full-auto", ExpectedFrom: "Shadow", Actor: "operator:kyriakosp", Reason: "owner canary GO", AdminAuthorized: true,
	})
	if err != nil {
		t.Fatalf("authorized flip refused: %v", err)
	}
	if res.Mode != "Full-auto" || res.From != "Shadow" || res.To != "Full-auto" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if ctl.Current() != policy.ModeFullAuto {
		t.Fatalf("controller not flipped: %v", ctl.Current())
	}
	if led.Len() != 1 || led.Entries()[0].Decision != "mode-transition:Shadow->Full-auto" {
		t.Fatalf("transition not audited exactly once: len=%d entries=%+v", led.Len(), led.Entries())
	}
}

// The admin-group fallback: with NO explicit allowlist, an admin-session operator is authorized.
func TestActivity_AdminGroupFallback(t *testing.T) {
	ctx := context.Background()
	ctl := newCtl(t, audit.NewLedger(), policy.NewModeTransitionAuthority(nil), greenPreflight{})
	a := &Activities{D: Deps{Controller: ctl}}
	if _, err := a.ApplyModeTransitionActivity(ctx, Request{To: "Semi-auto", Actor: "operator:kyriakosp", Reason: "flip", AdminAuthorized: true}); err != nil {
		t.Fatalf("admin-group operator denied under an empty allowlist: %v", err)
	}
	if ctl.Current() != policy.ModeSemiAuto {
		t.Fatalf("mode not flipped by admin-group operator: %v", ctl.Current())
	}
}

// An unauthorized operator is denied + audited; the mode stays Shadow.
func TestActivity_UnauthorizedDeniedAndAudited(t *testing.T) {
	ctx := context.Background()
	led := audit.NewLedger()
	ctl := newCtl(t, led, policy.NewModeTransitionAuthority([]string{"kyriakosp"}), greenPreflight{})
	a := &Activities{D: Deps{Controller: ctl}}

	res, err := a.ApplyModeTransitionActivity(ctx, Request{To: "Full-auto", Actor: "operator:mallory", Reason: "sneaky", AdminAuthorized: true})
	if !errors.Is(err, policy.ErrUnauthorizedModeChange) {
		t.Fatalf("unauthorized err = %v, want ErrUnauthorizedModeChange", err)
	}
	if res.Mode != "Shadow" || ctl.Current() != policy.ModeShadow {
		t.Fatalf("mode changed on an unauthorized transition: %v", ctl.Current())
	}
	if led.Len() != 1 || led.Entries()[0].Decision != "mode-transition-refused:Shadow->Full-auto" {
		t.Fatalf("denied transition not audited: len=%d entries=%+v", led.Len(), led.Entries())
	}
}

// A red preflight refuses an escalation even for an authorized operator; refusal is audited.
func TestActivity_RedPreflightRefused(t *testing.T) {
	ctx := context.Background()
	led := audit.NewLedger()
	ctl := newCtl(t, led, policy.NewModeTransitionAuthority([]string{"kyriakosp"}), redPreflight{})
	a := &Activities{D: Deps{Controller: ctl}}

	if _, err := a.ApplyModeTransitionActivity(ctx, Request{To: "Full-auto", Actor: "operator:kyriakosp", Reason: "early", AdminAuthorized: true}); !errors.Is(err, policy.ErrPreflightNotGreen) {
		t.Fatalf("red-preflight err = %v, want ErrPreflightNotGreen", err)
	}
	if ctl.Current() != policy.ModeShadow {
		t.Fatalf("mode escalated despite a red preflight: %v", ctl.Current())
	}
	if led.Len() != 1 || led.Entries()[0].Decision != "mode-transition-refused:Shadow->Full-auto" {
		t.Fatalf("red-preflight refusal not audited: %+v", led.Entries())
	}
}

// A stale ExpectedFrom (the console's view lagged) is refused (audited) by the controller's compare-and-swap.
func TestActivity_StaleExpectedFromRefused(t *testing.T) {
	ctx := context.Background()
	led := audit.NewLedger()
	ctl := newCtl(t, led, policy.NewModeTransitionAuthority([]string{"kyriakosp"}), greenPreflight{})
	a := &Activities{D: Deps{Controller: ctl}}
	// active is Shadow, but the operator expects Semi-auto.
	if _, err := a.ApplyModeTransitionActivity(ctx, Request{To: "Full-auto", ExpectedFrom: "Semi-auto", Actor: "operator:kyriakosp", Reason: "stale view", AdminAuthorized: true}); !errors.Is(err, policy.ErrStaleMode) {
		t.Fatalf("stale expected_from err = %v, want ErrStaleMode", err)
	}
	if ctl.Current() != policy.ModeShadow {
		t.Fatalf("mode changed on a stale-CAS refusal: %v", ctl.Current())
	}
}

// Malformed requests fail closed with typed errors (rationale, unknown target, nil controller).
func TestActivity_MalformedFailClosed(t *testing.T) {
	ctx := context.Background()
	ctl := newCtl(t, audit.NewLedger(), policy.NewModeTransitionAuthority([]string{"kyriakosp"}), greenPreflight{})
	a := &Activities{D: Deps{Controller: ctl}}

	if _, err := a.ApplyModeTransitionActivity(ctx, Request{To: "Full-auto", Actor: "operator:kyriakosp", Reason: "  "}); !errors.Is(err, ErrRationaleRequired) {
		t.Fatalf("empty rationale err = %v, want ErrRationaleRequired", err)
	}
	if _, err := a.ApplyModeTransitionActivity(ctx, Request{To: "Turbo", Actor: "operator:kyriakosp", Reason: "bad mode"}); !errors.Is(err, policy.ErrUnknownMode) {
		t.Fatalf("unknown target err = %v, want ErrUnknownMode", err)
	}
	nilAct := &Activities{}
	if _, err := nilAct.ApplyModeTransitionActivity(ctx, Request{To: "Full-auto", Actor: "operator:kyriakosp", Reason: "no ctl"}); !errors.Is(err, ErrNoController) {
		t.Fatalf("nil controller err = %v, want ErrNoController", err)
	}
}
