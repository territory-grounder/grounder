package attribution

import (
	"strings"
	"testing"
	"time"
)

// TG-407 / REQ-2304 half 2: covered-but-empty (a reader that AFFIRMATIVELY COVERS the subject's audit trail
// recorded NO actor) was structurally inexpressible — Covered rode on evidence ROWS and an empty result has no
// row to carry the flag, so attributed-suspicious fired 0/3,383 sessions. A coverage marker (Covered=true, no
// actor) carries the signal. This test group pins the OBSERVE-ONLY half of the disposition: the plain Attribute()
// (which supplies no session Observation) never escalates covered-but-empty — covered-but-empty is the COMMON
// no-actor fault (a crash, an in-flight job, a system-triggered change all leave no actor entry, indistinguishable
// here from an unaudited mutation), so minting suspicion on it alone would route the majority of no-actor sessions
// to SECURITY and neuter auto-heal. The taxonomy stays Unattributable; f.CoveredButEmpty records the fact. The
// ARMED half — the SAME signal escalates once the session confirms an observed mutation — is proven separately by
// the AttributeObserving truth-table below (the mutation flag is the one discriminator between them).
//
// Killing mutations: (a) escalate covered-but-empty UNCONDITIONALLY to AttributedSuspicious → the Unattributable
// assertion here RED (the flood the review caught — an escalation must require an observed mutation); (b) drop the
// f.CoveredButEmpty=true assignment → the surfaced-signal assertion RED (the signal stays inexpressible).
func TestCoveredButEmptyIsSurfacedNotEscalated(t *testing.T) {
	inWindow := epoch.Add(-5 * time.Minute)
	f := Attribute("web01", "start-guest", []Evidence{CoverageMarker("pve", "web01", inWindow)}, nil, baseCfg)

	if f.Taxonomy != Unattributable {
		t.Fatalf("covered-but-empty must stay Unattributable (NOT escalated — that flood neuters auto-heal), got %v", f.Taxonomy)
	}
	if !f.CoveredButEmpty {
		t.Fatalf("covered-but-empty must SET the surfaced signal f.CoveredButEmpty (REQ-2304 half 2, observe-only)")
	}
	if !hasWarning(f, "covered-but-empty") {
		t.Fatalf("covered-but-empty must carry a review warning, got %v", f.Warnings)
	}
	// A marker is never counted as actor evidence.
	for _, e := range f.Evidence {
		if IsCoverageMarker(e) {
			t.Fatalf("a coverage marker must not be counted as actor evidence: %+v", f.Evidence)
		}
	}
}

// GENUINELY BLIND stays unattributable AND does NOT raise the signal: no covering reader ⇒ nothing to surface.
// This is the discrimination the whole design turns on — silence-because-unobserved is not covered-but-empty.
func TestNoCoverageIsNotCoveredButEmpty(t *testing.T) {
	f := Attribute("web01", "start-guest", nil, nil, baseCfg)
	if f.Taxonomy != Unattributable {
		t.Fatalf("no coverage + no evidence must stay unattributable (REQ-2303), got %v", f.Taxonomy)
	}
	if f.CoveredButEmpty {
		t.Fatalf("genuinely-blind must NOT set CoveredButEmpty — there was no covering reader to be empty")
	}
}

// A marker never OVERRIDES real actor evidence: a covering reader that DID find a sanctioned actor resolves on
// the actor, and the covered-but-empty signal is NOT raised (there was an actor).
func TestCoverageMarkerDoesNotOverrideRealActor(t *testing.T) {
	inWindow := epoch.Add(-5 * time.Minute)
	f := Attribute("web01", "start-guest", []Evidence{
		CoverageMarker("pve", "web01", inWindow),
		ev("pve", "root@pam", "vzstop", "web01", inWindow), // a sanctioned actor (baseCfg.Sanctioned["pve"])
	}, nil, baseCfg)
	if f.Taxonomy != AttributedAuthorized {
		t.Fatalf("a real sanctioned actor must resolve authorized despite a co-present coverage marker, got %v (candidates=%v)", f.Taxonomy, f.Candidates)
	}
	if f.CoveredButEmpty {
		t.Fatalf("CoveredButEmpty must not be set when admissible actor evidence exists, got %+v", f)
	}
}

// An OUT-OF-WINDOW coverage marker is not admissible coverage — a reader that covered a DIFFERENT window says
// nothing about this mutation, so it neither escalates nor raises the signal.
func TestOutOfWindowCoverageMarkerIsNotCoveredButEmpty(t *testing.T) {
	stale := epoch.Add(-2 * time.Hour) // older than the 30m window
	f := Attribute("web01", "start-guest", []Evidence{CoverageMarker("pve", "web01", stale)}, nil, baseCfg)
	if f.Taxonomy != Unattributable || f.CoveredButEmpty {
		t.Fatalf("an out-of-window coverage marker must not escalate or raise the signal, got taxonomy=%v covered-but-empty=%v", f.Taxonomy, f.CoveredButEmpty)
	}
}

// THE COMMON NO-ACTOR CASE MUST NOT FLOOD (the review's central concern): a covering reader clean-miss on an
// ordinary fault stays Unattributable — auto-heal is preserved — while still surfacing the signal for review.
// This is the end-to-end join the reader-level tests (still-running / blank-actor / no-match) could not make.
func TestCommonNoActorFaultDoesNotFlood(t *testing.T) {
	inWindow := epoch.Add(-5 * time.Minute)
	// A journal reader covered the host and found no privileged action (a crash — the dominant fault shape).
	f := Attribute("web01", "start-service", []Evidence{CoverageMarker("journal", "web01", inWindow)}, nil, baseCfg)
	if f.Taxonomy != Unattributable {
		t.Fatalf("an ordinary no-actor fault (crash) must stay Unattributable so it can auto-heal — NOT be escalated to security, got %v", f.Taxonomy)
	}
	if !f.CoveredButEmpty {
		t.Fatalf("...while still surfacing covered-but-empty for review")
	}
}

func hasWarning(f Finding, sub string) bool {
	for _, w := range f.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// TG-407 / REQ-2304 half 2 — the ARMED disposition. Covered-but-empty (a reader that AFFIRMATIVELY COVERS the
// subject's audit trail, ANSWERED, and recorded NO actor) escalates to attributed-suspicious IFF the session
// carries a confirmed OBSERVED MUTATION (Observation.MutationObserved). The mutation signal is the discriminator
// the review (de3bbb19) named as the missing ingredient: without it, covered-but-empty is the COMMON no-actor
// fault (a crash) and must NOT flood SECURITY; with it, a state change occurred that the covering domain has no
// actor for — the intrusion signal. These drive AttributeObserving directly; the plain Attribute() (the zero
// Observation) keeps the observe-only tests above green, proving the escalation is unreachable without a mutation.
//
// Killing mutations for case (1): drop the escalation in the obs.MutationObserved branch (leave the taxonomy
// Unattributable) → RED; ignore obs entirely (the pre-TG-407 body) → RED. Confirmed RED before the gate landed.

// (1) Covered + answered + zero-actor + OBSERVED MUTATION ⇒ attributed-suspicious.
func TestCoveredEmptyOnObservedMutationEscalatesSuspicious(t *testing.T) {
	inWindow := epoch.Add(-5 * time.Minute)
	obs := Observation{MutationObserved: true}
	f := AttributeObserving("web01", "start-guest", []Evidence{CoverageMarker("pve", "web01", inWindow)}, nil, baseCfg, obs)
	if f.Taxonomy != AttributedSuspicious {
		t.Fatalf("covered + answered + zero-actor + OBSERVED MUTATION ⇒ attributed-suspicious (REQ-2304 half 2), got %v (candidates=%v)", f.Taxonomy, f.Candidates)
	}
	if !f.CoveredButEmpty {
		t.Fatalf("the covered-but-empty fact must still be surfaced on the escalation, got %+v", f)
	}
	if !hasWarning(f, "OBSERVED MUTATION") {
		t.Fatalf("the escalation must carry a warning naming the observed-mutation intrusion signal, got %v", f.Warnings)
	}
}

// (2) Covered + answered + an ACTOR PRESENT resolves on the ACTOR, never on emptiness — even with a mutation.
func TestObservedMutationWithActorResolvesOnActorNotEmptiness(t *testing.T) {
	inWindow := epoch.Add(-5 * time.Minute)
	obs := Observation{MutationObserved: true}
	f := AttributeObserving("web01", "start-guest", []Evidence{
		CoverageMarker("pve", "web01", inWindow),
		ev("pve", "root@pam", "vzstop", "web01", inWindow), // sanctioned (baseCfg.Sanctioned["pve"])
	}, nil, baseCfg, obs)
	if f.Taxonomy != AttributedAuthorized {
		t.Fatalf("an observed mutation WITH a sanctioned actor entry resolves on the actor (attributed-authorized), not covered-but-empty, got %v (candidates=%v)", f.Taxonomy, f.Candidates)
	}
	if f.CoveredButEmpty {
		t.Fatalf("CoveredButEmpty must not be set when admissible actor evidence exists, got %+v", f)
	}
}

// (3) A reader that FAILED/timed out contributes NO marker (it returns an error; the fanout/activity records a
// warning and drops it), so Attribute never sees affirmative coverage. Even WITH an observed mutation it must
// stay Unattributable — a failed read is never suspicious (guard (b); REQ-2307).
func TestFailedReaderNeverSuspiciousEvenWithObservedMutation(t *testing.T) {
	obs := Observation{MutationObserved: true}
	// The failed reader yields nothing at all — no marker, no actor rows.
	f := AttributeObserving("web01", "start-guest", nil, nil, baseCfg, obs)
	if f.Taxonomy != Unattributable {
		t.Fatalf("a failed reader (no coverage marker) must stay unattributable even under an observed mutation — a failed read is never suspicious (REQ-2307), got %v", f.Taxonomy)
	}
	if f.CoveredButEmpty {
		t.Fatalf("no covering reader answered, so covered-but-empty must NOT be set, got %+v", f)
	}
}

// (4) A reader that covers a DIFFERENT subject leaves THIS subject uncovered — coverage cannot be claimed for a
// target no reader covered. Even WITH an observed mutation it stays Unattributable, not suspicious.
func TestUncoveredSubjectNeverSuspiciousEvenWithObservedMutation(t *testing.T) {
	inWindow := epoch.Add(-5 * time.Minute)
	obs := Observation{MutationObserved: true}
	// The only marker covers db02; web01 (the subject) is uncovered.
	f := AttributeObserving("web01", "start-guest", []Evidence{CoverageMarker("pve", "db02", inWindow)}, nil, baseCfg, obs)
	if f.Taxonomy != Unattributable {
		t.Fatalf("no reader covers the SUBJECT (only db02 is covered) ⇒ unattributable even under an observed mutation — coverage cannot be claimed for a target nobody covered, got %v", f.Taxonomy)
	}
	if f.CoveredButEmpty {
		t.Fatalf("the subject was not covered, so covered-but-empty must NOT be set, got %+v", f)
	}
}

// THE MUTATION FLAG IS THE DISCRIMINATOR (vacuity guard): byte-identical covered-but-empty inputs resolve
// observe-only with MutationObserved=false and attributed-suspicious with =true. Without this pair, case (1)
// and TestCommonNoActorFaultDoesNotFlood could each be passing for some unrelated reason; together they prove
// the obs.MutationObserved gate — and nothing else — decides.
func TestMutationFlagIsWhatDiscriminates(t *testing.T) {
	inWindow := epoch.Add(-5 * time.Minute)
	evs := []Evidence{CoverageMarker("journal", "web01", inWindow)}
	off := AttributeObserving("web01", "start-service", evs, nil, baseCfg, Observation{MutationObserved: false})
	if off.Taxonomy != Unattributable || !off.CoveredButEmpty {
		t.Fatalf("MutationObserved=false ⇒ observe-only (Unattributable + CoveredButEmpty), got taxonomy=%v covered-but-empty=%v", off.Taxonomy, off.CoveredButEmpty)
	}
	on := AttributeObserving("web01", "start-service", evs, nil, baseCfg, Observation{MutationObserved: true})
	if on.Taxonomy != AttributedSuspicious {
		t.Fatalf("MutationObserved=true on the SAME inputs ⇒ attributed-suspicious, got %v — the flag is the only difference, so it must be what decides", on.Taxonomy)
	}
}
