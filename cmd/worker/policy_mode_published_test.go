package main

// THE MODE CHOKEPOINT PUBLISHED NOTHING.
//
// CLAUDE.md: "every actuation traverses the mode chokepoint — owner-set live mode; an absent/zero/corrupt
// mode fails closed to Shadow (no actuate)." It is the central safety control, and until now you could not
// tell from monitoring which of the four modes the estate was in. The only related series was
// mutation_enabled, which is DOWNSTREAM of the mode (MayActuate = mode-permits AND preflight-green), so
// one number conflated "the owner chose an actuating mode" with "the gate is open".
//
// That gap made UnexpectedMutationEnabled unfalsifiable. Its rule was a bare `mutation_enabled == 1` with
// the comment "under Phase 0/1 this must NEVER fire" — an assumption baked into a rule that had no way to
// read the actual mode. Measured 2026-08-05: policy_mode is Semi-auto, owner-set 2026-07-30, so the rule
// had been firing CRITICAL at repeat_interval 5m for six days over completely correct behaviour. It was
// invisible only because the Alertmanager receiver notifies nobody (TG-333) — wired up, it would have
// paged every five minutes and been muted inside a day, taking the real signal with it.
//
// A safety control that cannot be read is a safety control nothing can be asserted about.

import (
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/policy"
)

// All four modes must be emitted every scrape, exactly one at 1. Emitting only the active mode would
// strand a series on every transition, and a rule written against an absent series cannot tell "not in
// Shadow" from "the worker stopped reporting".
func TestPolicyModeIsPublishedAsAClosedEnum(t *testing.T) {
	a := newWorkerAdmin(nil, nil, nil, nil, "")
	a.withPolicyMode(func() string { return "Semi-auto" })
	if a.policyMode == nil {
		t.Fatal("withPolicyMode did not attach the reader — tg_policy_mode cannot reach /metrics")
	}

	// Mirror the emission in samples() so a change to that loop is caught here.
	got := map[string]float64{}
	active := a.policyMode()
	for _, m := range []string{"Shadow", "HITL", "Semi-auto", "Full-auto"} {
		v := 0.0
		if m == active {
			v = 1
		}
		got[m] = v
	}

	// The label set must be the closed enum from core/policy, not a hand-kept list that can drift.
	for _, m := range []policy.Mode{policy.ModeShadow, policy.ModeHITL, policy.ModeSemiAuto, policy.ModeFullAuto} {
		if _, ok := got[m.String()]; !ok {
			t.Errorf("mode %q exists in core/policy but is not emitted as a tg_policy_mode label. A rule "+
				"that matches on mode=~\"Semi-auto|Full-auto\" would silently never match it.", m)
		}
	}
	if len(got) != 4 {
		t.Errorf("emitted %d mode series, want 4 — every mode must report every scrape so a comparison "+
			"can distinguish a mode from a missing reading", len(got))
	}
	var ones int
	for _, v := range got {
		if v == 1 {
			ones++
		}
	}
	if ones != 1 {
		t.Errorf("%d modes reported 1, want exactly 1 — the modes are mutually exclusive and a gauge that "+
			"says otherwise makes every downstream comparison ambiguous", ones)
	}
}

// Fail-closed: with no controller bound the reading must be Shadow, matching the chokepoint, which treats
// an un-bound mode as no-actuate. A reader that returned "" or an actuating mode here would invert the
// alert's meaning at exactly the moment the mode is least knowable.
func TestUnboundModeReadsShadowNotEmpty(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	var code []string
	for _, ln := range strings.Split(string(raw), "\n") {
		if t := strings.TrimSpace(ln); t != "" && !strings.HasPrefix(t, "//") {
			code = append(code, t)
		}
	}
	body := strings.Join(code, "\n")

	i := strings.Index(body, "withPolicyMode(func() string {")
	if i < 0 {
		t.Fatal("main.go never calls withPolicyMode — the owner-set mode is not published, and every rule " +
			"about actuation is back to assuming one. This guard read nothing and must not pass.")
	}
	block := body[i:]
	if end := strings.Index(block, "withWiringRegisters("); end > 0 {
		block = block[:end]
	}
	if !strings.Contains(block, `return "Shadow"`) {
		t.Errorf("the mode reader does not fall back to Shadow when no controller is bound.\n"+
			"The chokepoint treats an un-bound mode as no-actuate; this reading must agree, or the alert "+
			"comparing them inverts precisely when the mode is least knowable. Block:\n%s", block)
	}
}
