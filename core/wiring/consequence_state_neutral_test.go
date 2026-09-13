package wiring

// A CONSEQUENCE IS PRINTED IN TWO STATES AND MUST BE TRUE IN BOTH (TG-354).
//
// Every seam in the register carries a yield Unit, so every one can be reported DARK (never bound) or
// STARVED (bound, running, producing nothing). The same Consequence string is printed either way, so a text
// that names WHY a seam is dark is false in the starved report — and both appear on one line.
//
// tracker.entry opened "no entry tracker is bound:". Measured 2026-08-06 in the STARVED state:
//
//	boot:        tracker: entry ticket read/transitioned via youtrack
//	module TEST: tracker/youtrack among the 10 that can prove themselves
//	register:    tracker.entry: starved — 306 entry-ticket lookups offered, 0 tickets read or
//	             transitioned produced [no entry tracker is bound: …]
//
// suppression.tier1 opened "no tier-1 suppression chain is configured:" — the wording of the OTHER boot
// branch. The running worker prints the first one:
//
//	suppression: tier-1 gate active — 0 freeze, 0 fold(s), 0 schedule(s), 0 pattern(s), 0 rule(s),
//	             dedup 10m0s
//	register:    suppression.tier1: starved — 171 alerts admitted, 0 suppressed
//
// The gate is ACTIVE with an empty rule set. That is not the same as unconfigured and has a different
// remedy: nothing is missing from the deployment, the operator has declared no windows yet.
//
// escalation.page and gov.notify were the third and fourth, verified POSITIVELY rather than by absence from
// a list:
//
//	notifier: governance notices/polls delivered via matrix
//	escalation requeue lane: durable store wired (per-incident cap 3, re-check delay 15m0s) —
//	                         fires via the FireDue cron, pages via the notifier
//	escalation FireDue: cron armed (*/5 * * * *)
//	module test: … notifier/matrix … can prove themselves
//
// Both opened by naming a nil field. Both are bound. They report UNOBSERVED — nothing measures their yield —
// which is a different problem with a different remedy, and the old texts sent a reader looking for a
// notifier that is already there.
//
// TWO SEAMS ARE DELIBERATELY STILL OUT. discovery.service and lessons.feed are DECLARED-DARK on this
// deployment ("discovery: NO service-observing probe configured", "TG_LESSONS_SOURCE_FILE is unset"), so
// their cause assertions are ACCURATE — for a genuinely dark seam, naming the missing thing is the most
// useful sentence available. vote.inbound also opens with a cause and I have no positive evidence either
// way, so it stays out rather than being assumed. Membership here is earned by measurement.

import (
	"strings"
	"testing"
)

// verifiedStateNeutral are the seams whose STARVED report was OBSERVED on the running system, with the costs
// their text must keep. Membership is earned by measurement, not by inspection.
var verifiedStateNeutral = map[Seam][]string{
	SeamTrackerEntry:   {"dedup", "RESOLVED", "close-out"},
	SeamSuppression:    {"maintenance windows", "flap patterns", "triage session"},
	SeamEscalationPage: {"FireDue", "permanently lost", "no retry"},
	SeamGovNotify:      {"judge-death page", "log.Printf", "NO operator surface"},
	SeamVoteInbound:    {"approver set", "inbound path", "console"},
}

// assertsCause reports whether a consequence NAMES A CAUSE rather than stating a cost.
//
// SHAPE, NOT A PHRASE LIST, and that correction is the point. My first version listed the literal openings I
// had already seen. When suppression.tier1 joined the verified set, restoring ITS cause assertion ("no tier-1
// suppression chain is configured") sailed straight through — the exact string was not on the list. A guard
// that only catches the instances already fixed catches nothing new.
//
// The shape is: a consequence that OPENS by declaring something absent ("no <thing> …"), or that names a
// nil/unset/unbound/unconfigured field. Both describe the seam's CONFIGURATION, which is what differs
// between the two states.
//
// Deliberately NOT matched: "nothing is suppressed before triage" (a cost that happens to begin with the
// letters "no" — the trailing space in "no " excludes it) and "the incident's own ticket is not read" (a
// cost phrased in the negative). The distinction is whether the sentence describes the SEAM's wiring or the
// SYSTEM's loss.
// THE RULE IS CONSERVATIVE AND THAT COSTS A REWORD SOMETIMES. It caught my own first draft of gov.notify's
// replacement — "no governance notice reaches an operator" — which is a COST, not a cause, but opens the
// forbidden way. Rephrased to "governance notices reach no operator". A guard that occasionally makes you
// phrase a cost AS a cost is doing its job; loosening it to admit a leading "no" would readmit every case
// it exists to catch.
func assertsCause(consequence string) (string, bool) {
	c := strings.ToLower(strings.TrimSpace(consequence))
	if strings.HasPrefix(c, "no ") {
		return `opens by declaring something absent ("no …")`, true
	}
	for _, marker := range []string{"is nil:", "is unset:", "is not bound", "is not configured", "is not wired"} {
		if strings.Contains(c, marker) {
			return marker, true
		}
	}
	return "", false
}

// KILLING MUTATION: restore either cause-asserting opening. RED, naming the seam.
func TestVerifiedConsequencesAreTrueInBothStates(t *testing.T) {
	if len(verifiedStateNeutral) < 2 {
		t.Fatalf("the verified set holds %d seam(s) — two were measured in the starved state and both "+
			"should be covered", len(verifiedStateNeutral))
	}
	for seam, mustKeep := range verifiedStateNeutral {
		var cons string
		var found bool
		for _, s := range All() {
			if s.ID == seam {
				cons, found = s.Consequence, true
			}
		}
		if !found {
			t.Fatalf("%s is no longer in the seam register — this guard is scanning for a seam that does "+
				"not exist and would pass on any text", seam)
		}
		if strings.TrimSpace(cons) == "" {
			t.Fatalf("%s has an EMPTY consequence — the register would report a starved seam with no "+
				"statement of what it costs, which is the condition it exists to end", seam)
		}
		if bad, yes := assertsCause(cons); yes {
			t.Errorf("%s's consequence %s. The same string is printed in the STARVED report, where the seam "+
				"IS bound and running — and both of these were measured in that state on 2026-08-06. State "+
				"the COST; the register already reports which state produced it.\nGot: %s", seam, bad, cons)
		}
		// A text made state-neutral by becoming CONTENTLESS trades a wrong message for an empty one, and
		// empty looks deliberate. The specific cost has to survive the rewrite.
		for _, must := range mustKeep {
			if !strings.Contains(cons, must) {
				t.Errorf("%s's consequence no longer mentions %q — the rewrite dropped the cost it exists "+
					"to state.\nGot: %s", seam, must, cons)
			}
		}
	}
}

// The shape rule must reject a cause assertion it has never seen, or it is a phrase list wearing a
// function's clothes.
func TestTheShapeRuleCatchesUnseenCauseAssertions(t *testing.T) {
	for _, c := range []struct {
		text string
		want bool
	}{
		{"no tier-1 suppression chain is configured: TG spends a full triage session", true},
		{"no entry tracker is bound: the investigation cannot read", true},
		{"deps.Notify is nil: every governance notice is dropped", true},
		{"TG_LESSONS_SOURCE_FILE is unset: the corpus can grow only from", true},
		{"no future phrasing anyone has written yet: something is lost", true}, // the point of a shape rule
		// Costs, which must pass.
		{"nothing is suppressed before triage: TG spends a full triage session", false},
		{"the incident's own ticket is not read: the investigation cannot see it", false},
		{"the agent cannot read the alerting host: every resolution is a guess", false},
		{"the world-model discovery pass never runs: manifests are never drafted", false},
	} {
		if _, got := assertsCause(c.text); got != c.want {
			t.Errorf("assertsCause(%.52q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// VACUITY FLOOR: the register must be populated and its seams must carry yield Units, or the premise of the
// guard above — that one string is printed in two states — no longer holds.
func TestTheSeamRegisterIsPopulated(t *testing.T) {
	all := All()
	if len(all) < 8 {
		t.Fatalf("the seam register holds %d seam(s) — the guard above iterates over almost nothing", len(all))
	}
	withUnit := 0
	for _, s := range all {
		if s.Unit.Offered != "" {
			withUnit++
		}
	}
	if withUnit == 0 {
		t.Fatal("no seam declares a yield Unit — none can be reported STARVED, and the premise of the guard " +
			"above no longer holds")
	}
}
