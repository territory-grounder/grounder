package main

import (
	"os"
	"strings"
	"testing"
)

// TestApprovalResolverArmsOnlyOnTheWordAutonomous pins the switch's fail direction (TG-559): exactly
// "autonomous" (any case, surrounding space ignored) removes the human vote; everything else — unset, "human",
// and every plausible typo or truthy spelling — keeps it. A resolver that armed on "true"/"1"/"auto" would let a
// guess at the key's grammar silently take the human out of the loop.
func TestApprovalResolverArmsOnlyOnTheWordAutonomous(t *testing.T) {
	cases := []struct {
		value string
		armed bool
	}{
		{"", false}, {"human", false}, {"HUMAN", false},
		{"autonomous", true}, {"Autonomous", true}, {"  autonomous  ", true},
		{"auto", false}, {"true", false}, {"1", false}, {"yes", false}, {"autonomus", false},
	}
	for _, c := range cases {
		value := c.value
		get := func(k, def string) string {
			if k == "TG_APPROVAL_RESOLVER" {
				return value
			}
			return def
		}
		if got := approvalResolverFromEnv(get); got != c.armed {
			t.Errorf("TG_APPROVAL_RESOLVER=%q armed=%v, want %v", c.value, got, c.armed)
		}
	}
}

// TestApprovalResolverReachesTheRunnerDeps is the reachability control (the "present, tested, unreachable"
// class this repo keeps shipping): the switch is worthless unless the composition root hands its answer to the
// runner's Deps. It fails if the field assignment is deleted or stops calling the env reader.
func TestApprovalResolverReachesTheRunnerDeps(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "AutonomousResolver:") && strings.Contains(line, "approvalResolverFromEnv(getenv)") {
			return
		}
	}
	t.Fatal("main.go never assigns runner.Deps.AutonomousResolver from approvalResolverFromEnv(getenv) — " +
		"TG_APPROVAL_RESOLVER would be read by nothing that decides a poll")
}
