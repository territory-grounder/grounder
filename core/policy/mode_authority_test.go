package policy

import (
	"context"
	"errors"
	"testing"
)

// The four REQ-1502 proof-points for the WIRED mode-transition RBAC, driven through the REAL
// ModeTransitionAuthority + ModeController + audit.Ledger (greenPreflight/redPreflight/newLedger are the
// same-package fakes from mode_test.go). These lock the LAST safety gate before the owner-present flip.

// --- the authority rule (allowlist ∪ admin-group), fail-closed ------------------------------------------

func TestModeAuthority_ExplicitAllowlist(t *testing.T) {
	a := NewModeTransitionAuthority([]string{"kyriakosp", "alice"})
	ctx := context.Background()
	// An operator on the allowlist is authorized (bare id AND the console principal form both normalize).
	if err := a.HasModeChangeAuthority(ctx, "kyriakosp"); err != nil {
		t.Fatalf("allowlisted operator denied: %v", err)
	}
	if err := a.HasModeChangeAuthority(ctx, "operator:Kyriakosp"); err != nil {
		t.Fatalf("allowlisted principal (prefixed, mixed-case) denied: %v", err)
	}
	// An operator NOT on a non-empty allowlist is denied — even WITH an admin-group signal (the allowlist is
	// the tighter gate; a non-listed admin cannot flip).
	if err := a.HasModeChangeAuthority(ctx, "operator:mallory"); err == nil {
		t.Fatal("non-allowlisted operator was authorized (must fail closed)")
	}
	if err := a.HasModeChangeAuthority(WithModeChangeAdmin(ctx), "operator:mallory"); err == nil {
		t.Fatal("a non-listed admin was authorized despite a narrowing allowlist (must fail closed)")
	}
	// An empty (unauthenticated) actor is denied with the typed sentinel.
	if err := a.HasModeChangeAuthority(ctx, ""); !errors.Is(err, ErrEmptyModeChangeActor) {
		t.Fatalf("empty actor err = %v, want ErrEmptyModeChangeActor", err)
	}
}

func TestModeAuthority_AdminGroupFallbackOnlyWhenNoAllowlist(t *testing.T) {
	ctx := context.Background()
	// No explicit allowlist: an authenticated admin-group operator (the trusted context signal) is admitted;
	// the same operator WITHOUT the admin signal is denied (fail closed — no allowlist, no admin ⇒ nobody).
	empty := NewModeTransitionAuthority(nil)
	if err := empty.HasModeChangeAuthority(WithModeChangeAdmin(ctx), "operator:kyriakosp"); err != nil {
		t.Fatalf("admin-group operator denied under an empty allowlist: %v", err)
	}
	if err := empty.HasModeChangeAuthority(ctx, "operator:kyriakosp"); err == nil {
		t.Fatal("a non-admin operator was authorized with no allowlist and no admin signal (must fail closed)")
	}
	if empty.AllowlistSize() != 0 {
		t.Fatalf("empty allowlist size = %d, want 0", empty.AllowlistSize())
	}
}

func TestParseOperatorAllowlist(t *testing.T) {
	got := ParseOperatorAllowlist(" kyriakosp , ,alice ,")
	if len(got) != 2 || got[0] != "kyriakosp" || got[1] != "alice" {
		t.Fatalf("ParseOperatorAllowlist trimmed/split wrong: %#v", got)
	}
	if ParseOperatorAllowlist("   ") != nil {
		t.Fatal("blank allowlist must parse to nil (no operators)")
	}
}

// --- the four end-to-end proof-points (authority + controller + audited both outcomes) ------------------

// authorized operator CAN transition Shadow->Full-auto with a GREEN preflight.
func TestAuditedTransition_AuthorizedGreenFlips(t *testing.T) {
	ctx := context.Background()
	led := newLedger()
	ctl := NewModeController(ctx, NewMemModeStore(), led,
		NewModeTransitionAuthority([]string{"kyriakosp"}), greenPreflight{}, nil)

	mode, err := AuditedModeTransition(ctx, ctl, ModeShadow, ModeFullAuto, "operator:kyriakosp", "owner canary GO")
	if err != nil {
		t.Fatalf("authorized Shadow->Full-auto with green preflight refused: %v", err)
	}
	if mode != ModeFullAuto || ctl.Current() != ModeFullAuto {
		t.Fatalf("mode after authorized flip = %v/%v, want Full-auto", mode, ctl.Current())
	}
	if led.Len() != 1 || led.Entries()[0].Decision != "mode-transition:Shadow->Full-auto" {
		t.Fatalf("ledger did not record exactly the transition: len=%d entries=%+v", led.Len(), led.Entries())
	}
	if err := led.Verify(); err != nil {
		t.Fatalf("ledger chain broken: %v", err)
	}
}

// an UNAUTHORIZED / unauthenticated operator is DENIED + audited; the mode is unchanged.
func TestAuditedTransition_UnauthorizedDeniedAndAudited(t *testing.T) {
	ctx := context.Background()
	led := newLedger()
	ctl := NewModeController(ctx, NewMemModeStore(), led,
		NewModeTransitionAuthority([]string{"kyriakosp"}), greenPreflight{}, nil)

	// mallory is authenticated but NOT flip-authorized (not on the allowlist, no admin signal).
	mode, err := AuditedModeTransition(ctx, ctl, ModeShadow, ModeFullAuto, "operator:mallory", "sneaky flip")
	if !errors.Is(err, ErrUnauthorizedModeChange) {
		t.Fatalf("unauthorized transition err = %v, want ErrUnauthorizedModeChange", err)
	}
	if mode != ModeShadow || ctl.Current() != ModeShadow {
		t.Fatalf("mode changed on an unauthorized transition: %v", ctl.Current())
	}
	// The DENIAL is audited: exactly one refused record, and it names the actor + the attempted transition.
	if led.Len() != 1 {
		t.Fatalf("denied transition wrote %d ledger records, want 1 refused record", led.Len())
	}
	e := led.Entries()[0]
	if e.Decision != "mode-transition-refused:Shadow->Full-auto" {
		t.Fatalf("refused record decision = %q, want the mode-transition-refused record", e.Decision)
	}
	if err := led.Verify(); err != nil {
		t.Fatalf("ledger chain broken after a refused transition: %v", err)
	}

	// An empty (unauthenticated) actor is likewise denied + audited.
	if _, err := AuditedModeTransition(ctx, ctl, ModeShadow, ModeSemiAuto, "", "no identity"); !errors.Is(err, ErrUnauthorizedModeChange) {
		t.Fatalf("unauthenticated transition err = %v, want ErrUnauthorizedModeChange", err)
	}
	if ctl.Current() != ModeShadow || led.Len() != 2 {
		t.Fatalf("unauthenticated transition changed state or was not audited: mode=%v ledger=%d", ctl.Current(), led.Len())
	}
}

// a transition with a NON-GREEN preflight is refused (even for an authorized operator) + audited; mode unchanged.
func TestAuditedTransition_RedPreflightRefused(t *testing.T) {
	ctx := context.Background()
	led := newLedger()
	ctl := NewModeController(ctx, NewMemModeStore(), led,
		NewModeTransitionAuthority([]string{"kyriakosp"}), redPreflight{}, nil)

	mode, err := AuditedModeTransition(ctx, ctl, ModeShadow, ModeFullAuto, "operator:kyriakosp", "premature flip")
	if !errors.Is(err, ErrPreflightNotGreen) {
		t.Fatalf("escalation with a red preflight err = %v, want ErrPreflightNotGreen", err)
	}
	if mode != ModeShadow || ctl.Current() != ModeShadow {
		t.Fatalf("mode escalated despite a red preflight: %v", ctl.Current())
	}
	if led.Len() != 1 || led.Entries()[0].Decision != "mode-transition-refused:Shadow->Full-auto" {
		t.Fatalf("red-preflight refusal not audited: len=%d entries=%+v", led.Len(), led.Entries())
	}
}

// the DEFAULT (absent-store) mode stays Shadow even with the real authority wired — wiring authz never
// auto-transitions anything.
func TestWiredController_DefaultStaysShadow(t *testing.T) {
	ctx := context.Background()
	ctl := NewModeController(ctx, NewMemModeStore(), newLedger(),
		NewModeTransitionAuthority([]string{"kyriakosp"}), greenPreflight{}, nil)
	if ctl.Current() != ModeShadow || ctl.ResolveMode(ctx) != ModeShadow {
		t.Fatalf("a freshly-wired controller did not default to Shadow: current=%v resolve=%v", ctl.Current(), ctl.ResolveMode(ctx))
	}
}
