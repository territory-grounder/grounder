package skills

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/execclass"
)

func hasAll(body string, subs ...string) bool {
	for _, s := range subs {
		if !strings.Contains(body, s) {
			return false
		}
	}
	return true
}

// A DEEP/STANDARD investigation gets the full protocols; the selection is deterministic.
func TestComposeDeepGetsFullProtocols(t *testing.T) {
	body, loaded := Default().Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation})
	if !hasAll(body, "Proving your work", "Investigation protocol", "Shortcuts to resist", "Conservative-remediation catalog", "Stop if your investigation stalls") {
		t.Fatalf("deep investigation must load the full behavioral set; loaded=%v", loaded)
	}
	// Composition is stable across calls (pure function).
	body2, _ := Default().Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation})
	if body != body2 {
		t.Fatal("Compose must be deterministic")
	}
}

// A FAST_AGENT gets the compact always-on skills but NOT the heavyweight protocols — the phase-aware
// prompt-compiler behavior (keep irrelevant instruction out of a cheap incident's prompt).
func TestComposeFastIsCompact(t *testing.T) {
	body, loaded := Default().Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.FastAgent})
	if !hasAll(body, "Proving your work", "Stop if your investigation stalls") {
		t.Fatalf("fast agent must still get the always-on essentials; loaded=%v", loaded)
	}
	if strings.Contains(body, "Investigation protocol") || strings.Contains(body, "Shortcuts to resist") {
		t.Fatalf("fast agent must NOT carry the heavyweight protocols; loaded=%v", loaded)
	}
}

// The conservative-remediation catalog (what to propose) and its hard floor are always present — the agent
// must always know which actions are never a reversible auto-fix.
func TestConservativeCatalogAlwaysPresentWithFloor(t *testing.T) {
	for _, c := range []execclass.Class{execclass.FastAgent, execclass.StandardAgent, execclass.DeepInvestigation} {
		body, _ := Default().Compose(Context{Phase: PhaseInvestigate, ExecClass: c})
		if !strings.Contains(body, "Conservative-remediation catalog") {
			t.Errorf("%s must carry the conservative catalog", c)
		}
		// the hard floor must name the stateful + irreversible classes
		if !hasAll(body, "etcd", "host reboot", "dropdb", "terraform destroy") {
			t.Errorf("%s conservative floor must name the never-auto classes", c)
		}
	}
}

// The re-authored behavioral skills (crit-3 / TG-69) must speak TG's JSON-directive, read-only wire format:
// the conservative catalog teaches a PREDICTION (feeding falsifiable_prediction), evidence is cited by
// OBSERVATION id, and NO skill carries the predecessor's chatops/execution idiom (fenced blocks, run-the-command,
// regression tests) that loop.go documents produced 0% proposals in an eval.
func TestSkillsSpeakWireFormatNotChatopsIdiom(t *testing.T) {
	body, _ := Default().Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation})
	if !strings.Contains(body, "predict:") || !strings.Contains(body, "PREDICTION") {
		t.Fatal("conservative-remediation must teach a falsifiable prediction (predict:) for each proposal")
	}
	if !strings.Contains(body, "evidence_ids") {
		t.Fatal("proving-your-work must ground claims in cited evidence_ids (wire format), not fenced blocks")
	}
	for _, banned := range []string{"fenced", "run the exact failing command", "add a regression test", "NOTE line", "Paste the actual output"} {
		if strings.Contains(body, banned) {
			t.Fatalf("re-authored skills must not carry the chatops/execution idiom %q (it conflicts with the read-only JSON wire format)", banned)
		}
	}
}

// The competence skills (#25a) are tool-grounded: a DEEP/STANDARD investigation gets the triage choreography
// and the per-alert-class playbooks, and both cite TG's ACTUAL read-only tools (not generic advice). A cheap
// FAST_AGENT does not carry them.
func TestComposeDeepGetsCompetenceSkills(t *testing.T) {
	body, loaded := Default().Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation})
	if !hasAll(body, "Triage protocol", "Alert-class playbooks") {
		t.Fatalf("deep investigation must load the competence skills; loaded=%v", loaded)
	}
	// Grounded in TG's real tools + the cascade signal — not model-invented capabilities.
	if !hasAll(body, "get-device-status", "get-device-eventlog", "get-active-alerts") {
		t.Fatalf("competence skills must reference TG's actual read-only tools; loaded=%v", loaded)
	}
	// Cascade awareness: co-alerting related hosts point UPSTREAM (propose against the shared cause).
	if !hasAll(body, "cascade", "upstream") {
		t.Fatalf("triage-protocol must carry the cascade / upstream-cause discipline; loaded=%v", loaded)
	}
	fast, _ := Default().Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.FastAgent})
	if strings.Contains(fast, "Alert-class playbooks") || strings.Contains(fast, "Triage protocol") {
		t.Fatal("a fast agent must NOT carry the heavyweight competence skills")
	}
}

// An UNCLASSIFIED context (empty ExecClass — no classification happened) composes the FULL set: seed
// composition fails toward more behavioral guidance, never toward less. Only an explicit FAST_AGENT
// classification earns the compact prompt.
func TestComposeUnclassifiedFailsTowardFullGuidance(t *testing.T) {
	body, loaded := Default().Compose(Context{Phase: PhaseInvestigate})
	if !hasAll(body, "Investigation protocol", "Triage protocol", "Alert-class playbooks") {
		t.Fatalf("an unclassified context must get the full behavioral set; loaded=%v", loaded)
	}
}

// The selector is a pure function of typed signals — no skill body leaks between contexts, and loaded names
// are reported for the audit record.
func TestLoadedNamesReported(t *testing.T) {
	_, loaded := Default().Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.StandardAgent})
	if len(loaded) == 0 {
		t.Fatal("Compose must report the loaded skill names")
	}
	want := map[string]bool{"proving-your-work": true, "conservative-remediation": true}
	got := map[string]bool{}
	for _, n := range loaded {
		got[n] = true
	}
	for n := range want {
		if !got[n] {
			t.Errorf("expected skill %q to load for a standard investigation", n)
		}
	}
}
