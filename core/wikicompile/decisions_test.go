package wikicompile

import (
	"strings"
	"testing"
	"time"
)

func dt(kind string, total, withheld, day int) DecisionTally {
	return DecisionTally{Decision: kind, Total: total, Withheld: withheld,
		Newest: time.Date(2026, 7, day, 12, 0, 0, 0, time.UTC)}
}

// productionShape mirrors the real ledger's proportions (measured 2026-08-01).
func productionShape() DecisionInputs {
	return DecisionInputs{
		LedgerTotal: 8570,
		Tallies: []DecisionTally{
			dt("suppress:escalate", 3414, 0, 30),
			dt("classify:POLL_PAUSE", 1315, 1315, 30),
			dt("human:poll-obsolete:subject-recovered", 956, 0, 29),
			dt("actuate:exec-log", 598, 0, 30),
			dt("actuate:refuse", 143, 143, 28),
		},
	}
}

// TestWithheldIsReportedAsAShareNotBuried — 1,616 of production's decisions are TG declining to act, and
// a surface that only reports what TG DID would describe less than a fifth of its governance behaviour.
//
// RED MUTATION CONTROL (executed 2026-08-01): summing Total instead of Withheld makes the withheld share
// equal 100%; restored green.
func TestWithheldIsReportedAsAShareNotBuried(t *testing.T) {
	body := CompileDecisions(productionShape()).Body
	if !strings.Contains(body, "1458") {
		t.Errorf("the withheld total (1315+143) must be stated; body:\n%s", body)
	}
	if !strings.Contains(body, "WITHHELD") {
		t.Error("withheld must be a named category, not a column an operator has to sum")
	}
	// And it must NOT be framed as failure.
	if !strings.Contains(body, "not a failure") {
		t.Error("a withheld decision is the governance working — framing it as failure would teach an " +
			"operator to read correct refusals as defects")
	}
}

// TestObsoletePollsAreSurfacedAsTheCostOfWaiting — the figure this page exists for.
//
// 956 times in production, TG paused for a human vote and the incident resolved itself before anyone
// voted. Correct behaviour, and invisible everywhere. It is the honest cost of the polling posture.
//
// RED MUTATION CONTROL (executed 2026-08-01): matching the decision string exactly instead of by prefix
// counts zero (the real strings carry a reason suffix); restored green.
func TestObsoletePollsAreSurfacedAsTheCostOfWaiting(t *testing.T) {
	body := CompileDecisions(productionShape()).Body
	if !strings.Contains(body, "956 poll(s) went obsolete") {
		t.Errorf("the obsolete-poll count must be surfaced; body:\n%s", body)
	}
	if !strings.Contains(body, "This is not a defect count") {
		t.Error("standing down on a recovered subject is CORRECT — the page must say so, or the number " +
			"reads as an error rate and argues for less polling on false grounds")
	}
	if !strings.Contains(body, "stopped mattering while they were being asked") {
		t.Error("it must say what the number MEASURES, or it is a statistic with no argument attached")
	}
}

// TestPrefixMatchSurvivesReasonSuffixes — the ledger's decision strings carry a reason suffix
// (`human:poll-obsolete:subject-recovered`). An exact match against the family name counts zero.
func TestPrefixMatchSurvivesReasonSuffixes(t *testing.T) {
	in := DecisionInputs{LedgerTotal: 10, Tallies: []DecisionTally{
		dt("human:poll-obsolete:subject-recovered", 5, 0, 30),
		dt("human:poll-obsolete:some-other-reason", 3, 0, 29),
	}}
	if !strings.Contains(CompileDecisions(in).Body, "8 poll(s) went obsolete") {
		t.Error("every human:poll-obsolete:* reason must fold into the family count — matching the bare " +
			"family name would count zero against real ledger strings")
	}
}

// TestDigestStatesItsOwnCoverage — the tallies name decision kinds; the chain may hold rows that carry
// none. Implying the tallies ARE the ledger would be the coverage-page defect in miniature.
func TestDigestStatesItsOwnCoverage(t *testing.T) {
	body := CompileDecisions(productionShape()).Body
	if !strings.Contains(body, "8570 rows in total") {
		t.Errorf("the page must state the chain total beside its own count; body:\n%s", body)
	}
	if !strings.Contains(body, "rows this digest does not name") {
		t.Error("it must say the remainder is uncounted rather than implying full coverage")
	}
}

// TestEmptyLedgerSaysSoAboutTheLedgerNotTheEstate — the honest-empty rule.
func TestEmptyLedgerSaysSoAboutTheLedgerNotTheEstate(t *testing.T) {
	body := CompileDecisions(DecisionInputs{}).Body
	if !strings.Contains(body, "statement about the ledger, not about the estate") {
		t.Error("an empty ledger means nothing has been GOVERNED yet — not that nothing has happened")
	}
}

// TestDecisionsDigestIsDeterministic — ties on Total must not reorder between compiles.
func TestDecisionsDigestIsDeterministic(t *testing.T) {
	in := DecisionInputs{LedgerTotal: 30, Tallies: []DecisionTally{
		dt("zebra", 10, 0, 30), dt("alpha", 10, 0, 30), dt("mid", 10, 5, 30),
	}}
	first := CompileDecisions(in).Body
	for i := 0; i < 25; i++ {
		if CompileDecisions(in).Body != first {
			t.Fatal("digest is not deterministic — equal totals must break on the decision name")
		}
	}
}
