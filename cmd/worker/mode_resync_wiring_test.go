package main

// THE MODE RESYNC MUST BE RUNNING IN EVERY WORKER PROCESS (TG-570, spec/015 REQ-1526).
//
// core/policy's oracles prove Resync carries a kill or a transition from one controller to another. None of that
// reaches production unless the composition root runs it: the defect was precisely that the actuation worker read
// the mode once at boot and never again, and an unwired Resync reproduces it exactly. The property is about the
// composition root (a ticker on the bound controller), so — like boot_posture_order_test.go — this guard reads
// main.go and ignores comment lines, so prose ABOUT the loop cannot satisfy it.

import (
	"os"
	"strings"
	"testing"
)

// KILLING MUTATION: delete the resync goroutine (or point its ticker at anything but policy.ModeResyncInterval).
// RED — the actuation plane is back to its boot-time copy of the mode.
func TestModeResyncLoopRunsOnTheBoundController(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read composition root: %v", err)
	}
	var code []string
	for _, line := range strings.Split(string(src), "\n") {
		if l := strings.TrimSpace(line); !strings.HasPrefix(l, "//") {
			code = append(code, l)
		}
	}
	text := strings.Join(code, "\n")

	bind := strings.Index(text, "chokepoint.BindMode(policyModeCtl)")
	if bind < 0 {
		t.Fatal("cmd/worker/main.go no longer binds policyModeCtl into the chokepoint — the boot sequence changed, " +
			"so re-derive this guard rather than deleting it")
	}
	after := text[bind:]
	ticker := strings.Index(after, "time.NewTicker(policy.ModeResyncInterval)")
	resync := strings.Index(after, "policyModeCtl.Resync(")
	if ticker < 0 || resync < 0 || resync < ticker {
		t.Fatal("no ticker on policy.ModeResyncInterval calling policyModeCtl.Resync after the chokepoint bind: " +
			"each worker process then serves its BOOT copy of the mode forever — a kill in the triage plane never " +
			"stops the actuation plane, and an operator's POST /v1/mode never reaches it (TG-570)")
	}
}

// The scaled burst bound reaches the worker only through reconBudgetFromEnv. KILLING MUTATION: drop the
// BurstPerSession field from it (the struct literal would then leave it 0, the flat bound). RED — the shipped
// worker keeps force-Shadowing on every mass outage although the governor supports scaling (TG-571).
func TestReconBudgetFromEnvArmsTheScaledBurst(t *testing.T) {
	if got := reconBudgetFromEnv().BurstPerSession; got != 12 {
		t.Fatalf("with TG_RECON_BURST_PER_SESSION unset the worker must arm 12/investigation, got %d", got)
	}
	t.Setenv("TG_RECON_BURST_PER_SESSION", "0")
	if got := reconBudgetFromEnv().BurstPerSession; got != 0 {
		t.Fatalf("an explicit 0 (the flat, stricter bound) must reach the budget as 0, got %d", got)
	}
	t.Setenv("TG_RECON_BURST_PER_SESSION", "20")
	if got := reconBudgetFromEnv().BurstPerSession; got != 20 {
		t.Fatalf("a positive value must pass through, got %d", got)
	}
	t.Setenv("TG_RECON_BURST_PER_SESSION", "lots")
	if got := reconBudgetFromEnv().BurstPerSession; got != 12 {
		t.Fatalf("a malformed value must fall back to the default 12, got %d", got)
	}
}
