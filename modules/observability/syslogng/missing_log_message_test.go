package syslogng

import (
	"os"
	"strings"
	"testing"
)

// TG-363, the caller half. The guard can now distinguish "this file is absent" (44) from "TG built a
// request I refuse" (42) — but that only matters if the sentence handed to the AGENT distinguishes them.
//
// It did not. Every non-zero read produced one line: "the device may not log there, or that day has no
// file". So a TG-side defect would have reached the model, permanently and silently, as an observation
// about the estate — and the model would then have cited "this host does not log here" as grounded fact,
// with a citation ID to back it.

// KILLING MUTATION: return the same string for every exit code (the pre-fix state). RED.
func TestAnEstateFactAndATGDefectReadDifferently(t *testing.T) {
	absent := missingLogMessage("", GuardNoSuchLogExit, "dc1mealie01", "dc1syslogng01", "2026-08-06")
	refused := missingLogMessage("", GuardRefusedExit, "dc1mealie01", "dc1syslogng01", "2026-08-06")

	if absent == refused {
		t.Fatal("a missing log and a refused request produce the SAME sentence — the agent cannot tell an " +
			"estate fact from a TG bug, which is the whole defect")
	}
	// The absent case must read as a fact the agent may ground on.
	if !strings.Contains(absent, "does not ship logs") {
		t.Errorf("the no-such-log answer does not state the estate fact plainly: %q", absent)
	}
	if strings.Contains(strings.ToLower(absent), "refused") {
		t.Errorf("the no-such-log answer mentions a refusal, muddying a clean fact: %q", absent)
	}
	// The refused case must read as TG's own defect and must actively warn the agent off the wrong
	// conclusion — a neutral "the read failed" is what let this pass as an estate fact for so long.
	if !strings.Contains(refused, "TG-side defect") {
		t.Errorf("the refusal answer does not name it as a TG defect: %q", refused)
	}
	if !strings.Contains(refused, "do not conclude the host has no logs") {
		t.Errorf("the refusal answer does not warn the agent off the estate conclusion: %q", refused)
	}
}

// ROLLOUT SAFETY. An older guard on a not-yet-updated syslog host still returns 42 for a missing file. The
// refusal text must therefore admit that ambiguity rather than assert a TG bug — otherwise deploying the
// caller before the guard turns every missing log into a false "TG defect" report.
//
// KILLING MUTATION: drop the parenthetical about older guards. RED.
func TestTheRefusalAdmitsTheOlderGuardAmbiguity(t *testing.T) {
	refused := missingLogMessage("", GuardRefusedExit, "h", "s", "2026-08-06")
	if !strings.Contains(refused, "older guard") {
		t.Errorf("the refusal answer does not acknowledge that an older guard returns this for a missing "+
			"file too — deploying the caller first would then report every absent log as a TG defect: %q", refused)
	}
}

// Any OTHER non-zero (tail's own exit 1 on an unreadable file, an ssh-level status) keeps the honest hedge.
// Inventing a third confident claim for a status nobody has characterised would be the same mistake again.
func TestAnUncharacterisedStatusKeepsTheHedge(t *testing.T) {
	other := missingLogMessage("", 1, "h", "s", "2026-08-06")
	if !strings.Contains(other, "may not log there") {
		t.Errorf("an uncharacterised non-zero status lost the hedge: %q", other)
	}
	if strings.Contains(other, "TG-side defect") || strings.Contains(other, "does not ship logs") {
		t.Errorf("an uncharacterised status was reported as one of the two known causes: %q", other)
	}
}

// The message must name the host, the server and the date in every branch — an answer the agent cannot
// attribute is not evidence, and the search and tail lanes must read the same way.
func TestEveryBranchIsAttributable(t *testing.T) {
	for _, code := range []int{GuardNoSuchLogExit, GuardRefusedExit, 1} {
		for _, verb := range []string{"", "to search"} {
			got := missingLogMessage(verb, code, "dc1mealie01", "dc1syslogng01", "2026-08-06")
			for _, want := range []string{"dc1mealie01", "dc1syslogng01", "2026-08-06"} {
				if !strings.Contains(got, want) {
					t.Errorf("exit %d verb %q: message omits %q — %q", code, verb, want, got)
				}
			}
		}
	}
}

// The path must never appear in an agent-facing answer: the guard deliberately withholds it (writing a
// caller-chosen string into the log host's own answer is how a caller learns the tree's shape), and the
// caller must not reconstruct what the guard refused to say.
func TestTheMessageLeaksNoPath(t *testing.T) {
	for _, code := range []int{GuardNoSuchLogExit, GuardRefusedExit, 1} {
		got := missingLogMessage("", code, "h", "s", "2026-08-06")
		if strings.Contains(got, "/mnt/") || strings.Contains(got, ".log") {
			t.Errorf("exit %d: the agent-facing message leaks a filesystem path: %q", code, got)
		}
	}
}

// GUARDING missingLogMessage IS NOT GUARDING THE READ PATH.
//
// Every test above calls it directly, and all of them stayed green against the mutation that matters:
// putting the old literal sentence back at the two call sites. The function would be perfect and the agent
// would still receive one answer for two causes. Eighth time in this project.
//
// The read path needs a live SSH runner, so the oracle is a scoped source assertion — comment lines
// stripped, because a guard of this shape has passed on its own commented-out subject before.
func TestBothReadLanesUseTheDisambiguatedMessage(t *testing.T) {
	b, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatalf("read tools.go: %v", err)
	}
	src := stripLineComments(string(b))

	// The literal the two lanes used to share. Its return means the disambiguation was bypassed.
	const oldLiteral = "the device may not log there, or that day has no file"
	if n := strings.Count(src, oldLiteral); n != 0 {
		t.Errorf("the pre-fix sentence appears %d time(s) OUTSIDE missingLogMessage — a read lane is "+
			"answering with one string for both an estate fact and a TG defect", n)
	}
	// Both lanes must route through the helper: the tail lane and the grep lane.
	if n := strings.Count(src, "missingLogMessage("); n < 3 { // 1 definition + 2 call sites
		t.Errorf("missingLogMessage appears %d time(s); expected its definition plus BOTH read lanes. "+
			"A lane that does not call it reports the old undifferentiated answer.", n)
	}
	// And the exit code must actually be passed — a call site that hardcodes a status disambiguates nothing.
	if !strings.Contains(src, "missingLogMessage(\"\", rr.ExitCode,") ||
		!strings.Contains(src, "missingLogMessage(\"to search\", rr.ExitCode,") {
		t.Error("a read lane calls missingLogMessage without passing rr.ExitCode, so it cannot tell the two " +
			"causes apart no matter what the helper does")
	}
}

// stripLineComments drops whole-line // comments so the assertions above cannot match the prose that
// explains them — including the pre-fix sentence quoted in this file's own commentary.
func stripLineComments(s string) string {
	var out strings.Builder
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		out.WriteString(l)
		out.WriteByte('\n')
	}
	return out.String()
}
