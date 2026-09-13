package policy

// TG-178: the graduation gate's boundary distance — how many verified-clean runs an op-class is from its
// next rung. -1 is the ticket's named boundary case ("one verified-clean outcome short of graduation"). The
// value feeds the observe-only gate-verdict margin the interceptor emits; it changes no verdict.

import (
	"context"
	"testing"
)

func TestGraduationMarginReportsRunsShortOfTheNextRung(t *testing.T) {
	ctx := context.Background()
	const op = "restart-service" // embedded + auto-eligible, so the graduation gate actually consults a rung

	// A class at approve, ONE clean run short of the promote bar → margin -1 (the boundary case).
	store := NewMemGraduationStore().Seed(ClassState{OpClass: op, Level: LevelApprove, CleanRunCount: DefaultPromoteThreshold - 1})
	if m, present := NewLadder(DefaultPromoteThreshold, store, nil).GraduationMargin(ctx, op); !present || m != -1 {
		t.Fatalf("one clean run short: margin=%d present=%v, want -1/true", m, present)
	}

	// Three short → margin -3 (still a signed distance, only -1 is the reviewable boundary).
	store2 := NewMemGraduationStore().Seed(ClassState{OpClass: op, Level: LevelApprove, CleanRunCount: DefaultPromoteThreshold - 3})
	if m, present := NewLadder(DefaultPromoteThreshold, store2, nil).GraduationMargin(ctx, op); !present || m != -3 {
		t.Fatalf("three clean runs short: margin=%d present=%v, want -3/true", m, present)
	}

	// A fully graduated class (LevelAuto) has NO next rung → present false, margin 0 (never a spurious 0
	// that the interceptor would emit as an at-threshold boundary).
	gradStore := NewMemGraduationStore().Seed(ClassState{OpClass: op, Level: LevelAuto})
	if m, present := NewLadder(DefaultPromoteThreshold, gradStore, nil).GraduationMargin(ctx, op); present || m != 0 {
		t.Fatalf("graduated class: margin=%d present=%v, want 0/false (no rung left)", m, present)
	}

	// A class the graduation gate never consults (unregistered / not auto-eligible) has no rung to measure.
	if m, present := NewLadder(DefaultPromoteThreshold, NewMemGraduationStore(), nil).GraduationMargin(ctx, "tg178-not-a-registered-class"); present || m != 0 {
		t.Fatalf("unregistered class: margin=%d present=%v, want 0/false", m, present)
	}
}
