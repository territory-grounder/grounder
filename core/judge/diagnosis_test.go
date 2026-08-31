package judge

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/proposal"
)

// THE DEFECT THIS DIMENSION CLOSES (TG-201 part 1). The typed diagnosis existed and NOTHING SCORED IT, so
// an agent could emit a root cause its own captured evidence refutes, propose the action anyway, and pay
// nothing: every judged dimension would keep reading the free-text rationale, which says nothing about the
// contradiction. That is the recorded A2 failure — the predecessor reads PVE task history, sees the guest
// was stopped DELIBERATELY and stands down; TG holds the SAME observation and proposes a restart.

// diag is a session carrying a bound diagnosis, recorded by a build that persists the column.
func diag(d proposal.Diagnosis, proposed bool) Session {
	return Session{Ref: "TG-diag", Proposed: proposed, Diagnosis: d, DiagnosisRecorded: true}
}

func bind(d proposal.Diagnosis, ids ...string) proposal.Diagnosis {
	g := map[string]struct{}{}
	for _, i := range ids {
		g[i] = struct{}{}
	}
	return d.BindEvidence(g)
}

// KILLING MUTATION (the ticket's): replace the body of ScoreDiagnosis with a constant — e.g.
// `return 5, "ok", true`. RED here, with the message below: a contradicted diagnosis and a clean one score
// identically, so the claim costs the agent nothing and the A2 failure ships at full marks.
func TestAContradictedDiagnosisIsPenalised(t *testing.T) {
	contradicted := bind(proposal.Diagnosis{
		RootCause:     "the guest crashed and needs restarting",
		Supporting:    []proposal.EvidenceRef{{ID: "lnms-1", Claim: "the guest is not running"}},
		Contradicting: []proposal.EvidenceRef{{ID: "pve-tasks-101", Claim: "the stop was a DELIBERATE operator task"}},
	}, "lnms-1", "pve-tasks-101")
	clean := bind(proposal.Diagnosis{
		RootCause:  "the guest crashed and needs restarting",
		Supporting: []proposal.EvidenceRef{{ID: "lnms-1", Claim: "the guest is not running"}},
	}, "lnms-1")

	bad, badWhy, ok := ScoreDiagnosis(diag(contradicted, true))
	if !ok {
		t.Fatal("a proposing session with a bound diagnosis was scored N/A — the axis cannot grade anything")
	}
	good, _, _ := ScoreDiagnosis(diag(clean, true))
	if bad >= good {
		t.Fatalf("contradicted diagnosis scored %d, clean diagnosis scored %d — an UNSCORED diagnosis is the "+
			"defect: the agent stated a root cause its OWN grounded evidence refutes, proposed the restart "+
			"anyway, and paid nothing for it (the recorded A2 failure)", bad, good)
	}
	if bad != 1 {
		t.Errorf("a self-contradicted asserted root cause must score the floor 1, got %d — it is worse than "+
			"saying nothing, because the refutation was in hand", bad)
	}
	if !strings.Contains(badWhy, "contradicted") {
		t.Errorf("the written reason must name the contradiction (it is what an operator reads on the row); got %q", badWhy)
	}
}

// ONLY A GROUNDED CONTRADICTION COUNTS. A model naming an id the orchestrator never captured must not be
// able to conjure the signal — nor to dodge it. This is HasContradiction's property, asserted THROUGH the
// scorer so the dimension cannot quietly start reading the model's own claim.
func TestAFabricatedContradictionDoesNotMoveTheScore(t *testing.T) {
	d := bind(proposal.Diagnosis{
		RootCause:     "disk full",
		Supporting:    []proposal.EvidenceRef{{ID: "disk-1", Claim: "/ is at 98%"}},
		Contradicting: []proposal.EvidenceRef{{ID: "never-captured", Claim: "df says 12% free"}},
	}, "disk-1")
	got, _, ok := ScoreDiagnosis(diag(d, true))
	if !ok {
		t.Fatal("scored N/A")
	}
	if got == 1 {
		t.Fatal("an id the orchestrator never captured produced the contradiction floor — a signal that can " +
			"be conjured from nothing will eventually be conjured from nothing")
	}
	if got != 4 {
		t.Fatalf("score=%d, want 4 — the fabricated ref is exactly one UNCITED assertion and must be counted as one", got)
	}
}

// HONEST UNCERTAINTY MUST SCORE WELL — the explicit test the ticket asks for.
//
// "I ruled out X and Y against these captured observations; the root cause is still unknown" is GOOD
// triage. If the rubric graded it as failure, the cheapest way for an agent to raise its score would be to
// invent a confident root cause it cannot ground — the rubric would be paying for fabrication, and the
// typed claim was introduced to expose exactly that.
//
// KILLING MUTATION: score the absence of a root cause as a defect (e.g. drop the AssertsRootCause guard so
// `CitedAssertions()==0`-style floors apply, or return 2 whenever RootCause is empty). RED — admitting
// uncertainty would then score below a confident guess.
func TestHonestUncertaintyScoresWell(t *testing.T) {
	honest := bind(proposal.Diagnosis{
		RuledOut: []proposal.RuledOut{
			{Cause: "disk full", Reason: "/ is at 12%", ID: "disk-1"},
			{Cause: "OOM", Reason: "no oom-kill in the journal", ID: "journal-1"},
		},
	}, "disk-1", "journal-1")
	got, why, ok := ScoreDiagnosis(diag(honest, false))
	if !ok {
		t.Fatal("an honest 'ruled these out, cause unknown' diagnosis was scored N/A — it is a claim, and it " +
			"must be gradeable so that admitting uncertainty can be REWARDED, not merely tolerated")
	}
	if got < 4 {
		t.Fatalf("honest uncertainty scored %d/5 (%q) — a rubric that punishes admitting uncertainty trains "+
			"the agent to fabricate a confident cause it cannot ground", got, why)
	}
	// The same shape must not be beaten by a confident, unsupported guess.
	guess := bind(proposal.Diagnosis{RootCause: "probably the service crashed"}, "disk-1")
	if g, _, _ := ScoreDiagnosis(diag(guess, true)); g >= got {
		t.Fatalf("an ungrounded confident guess scored %d and honest uncertainty scored %d — the rubric pays "+
			"for fabricated confidence", g, got)
	}
}

// Disclosing disconfirming evidence WITHOUT committing to a cause is disclosure, not self-contradiction.
// Penalising it would teach the agent to hide the evidence rather than to withdraw the claim.
func TestDisclosingEvidenceAgainstNoStatedCauseIsNotPenalised(t *testing.T) {
	d := bind(proposal.Diagnosis{
		Contradicting: []proposal.EvidenceRef{{ID: "pve-tasks-101", Claim: "the stop was deliberate — the crash theory does not hold"}},
		RuledOut:      []proposal.RuledOut{{Cause: "crash", Reason: "the stop was an operator task", ID: "pve-tasks-101"}},
	}, "pve-tasks-101")
	got, _, ok := ScoreDiagnosis(diag(d, false))
	if !ok {
		t.Fatal("scored N/A")
	}
	if got < 4 {
		t.Fatalf("recording evidence against a hypothesis while claiming NO cause scored %d — that is honest "+
			"disclosure; penalising it teaches the agent to hide the observation instead of dropping the claim", got)
	}
}

// AN ABSENT DIAGNOSIS UNDER A CAUSAL CLAIM IS GRADED. Proposing a remedy asserts what it remedies; supplying
// no claim at all is not neutrality, it is the pre-TG-201 status quo the typed field exists to replace.
func TestAProposalWithNoDiagnosisIsPenalised(t *testing.T) {
	got, why, ok := ScoreDiagnosis(Session{Ref: "r", Proposed: true, DiagnosisRecorded: true})
	if !ok {
		t.Fatal("a proposal with no diagnosis was scored N/A — the absent-claim case is exactly the one the " +
			"dimension has to price, or the field stays optional in practice forever")
	}
	if got > 2 {
		t.Fatalf("a proposal that bound no diagnosis scored %d/5 (%q) — a remedy asserts what it remedies", got, why)
	}
}

// N/A IS NOT A FLOOR — twice over, and both are the TG-61 lesson (a dimension floored across a whole
// population fired the flywheel's Regressed trigger for every skill at once).
func TestNonApplicableSessionsAreOmittedNotFloored(t *testing.T) {
	// (a) A record from before migration 0056: the column did not exist, so an empty claim is the SCHEMA's
	// silence, not the agent's. Grading it would retroactively fail every historical session.
	if _, _, ok := ScoreDiagnosis(Session{Ref: "old", Proposed: true, DiagnosisRecorded: false}); ok {
		t.Error("a pre-migration record was scored — its empty diagnosis means the field did not exist, and " +
			"retro-grading it floors thousands of sessions against a rule they were never offered")
	}
	// (b) A grounded stand-down that supplied no diagnosis asserted no cause. correct_diagnosis and
	// evidence_grounded already grade its conclusion; it owes no formal root-cause artifact.
	if _, _, ok := ScoreDiagnosis(Session{Ref: "stop", Proposed: false, DiagnosisRecorded: true}); ok {
		t.Error("a no-proposal stop with no diagnosis was scored — no action, no causal claim, nothing to grade")
	}
}

// UNCITED ASSERTIONS COST, PROPORTIONALLY. "assertion 2 of 4 is uncited" is the thing the flat []string
// could never express; the score has to move with the count or the type bought nothing.
func TestUncitedAssertionsLowerTheScoreProportionally(t *testing.T) {
	mk := func(uncited int) Session {
		d := proposal.Diagnosis{
			RootCause:  "journald grew unbounded",
			Supporting: []proposal.EvidenceRef{{ID: "disk-1", Claim: "/ is at 98%"}},
		}
		for i := 0; i < uncited; i++ {
			d.Supporting = append(d.Supporting, proposal.EvidenceRef{Claim: "asserted with no observation"})
		}
		return diag(bind(d, "disk-1"), true)
	}
	prev := 6
	for u := 0; u <= 3; u++ {
		got, _, ok := ScoreDiagnosis(mk(u))
		if !ok {
			t.Fatalf("uncited=%d scored N/A", u)
		}
		if got >= prev {
			t.Fatalf("uncited=%d scored %d, not below the %d scored at uncited=%d — ungrounded assertions must "+
				"cost, or 'assertion 2 of 4 is uncited' is recorded and never charged for", u, got, prev, u-1)
		}
		prev = got
	}
	if prev < 2 {
		t.Fatalf("the scale bottomed out at %d — 1 is reserved for the self-contradiction case, which is a "+
			"strictly worse failure than sloppy citation", prev)
	}
}

// A NAMED CAUSE WITH NOTHING BOUND is invisible to an uncited COUNT (zero refs ⇒ zero uncited), so it must
// be caught by its own rule or a bare assertion scores full marks.
func TestANamedCauseGroundedInNothingScoresLow(t *testing.T) {
	got, _, ok := ScoreDiagnosis(diag(bind(proposal.Diagnosis{RootCause: "the service crashed"}, "disk-1"), true))
	if !ok {
		t.Fatal("scored N/A")
	}
	if got > 2 {
		t.Fatalf("a root cause with zero grounded assertions scored %d/5 — an assertion with no support is a "+
			"guess with a citation field, and counting only UNCITED refs would score it perfect", got)
	}
}

// The dimension's NAME must come from the one rubric source, and it must not have been smuggled into the
// LLM reply schema: Dimensions is both what the judge model is asked for and the eval Overall's fixed
// denominator, and this axis belongs to neither.
func TestDeterministicDimensionIsDeclaredOnceAndNotAnLLMAxis(t *testing.T) {
	if DimDiagnosisGrounded != "diagnosis_grounded" {
		t.Fatalf("dimension name %q — the durable session_judgment rows are keyed on it", DimDiagnosisGrounded)
	}
	if LoadedRubric().DeterministicDimensions[0] != DimDiagnosisGrounded {
		t.Fatal("DimDiagnosisGrounded is not sourced from rubric.json — a second declaration is a second thing to drift")
	}
	for _, d := range Dimensions {
		if d == DimDiagnosisGrounded {
			t.Fatal("diagnosis_grounded entered judge.Dimensions — that asks the judge MODEL to re-author a " +
				"fact the orchestrator bound, and widens the eval Overall denominator so every historical " +
				"scorecard moves for a reason unrelated to agent quality")
		}
	}
	if !strings.Contains(Prompt(goldenSession), "correct_diagnosis") {
		t.Fatal("the golden prompt lost its dimensions")
	}
	if strings.Contains(Prompt(goldenSession), DimDiagnosisGrounded) {
		t.Fatal("the deterministic dimension leaked into the judge prompt — the model must not be asked to score it")
	}
	if DiagnosisRule() == "" {
		t.Fatal("the dimension ships with no written calibration — an axis nobody can read the rule for is one nobody can audit")
	}
}

// Facts() must carry BOTH halves off the durable record. The field existing on Session is worthless if the
// projection drops it — which is exactly how the committed prediction used to arrive blank at the judge.
func TestFactsCarriesTheDiagnosisOffTheRecord(t *testing.T) {
	row := TriageRow{
		ExternalRef:       "r-9",
		Proposed:          true,
		DiagnosisRecorded: true,
		Diagnosis: proposal.Diagnosis{
			RootCause:     "the guest crashed",
			Contradicting: []proposal.EvidenceRef{{ID: "pve-tasks-101", Claim: "deliberate stop", Cited: true}},
		},
	}
	f := row.Facts()
	if !f.DiagnosisRecorded || f.Diagnosis.RootCause != "the guest crashed" || !f.Diagnosis.HasContradiction() {
		t.Fatalf("Facts() dropped the typed claim: %+v", f.Diagnosis)
	}
	if got, _, ok := ScoreDiagnosis(f); !ok || got != 1 {
		t.Fatalf("the record's contradicted diagnosis scored %d (applicable=%v) through Facts() — the durable "+
			"path is what the live judge cron scores", got, ok)
	}
}
