package main

import (
	"os"
	"strings"
	"testing"
)

// ★ A CORRECT POSTURE MUST NOT BE REPORTED AS A FAULT (TG-353).
//
// The triage worker logged, every boot: "SSH-session evidence is ARMED but TG's own SSH identity did NOT
// resolve — every heal TG performs over SSH will read attributed-suspicious. This is the one combination
// that actively misreports; fix the key ref."
//
// Measured 2026-08-06, that fired on exactly the configuration TG-153 mandates:
//
//	worker (triage):  TG_JOURNAL_SSH_SESSIONS=1   TG_ACTUATION_SSH_KEY=EMPTY   <- the split working
//	worker-actuate:   TG_JOURNAL_SSH_SESSIONS=    TG_ACTUATION_SSH_KEY=resolves
//
// The triage plane performs no heals, so the misreporting condition cannot arise there — and "fix the key
// ref" means giving the triage plane an actuation credential, which ValidateFor refuses at boot. The
// warning's remedy undid the control it was reporting against.
//
// This reads the composition root because the defect was never in a function: it was in which branch the
// wiring took. Three separate times this week a resolver was guarded while its call site was not.
func TestTheMisreportWarningIsGatedOnHoldingActuation(t *testing.T) {
	src := stripAttrComments(readAttrMain(t))

	i := strings.Index(src, "actively misreports")
	if i < 0 {
		t.Fatal("the actively-misreports warning is gone entirely. It is CORRECT on the actuation plane — " +
			"a heal really would be attributed to an unknown actor — so deleting it trades a false positive " +
			"for a false negative on the plane that matters")
	}
	// Walk back to the nearest enclosing condition and require it to be the plane predicate.
	head := src[:i]
	if !strings.Contains(head[max(0, len(head)-600):], "credentialPlane.HoldsActuation()") {
		t.Error("the actively-misreports warning is not gated on credentialPlane.HoldsActuation(). It then " +
			"fires on a triage worker, which performs no heals, and tells the operator to add an actuation " +
			"key ref that the plane split refuses at boot.")
	}
}

// The other branch must exist and must NOT tell the operator to add the key ref.
func TestTheTriagePlaneBranchDoesNotAdviseAddingTheKey(t *testing.T) {
	src := stripAttrComments(readAttrMain(t))
	if !strings.Contains(src, "EXPECTED on this plane") {
		t.Fatal("no triage-plane branch: a worker whose self-actor is withheld by design must still say so, " +
			"or the operator is left to infer silence")
	}
	j := strings.Index(src, "EXPECTED on this plane")
	branch := src[j:min(len(src), j+700)]
	if !strings.Contains(branch, "Do NOT add the key ref") {
		t.Error("the triage-plane message does not warn against adding the key ref. That is the remedy an " +
			"operator reaches for, and it undoes the credential plane split.")
	}
	if strings.Contains(branch, "actively misreports") {
		t.Error("the triage-plane branch still claims misreporting — the condition it names cannot occur on " +
			"a plane that performs no heals")
	}
	// AND IT MUST NOT OFFER THE REMEDY AT ALL. Keeping "Do NOT add the key ref" while the sentence goes on
	// to say "fix the key ref" reads as contradictory advice and an operator follows the actionable half —
	// which is the half that hands an actuation credential to the triage plane. That mutation was executed
	// and survived until this assertion existed.
	if strings.Contains(branch, "fix the key ref") {
		t.Error("the triage-plane branch tells the operator to fix the key ref. On this plane that means " +
			"adding an actuation credential, which ValidateFor refuses at boot — the remedy undoes the " +
			"control. Name the self-actor declaration instead.")
	}
}

func TestTheAttributionWarningGuardIgnoresProse(t *testing.T) {
	prose := "// credentialPlane.HoldsActuation() actively misreports\nfunc main() {}\n"
	if got := stripAttrComments(prose); strings.Contains(got, "actively misreports") {
		t.Fatalf("stripAttrComments left commented-out text in place; got %q", got)
	}
}

func readAttrMain(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("main.go is empty — the assertions would be vacuous")
	}
	return string(b)
}

func stripAttrComments(src string) string {
	var b strings.Builder
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
