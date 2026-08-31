package agent

// TG-472 — the base-prompt byte-identity golden (epic TG-114 leaf 2, externalization half). Captured over
// renderPreamble's FIXED inputs before the embed swap; the swap had to reproduce every byte, and the
// TG-472 eval-gate waiver rested on that identity (TG-394 precedent). Killing mutation: edit one byte of
// any agent/prompts/*.md — its combo hash reddens.
//
// RECAPTURED 2026-08-14 for TG-49: the batched-dispatch grammar line added +506 bytes to the protocol
// part, so every combo below moved by exactly that prose (a DELIBERATE, REACHABLE preamble change — no
// waiver; the on-box eval change gate runs on it before merge). The goldens' job is unchanged: they pin
// the assembly so the NEXT drift is loud.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/execclass"
)

var preambleGoldens = map[string]string{
	"sentinel+fallback": "4e3fec92e2cd289a8c64b340fc4f310e0d3cea56254b412400357b6bf5611968",
	"sentinel+ops":      "22cc7bacdf662578629fd11ec9bccc1907f734b1da5f195e35dfd3082f1b6d74",
	"tools+fallback":    "ac723ac73f4d65eaa5bffec2076d35701da5c1290728f05ea3bab78f47ec54f2",
	"tools+ops":         "0ed18e0e4028e98f43baebeda9fdae991cb8fa39bde8abc528363a224b4ece5b",
}

func psum(b string) string { h := sha256.Sum256([]byte(b)); return hex.EncodeToString(h[:]) }

func preambleCombos() map[string]string {
	const fakeTools = "- probe-tool: a fixture tool\n    host (string, required): target"
	const sentinel = "none — you cannot gather evidence; propose only if the alert itself is sufficient, else stop"
	fallback := emptyOpClassCatalog()
	const fakeOps = "- restart-service: params unit (string, required)"
	return map[string]string{
		"sentinel+fallback": renderPreamble(sentinel, fallback, ""),
		"sentinel+ops":      renderPreamble(sentinel, fakeOps, ""),
		"tools+fallback":    renderPreamble(fakeTools, fallback, ""),
		"tools+ops":         renderPreamble(fakeTools, fakeOps, ""),
	}
}

func TestPreambleGoldenGenerate(t *testing.T) {
	if len(preambleGoldens) != 0 {
		return
	}
	for k, v := range preambleCombos() {
		fmt.Printf("\t%q: %q,\n", k, psum(v))
	}
	t.Fatal("preambleGoldens empty — paste the printed map")
}

func TestProtocolPreambleGolden(t *testing.T) {
	if len(preambleGoldens) == 0 {
		t.Fatal("preambleGoldens empty")
	}
	for k, v := range preambleCombos() {
		if got, want := psum(v), preambleGoldens[k]; got != want {
			t.Errorf("renderPreamble(%s) drifted from the golden (the byte-identity the eval waiver rests on)", k)
		}
	}
}

// The embed loader fails CLOSED (mirrors agent/skills/seedBody): a part the renderer names but the embed
// lacks panics rather than rendering a thinner protocol.
func TestPromptPartFailsClosedOnMissingPart(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("promptPart on a missing part must panic (fail closed)")
		}
	}()
	_ = promptPart("no-such-part")
}

// --- TG-215: per-execution-class preamble goldens ---
//
// GOLDEN-FIRST (TG-215): "main" was first captured on main BEFORE the progressive-disclosure change
// existed (2026-08-14, commit c543d265), over the SAME disclosure fixture, as
// sha256(renderPreamble(Catalog(), classGoldenOps)), and every REACHABLE class (DEEP_INVESTIGATION,
// HUMAN_LED, STANDARD_AGENT) plus both conservative fallbacks (the empty class of an unthreaded caller, a
// garbage class) had to keep reproducing those exact bytes — the proof TG-215 was non-behavioral for them.
// Only FAST_AGENT — correlator-unreachable until the classifier wiring lands (TG-42's caveat) — carries
// the reduced render, pinned by its own golden.
//
// RECAPTURED 2026-08-14 for TG-49 (the batched-dispatch grammar line, +506 protocol-prose bytes — a
// DELIBERATE, REACHABLE preamble change for every class at once; no waiver, the on-box eval change gate
// runs on it before merge): "main" is now 8245 preamble bytes (was 7739) and "fast" 8150 (was 7644); the
// tool-catalog halves are untouched (1546 flat / 1451 fast over this deliberately SMALL fixture — the
// index note costs a fixed ~320 bytes, so the fixture understates the disclosure win). The goldens'
// STRUCTURAL claims are unchanged: every reachable class + fallback renders ONE identical preamble, and
// FAST_AGENT's render stays pinned and strictly smaller. The live-catalog delta is measured over the REAL
// tool families in cmd/worker/tool_disclosure_delta_test.go, whose log records the live reduction.
var classGoldens = map[string]string{
	"main": "b3e23514cea668563d9d127f782249fdad89d2a736d61a3b768ff635918f01e5",
	"fast": "a879caf51ce35168baa6522b435f3c1de62d9b268aaa6299d8b240634a8e3340",
}

// classGoldenOps mirrors preambleCombos' fixed op-class catalog: the per-class goldens pin the TOOL
// disclosure, so the other slot stays a constant.
const classGoldenOps = "- restart-service: params unit (string, required)"

func classCombo(class execclass.Class) string {
	return renderPreamble(toolListFor(disclosureFixtureSet(), class), classGoldenOps, "")
}

func TestPerClassPreambleGoldenGenerate(t *testing.T) {
	if len(classGoldens) != 0 {
		return
	}
	fast := classCombo(execclass.FastAgent)
	deep := classCombo(execclass.DeepInvestigation)
	fmt.Printf("\t\"main\": %q,\n\t\"fast\": %q,\n", psum(deep), psum(fast))
	fmt.Printf("deep_bytes=%d fast_bytes=%d\n", len(deep), len(fast))
	t.Fatal("classGoldens empty — paste the printed map")
}

// TestPerClassPreambleGolden: the reachable classes and both conservative fallbacks reproduce main's
// pre-TG-215 bytes exactly; FAST_AGENT reproduces its pinned reduced render, and that render is
// actually smaller (the recorded delta cannot silently evaporate).
func TestPerClassPreambleGolden(t *testing.T) {
	if len(classGoldens) == 0 {
		t.Fatal("classGoldens empty")
	}
	for name, class := range map[string]execclass.Class{
		"DEEP_INVESTIGATION": execclass.DeepInvestigation,
		"HUMAN_LED":          execclass.HumanLed,
		"STANDARD_AGENT":     execclass.StandardAgent,
		"unthreaded-empty":   "",
		"garbage":            execclass.Class("NOT_A_CLASS"),
	} {
		if got, want := psum(classCombo(class)), classGoldens["main"]; got != want {
			t.Errorf("preamble for %s drifted from the pinned main golden — a reachable-class preamble "+
				"changed, which is exactly what the byte-identity gate exists to catch: recapture ONLY as a "+
				"deliberate, eval-gated preamble change (got %s)", name, got)
		}
	}
	fast := classCombo(execclass.FastAgent)
	if got, want := psum(fast), classGoldens["fast"]; got != want {
		t.Errorf("FAST_AGENT preamble drifted from its golden (got %s)", got)
	}
	if deep := classCombo(execclass.DeepInvestigation); len(fast) >= len(deep) {
		t.Errorf("FAST_AGENT preamble (%d bytes) is not smaller than the deep preamble (%d bytes) — the "+
			"disclosure reduction this ticket ships has evaporated", len(fast), len(deep))
	}
}

// C-3b: a non-empty override IS the guidance half — the store-composed body replaces the embed verbatim,
// and the protocol half is untouched. (The empty-override byte-identity is pinned by the goldens above.)
// Killing mutation executed 2026-08-15: renderPreamble ignoring the override → this red.
func TestRenderPreambleComposesTheOverride(t *testing.T) {
	override := "STORE GUIDANCE vNEXT — trial arm body"
	got := renderPreamble("TOOLS", "OPS", override)
	if !strings.HasSuffix(got, "\n"+override) {
		t.Fatalf("the override must be the guidance half verbatim, got tail %q", got[max(0, len(got)-80):])
	}
	if strings.Contains(got, promptPart("base-prompt-guidance")) {
		t.Fatal("the embedded guidance must NOT also render when an override is present (double guidance)")
	}
	if !strings.Contains(got, "TOOLS") || !strings.Contains(got, "OPS") {
		t.Fatal("the protocol half must render unchanged around the override")
	}
}
