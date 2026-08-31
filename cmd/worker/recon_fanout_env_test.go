package main

import "testing"

// TG-325 (review finding #1): the observe-only fan-out flag's documented "0 disables" off-switch must be
// REACHABLE through the one production wire an operator actually has — the TG_RECON_FANOUT_OBSERVE env var.
// envInt coerces any <=0 to the default (correct for the gating bounds, a guard must never be blanked off; wrong
// for this observe-only, legitimately-disable-able field), so FanoutObserve is wired through envIntAllowZero.
// This proves an explicit 0 survives the wire as 0 (off), a positive passes through, and unset/malformed arm the
// default. Killing mutation: wire FanoutObserve through envInt (or clamp <=0 here) → "0" returns 12 → RED.
func TestReconFanoutObserveZeroIsReachableThroughEnv(t *testing.T) {
	const def = 12
	// An UNSET key (not in bootConfig or the OS env) arms the default — the flag is on by default.
	if got := envIntAllowZero("TG_RECON_FANOUT_OBSERVE_UNSET_TESTKEY", def); got != def {
		t.Fatalf("an unset key must fall back to the default %d, got %d", def, got)
	}
	// An explicit 0 reaches the budget as 0 (off) — the reachability the fix restores.
	t.Setenv("TG_RECON_FANOUT_OBSERVE", "0")
	if got := envIntAllowZero("TG_RECON_FANOUT_OBSERVE", def); got != 0 {
		t.Fatalf("TG_RECON_FANOUT_OBSERVE=0 must reach the budget as 0 (off), got %d — the documented off-switch is severed", got)
	}
	// A positive value passes through unchanged.
	t.Setenv("TG_RECON_FANOUT_OBSERVE", "20")
	if got := envIntAllowZero("TG_RECON_FANOUT_OBSERVE", def); got != 20 {
		t.Fatalf("a positive value must pass through, got %d", got)
	}
	// A malformed value is a typo, not a decision: fall back to the armed default rather than silently disabling.
	t.Setenv("TG_RECON_FANOUT_OBSERVE", "not-a-number")
	if got := envIntAllowZero("TG_RECON_FANOUT_OBSERVE", def); got != def {
		t.Fatalf("a malformed value must fall back to the default %d, got %d", def, got)
	}
}
