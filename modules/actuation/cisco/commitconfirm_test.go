package cisco

import (
	"context"
	"testing"
	"time"
)

// The two-phase commit-confirmed flow end-to-end against the fake: apply under a revert timer (session 1),
// then commit with `configure confirm` (session 2, sharing the device's armed-revert state). The confirm
// succeeds precisely BECAUSE the revert was armed — the fake refuses a confirm with nothing pending.
func TestCommitConfirmedApplyThenConfirm(t *testing.T) {
	cr, gotLines := ciscoTestConfigRunner(t, "", false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := cr.RunConfigWithRevert(ctx, []string{"interface Gi0/1", "description tg-managed"}, 5); err != nil {
		t.Fatalf("apply-with-revert: %v", err)
	}
	got := drainLines(gotLines)
	if len(got) != 2 || got[0] != "interface Gi0/1" || got[1] != "description tg-managed" {
		t.Fatalf("device applied the wrong lines: %v", got)
	}
	// The change is live under the armed timer but NOT yet committed — confirm it in a fresh session.
	if _, err := cr.ConfirmConfig(ctx); err != nil {
		t.Fatalf("confirm of an armed revert must succeed: %v", err)
	}
	// A SECOND confirm must fail closed — the first one cleared the pending revert.
	if _, err := cr.ConfirmConfig(ctx); err == nil {
		t.Fatal("a second confirm (nothing pending) must fail closed")
	}
}

// RunConfigWithRevert refuses a non-positive timer BEFORE any dial — a commit-confirmed apply with no armed
// dead-man's-switch is just a persistent write, which this method must not do.
func TestRunConfigWithRevertRefusesNonPositiveTimer(t *testing.T) {
	cr, gotLines := ciscoTestConfigRunner(t, "", false)
	for _, mins := range []int{0, -1} {
		if _, err := cr.RunConfigWithRevert(context.Background(), []string{"interface Gi0/1"}, mins); err == nil {
			t.Fatalf("timer=%d must be refused", mins)
		}
	}
	if got := drainLines(gotLines); len(got) != 0 {
		t.Fatalf("a refused revert apply must not reach the device: %v", got)
	}
}

// ConfirmConfig fails closed when no revert is pending — the caller must read that as "the change is NOT
// committed", never as success.
func TestConfirmConfigFailsClosedWhenNothingPending(t *testing.T) {
	cr, _ := ciscoTestConfigRunner(t, "", false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cr.ConfirmConfig(ctx); err == nil {
		t.Fatal("confirm with no pending revert must fail closed")
	}
}

// The revert-timer apply still fails closed on a rejected line — it shares the slice-2 apply path — and must NOT
// leave a half-applied change armed silently: it returns the error, and the caller does not confirm.
func TestRunConfigWithRevertFailsClosedOnDeviceReject(t *testing.T) {
	cr, gotLines := ciscoTestConfigRunner(t, "bogus-command", false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cr.RunConfigWithRevert(ctx, []string{"interface Gi0/1", "bogus-command", "description never"}, 5); err == nil {
		t.Fatal("a rejected line must fail the revert apply closed")
	}
	got := drainLines(gotLines)
	for _, l := range got {
		if l == "description never" {
			t.Fatalf("a line after the rejected one reached the device: %v", got)
		}
	}
}
