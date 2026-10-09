package judge

import (
	"strings"
	"testing"
)

// THE JUDGE MUST BE ABLE TO SEE WHETHER AN ACTOR WAS KNOWN.
//
// It was shown "CITED EVIDENCE IDS" as OPAQUE IDS, so it could tell that SOME evidence was cited and never
// whether the conclusion accounted for a KNOWN actor — which is most of what `correct_diagnosis` and
// `evidence_grounded` are asking. Measured live: 508 of 1228 sessions carried a resolved attribution taxonomy
// and 465 carried reader-captured evidence, and none of it reached the scorer. Any improvement from surfacing
// attribution to the agent would therefore have been UNMEASURABLE — the instrument could not see it.

func TestPromptCarriesTheAttributionFact(t *testing.T) {
	s := goldenSession
	s.ActorAttribution = "attributed-authorized"
	s.ActorEvidenceCount = 3
	got := Prompt(s)
	if !strings.Contains(got, `taxonomy="attributed-authorized"`) {
		t.Fatalf("the resolved taxonomy must reach the judge; got:\n%s", got)
	}
	if !strings.Contains(got, "evidence_records=3") {
		t.Errorf("the evidence count must reach the judge; got:\n%s", got)
	}
}

// The RECORDS themselves must NOT cross this boundary. They are external text — actor names, verbs and refs
// out of other systems' logs — and carry no scoring value the count does not (REQ-2313: rendered evidence is
// data, and an untrusted payload should not be exported to a scorer for nothing).
func TestPromptCarriesTheCountNotTheRawRecords(t *testing.T) {
	s := goldenSession
	s.ActorAttribution = "unattributable"
	s.ActorEvidenceCount = 2
	got := Prompt(s)
	for _, leaked := range []string{"root@pam", "vzstop", "UPID:", "journal"} {
		if strings.Contains(got, leaked) {
			t.Errorf("raw evidence content %q must not be exported into the judge prompt", leaked)
		}
	}
}

// Facts() must actually copy them off the durable record — the field existing on Session is not enough if the
// projection drops it, which is exactly how the prediction used to arrive blank.
func TestFactsCopiesAttributionOffTheRecord(t *testing.T) {
	r := TriageRow{ExternalRef: "r-1", ActorAttribution: "authorized-test", ActorEvidenceCount: 5}
	f := r.Facts()
	if f.ActorAttribution != "authorized-test" || f.ActorEvidenceCount != 5 {
		t.Fatalf("Facts() dropped the attribution: %+v", f)
	}
}

// An UNATTRIBUTED session renders honestly rather than omitting the line — a missing line and "no actor known"
// are different facts, and the judge must be able to tell them apart.
func TestUnattributedSessionStillRendersTheFact(t *testing.T) {
	got := Prompt(goldenSession) // no attribution set
	if !strings.Contains(got, `ACTOR ATTRIBUTION: taxonomy="" evidence_records=0`) {
		t.Errorf("an unattributed session must still render the fact explicitly; got:\n%s", got)
	}
}

// MUTATION CONTROL. The fact is only load-bearing if changing it changes what the judge reads. Two sessions
// differing ONLY in attribution must produce different prompts — if they do not, the plumbing is decorative
// and every assertion above is vacuous.
func TestMutationControl_AttributionChangesWhatTheJudgeReads(t *testing.T) {
	a, b := goldenSession, goldenSession
	a.ActorAttribution, a.ActorEvidenceCount = "attributed-authorized", 4
	b.ActorAttribution, b.ActorEvidenceCount = "unattributable", 0
	if Prompt(a) == Prompt(b) {
		t.Fatal("two sessions with DIFFERENT attribution produced an identical prompt — the fact never reaches " +
			"the judge, so no attribution improvement could ever be scored")
	}
	t.Log("mutation control holds: attribution is visible to the scorer")
}
