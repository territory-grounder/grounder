package skillstore

import "testing"

// ★ THE SHIPPED CAPS HAVE NO ORACLE (found 2026-08-06 verifying TG-63 / TG-65).
//
// TG-63 and TG-65 were fixed by capping how much the creation half may generate, admit and trial per run.
// Every existing test passes those caps EXPLICITLY (creation_test.go sets MaxCandidatesPerTrial: 100, 1, …),
// which is right for testing the mechanism — and it means the DEFAULTS are never exercised.
//
// Production runs on the defaults: cmd/worker/main.go reads
// envInt("TG_SKILL_GEN_MAX_SKILLS", DefaultMaxGenSkillsPerRun) and the three env knobs are all EMPTY on the
// box, so the constants below ARE the shipped behaviour.
//
// I raised each of them to 999 and the entire suite stayed green — restoring precisely the flood and
// accumulation the two tickets describe, with no test objecting. These assertions close that.
//
// They deliberately pin BOUNDS and the RELATIONSHIP, not exact values: the caps are operator-tunable
// (config-not-code) and a future retune should be free to move them, but not to the shape of the bug.

func TestTheGenerationCapCannotReturnToTheFlood(t *testing.T) {
	// TG-63's repro: a GLOBALLY floored dimension (falsifiable_prediction ~1.1) makes Regressed fire for
	// EVERY non-pinned production skill, so an uncapped generate drafts 3 lenses × every skill — 17 drafts
	// across 6 skills, then admit offline-scores each with reasoning-model judge calls and the run times out.
	// The cap is what stops a global floor becoming a global flood.
	if DefaultMaxGenSkillsPerRun < 1 {
		t.Fatalf("DefaultMaxGenSkillsPerRun = %d — a cap below 1 means the creation half can never draft "+
			"anything and the flywheel silently stops generating", DefaultMaxGenSkillsPerRun)
	}
	if DefaultMaxGenSkillsPerRun > 3 {
		t.Errorf("DefaultMaxGenSkillsPerRun = %d. Each drafted skill costs 3-lens generation and then "+
			"offline admission scoring with reasoning-model judge calls; TG-63's timeout came from doing that "+
			"for every regressed skill at once. Raising this past a handful reintroduces the flood — if a "+
			"deployment genuinely needs more, set TG_SKILL_GEN_MAX_SKILLS rather than moving the default.",
			DefaultMaxGenSkillsPerRun)
	}
}

func TestTheAdmitCapStaysBounded(t *testing.T) {
	if DefaultMaxAdmitPerRun < 1 {
		t.Fatalf("DefaultMaxAdmitPerRun = %d — nothing would ever be admitted", DefaultMaxAdmitPerRun)
	}
	if DefaultMaxAdmitPerRun > 5 {
		t.Errorf("DefaultMaxAdmitPerRun = %d. Every admit offline-scores candidate-vs-production with "+
			"reasoning-model calls, so this bounds a per-run COST as well as a queue. TG-63 capped it at 3.",
			DefaultMaxAdmitPerRun)
	}
}

// THE ONE THAT MATTERS MOST. TG-65: the admitted set grows every run until a trial starts, and StartTrial
// needs MinSamplesPerArm × (1+numCandidates) samples inside the window. At bootstrap traffic (~1 judged
// session/day) even a 3-arm trial needs 45 samples at 15/arm and cannot fill a 30-day window — so the trial
// starves, the next cron admits MORE, and the arm count grows without bound.
func TestTheTrialArmCapKeepsATrialFillableAtBootstrapTraffic(t *testing.T) {
	if DefaultMaxCandidatesPerTrial < 1 {
		t.Fatalf("DefaultMaxCandidatesPerTrial = %d — a trial with no candidate arm cannot test anything",
			DefaultMaxCandidatesPerTrial)
	}
	if DefaultMaxCandidatesPerTrial > 2 {
		t.Errorf("DefaultMaxCandidatesPerTrial = %d. The traffic gate needs MinSamplesPerArm × (1+N) samples "+
			"inside the window; at the ~1 judged-session/day bootstrap rate this estate actually runs at, "+
			"N>2 cannot fill and every trial aborts starved — which is TG-65 exactly. Live today: 10 trials "+
			"aborted_no_winner against 1 completed.", DefaultMaxCandidatesPerTrial)
	}
	// And the arm count must stay strictly below the admit cap, or admission outpaces trialling by
	// construction and the admitted-but-untrialed set grows forever — the accumulation TG-65 names.
	if DefaultMaxCandidatesPerTrial >= DefaultMaxAdmitPerRun {
		t.Errorf("DefaultMaxCandidatesPerTrial (%d) >= DefaultMaxAdmitPerRun (%d): each run admits at least "+
			"as many candidates as a trial can ever consume, so the admitted set can only grow. That is the "+
			"unbounded accumulation TG-65 was filed for.",
			DefaultMaxCandidatesPerTrial, DefaultMaxAdmitPerRun)
	}
}
