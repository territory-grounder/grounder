package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// collapseSpace normalizes runs of whitespace to a single space. gofmt ALIGNS struct fields, so
// `Rearm: x` becomes `Rearm:  x` the moment a longer key joins the literal — a guard that matched the
// exact spacing would go red on a formatting pass and teach the next person to loosen it.
func collapseSpace(s string) string { return regexp.MustCompile(`[ \t]+`).ReplaceAllString(s, " ") }

// GUARDING THE MONITOR IS NOT GUARDING THE WIRING.
//
// core/governance's oracles prove JudgeLivenessMonitor releases the halt when the judge measures alive.
// Every one of them stayed green against the mutation that matters here: commenting out
// `Rearm: judgeDeadMan` at the composition root. The monitor would then run forever with a nil Rearmer —
// which is exactly the state the fix is for, since JudgeDeadMan.Rearm calls itself "the ONLY path back"
// and nothing outside a test had ever called it.
//
// This reads main.go with comment lines STRIPPED, because a guard of this shape has previously passed on
// its own commented-out subject.

// judgeLivenessWiringBlock returns the JudgeLivenessMonitor composite-literal body from main.go.
func judgeLivenessWiringBlock(t *testing.T, src string) string {
	t.Helper()
	const marker = "JudgeLivenessMonitor{"
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("main.go constructs no coregov.JudgeLivenessMonitor at all — the judge-liveness monitor " +
			"is not wired, so neither the halt nor its release can happen")
	}
	rest := src[i+len(marker):]
	depth := 1
	for j, r := range rest {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[:j]
			}
		}
	}
	t.Fatal("the JudgeLivenessMonitor literal in main.go is unbalanced — cannot scope this assertion")
	return ""
}

// KILLING MUTATION: comment out (or delete) `Rearm: judgeDeadMan` in main.go. RED.
//
// That mutation was executed against the core/governance suite and SURVIVED — every behavioural oracle
// there passed while the production monitor held a nil Rearmer.
func TestTheJudgeLivenessMonitorIsWiredToRelease(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	stripped := stripGoComments(string(src))
	block := judgeLivenessWiringBlock(t, stripped)

	if !strings.Contains(block, "Halt:") {
		t.Errorf("the judge-liveness monitor is constructed with no Halt — a confirmed dead judge would stop "+
			"nothing (TG-222). Wiring block:\n%s", block)
	}
	if !strings.Contains(block, "Rearm:") {
		t.Errorf("the judge-liveness monitor is constructed with no Rearm, so its halt has NO reachable "+
			"recovery: JudgeDeadMan.Rearm is the only path back and nothing else in the tree calls it. "+
			"Measured live 2026-08-06 — the dead-man was OPEN and the skill-store flywheel had graduated "+
			"nothing since 2026-07-31 while drafts and trials kept being produced. Wiring block:\n%s", block)
	}
	// The release must be bound to the SAME dead-man the halt trips, or the two halves govern different
	// state and a release closes a breaker nobody opened.
	if !strings.Contains(collapseSpace(block), "Rearm: judgeDeadMan") {
		t.Errorf("Rearm is not bound to judgeDeadMan — the halt and its release must act on one breaker. "+
			"Wiring block:\n%s", block)
	}
}

// NEGATIVE CONTROL for the block walker. A scoper that silently returned nothing would report every field
// missing and could never fail for the right reason; one that returned the whole file would report every
// field present and could never fail at all.
func TestJudgeLivenessWiringBlockScoperWorks(t *testing.T) {
	got := judgeLivenessWiringBlock(t, "x := &coregov.JudgeLivenessMonitor{\n\tRearm:  judgeDeadMan,\n\tInner: X{Y: 1},\n}\nAFTER_THE_LITERAL")
	if !strings.Contains(collapseSpace(got), "Rearm: judgeDeadMan") {
		t.Errorf("the scoper dropped a field that is inside the literal: %q", got)
	}
	if !strings.Contains(got, "Inner: X{Y: 1}") {
		t.Errorf("the scoper stopped at the first nested closing brace: %q", got)
	}
	if strings.Contains(got, "AFTER_THE_LITERAL") {
		t.Errorf("the scoper ran past the end of the literal, so the assertions are file-wide, not scoped: %q", got)
	}
	if strings.Contains(judgeLivenessWiringBlock(t, "&coregov.JudgeLivenessMonitor{\n\tHalt: h,\n}\nRearm: judgeDeadMan"), "Rearm") {
		t.Error("a Rearm OUTSIDE the literal satisfied the scoped read — the assertion is not scoped")
	}
}
