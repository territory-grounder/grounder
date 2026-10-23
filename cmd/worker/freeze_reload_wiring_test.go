package main

import (
	"os"
	"strings"
	"testing"
)

// GUARDING FreezeGate.Replace IS NOT GUARDING THE RELOAD.
//
// core/suppression proves the gate can be swapped at runtime. None of that matters if main.go still reads
// the file once and arms the gate only when it happens to be non-empty at boot — which is what it did:
//
//	windows := freezeWindows(getenv("TG_SUPPRESSION_FREEZE_FILE", ""))
//	...
//	if len(windows) > 0 { gate.Freeze = &suppression.FreezeGate{Windows: windows} }
//
// The file is empty at boot precisely when nobody has declared a window yet — every ordinary day — so the
// gate was nil and a window declared later had nothing to bind to. Measured on the live worker 2026-08-06:
// "tier-1 gate active — 0 freeze, 0 fold(s), 0 schedule(s), 0 pattern(s), 0 rule(s)" against a wiring
// register reporting "suppression.tier1: STARVED — 162 alerts offered, 0 produced".
//
// Read with comment lines STRIPPED: a guard of this shape has passed on its own commented-out subject
// before.

// freezeWiringBlock returns the freeze-arming block from main.go, scoped so a match elsewhere in a
// 6000-line file cannot satisfy these assertions.
func freezeWiringBlock(t *testing.T, src string) string {
	t.Helper()
	const marker = "gate.Freeze = "
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatal("main.go never assigns gate.Freeze — the tier-1 chain's FIRST gate (operator-declared " +
			"maintenance/chaos freeze) is not wired at all")
	}
	// Walk back to the enclosing `if`, forward to its closing brace at the same depth. A FAILURE to find
	// the enclosing `if` must be loud: silently starting at the assignment instead would begin the walk at
	// depth 0 mid-block, so the first nested literal would close it and every assertion below would run
	// against a truncated fragment — a scoper that quietly returns the wrong span cannot fail correctly.
	start := strings.LastIndex(src[:i], "\n\t\tif ")
	if start < 0 {
		t.Fatal("could not find the `if` enclosing the gate.Freeze assignment — the scoper would otherwise " +
			"assert against a fragment starting mid-block")
	}
	rest := src[start:]
	depth, seen := 0, false
	for j, r := range rest {
		switch r {
		case '{':
			depth++
			seen = true
		case '}':
			depth--
			if seen && depth == 0 {
				return rest[:j]
			}
		}
	}
	t.Fatal("the freeze-arming block in main.go is unbalanced — cannot scope this assertion")
	return ""
}

// KILLING MUTATION: restore `if len(windows) > 0` as the arming condition, or delete the reload goroutine.
// RED.
func TestTheFreezeGateIsArmedByTheFileNotByItsBootContents(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	stripped := stripGoComments(string(src))
	block := freezeWiringBlock(t, stripped)

	if strings.Contains(block, "len(windows) > 0") {
		t.Errorf("the freeze gate is armed by the file's BOOT CONTENTS. The file is empty exactly when nobody "+
			"has declared a window yet, so the gate is nil on every ordinary day and a window declared later "+
			"can never bind. Arm on the PATH. Block:\n%s", block)
	}
	if !strings.Contains(block, "freezePath") {
		t.Errorf("the freeze gate is not armed from the configured path. Block:\n%s", block)
	}
	if !strings.Contains(block, ".Replace(") {
		t.Errorf("nothing calls FreezeGate.Replace in the arming block — the file is still read once and a "+
			"maintenance window would need a worker restart to take effect, which is itself a disruption "+
			"during maintenance. Block:\n%s", block)
	}
	if !strings.Contains(block, "freezeWindows(") {
		t.Errorf("the reload does not re-read the file through the loader, so it cannot pick up a new "+
			"declaration. Block:\n%s", block)
	}
	// The loader is what makes an unattended reload safe: it returns NOTHING for an unreadable or malformed
	// file, so a broken file re-opens the estate to triage instead of freezing it. Re-reading through
	// anything else would lose that.
	if strings.Contains(block, "os.ReadFile") {
		t.Errorf("the reload reads the file directly instead of through freezeWindows(), losing the loader's "+
			"refusals (malformed rows, inverted windows). Block:\n%s", block)
	}
}

// The whole tier-1 chain must arm when a freeze FILE is configured, even with nothing declared today —
// otherwise the reload above has nothing to reload into.
//
// KILLING MUTATION: drop `freezePath != ""` from the chain-arming condition. RED.
func TestTheChainArmsWhenAFreezeFileIsConfigured(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	stripped := stripGoComments(string(src))
	i := strings.Index(stripped, "runner.LiveSuppressGate{")
	if i < 0 {
		t.Fatal("main.go constructs no runner.LiveSuppressGate — tier-1 suppression is not wired")
	}
	// The arming condition is the `if` immediately preceding the construction.
	head := stripped[:i]
	cond := head[strings.LastIndex(head, "\n\tif "):]
	if !strings.Contains(cond, `freezePath != ""`) {
		t.Errorf("the tier-1 chain does not arm on a configured freeze file, so an empty-at-boot file leaves "+
			"the whole gate nil and the reload has nothing to reload into. Condition:\n%s", cond)
	}
}

// NEGATIVE CONTROL for the block scoper: it must be capable of both finding a field and NOT finding one
// that sits outside the block.
func TestFreezeWiringBlockScoperWorks(t *testing.T) {
	got := freezeWiringBlock(t, "func x() {\n\t\tif freezePath != \"\" {\n\t\t\tgate.Freeze = fg\n\t\t\tinner := X{Y: 1}\n\t\t}\n\t}\nAFTER_THE_BLOCK")
	if !strings.Contains(got, "gate.Freeze = fg") {
		t.Errorf("the scoper dropped the assignment it is scoped around: %q", got)
	}
	if !strings.Contains(got, "inner := X{Y: 1}") {
		t.Errorf("the scoper stopped at the first nested closing brace: %q", got)
	}
	if strings.Contains(got, "AFTER_THE_BLOCK") {
		t.Errorf("the scoper ran past the block, so the assertions are file-wide rather than scoped: %q", got)
	}
}
