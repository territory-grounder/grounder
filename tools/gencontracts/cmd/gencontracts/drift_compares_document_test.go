package main

import (
	"os"
	"strings"
	"testing"

	gc "github.com/territory-grounder/grounder/tools/gencontracts"
)

// The drift gate compared a HASH, and the hash is not the document.
//
// Model.SourceHash() covers routes and entities only — never the emitter's own output. So any change to
// what Generate WRITES (the securitySchemes block, the scaffolding, a description) left the hash
// identical, `-check` printed "no drift", and the committed artifact silently stopped matching the
// generator.
//
// Measured while adding the tgMTLS scheme (TG-249): the generated document gained two lines and the gate
// still reported `no drift — source f4585054ae3d`. The published contract was missing a security scheme
// the generator emits, and nothing said so. Same shape as the defect that ticket was about — a gate that
// verifies something narrower than it appears to.

func TestNormaliseDropsOnlyTheTimestamp(t *testing.T) {
	doc := strings.Join([]string{
		"openapi: 3.0.3",
		"    generated_at: \"2026-08-05T02:27:40Z\"",
		"    source_hash: \"abc123\"",
		"    tgMTLS:",
		"      type: mutualTLS",
	}, "\n")

	got := normaliseContract(doc)
	if strings.Contains(got, "generated_at") {
		t.Error("generated_at survived normalisation — it differs on every run, so the comparison would " +
			"report drift constantly and the gate would be turned off")
	}
	for _, must := range []string{"openapi: 3.0.3", "source_hash", "tgMTLS", "mutualTLS"} {
		if !strings.Contains(got, must) {
			t.Errorf("normalisation removed %q. It must drop generated_at and NOTHING else — a normaliser "+
				"that strips more hides the very drift this gate exists to find.", must)
		}
	}
}

// TestAnEmitterOnlyChangeIsDetected is the defect itself: two documents whose route/entity model is
// identical, differing only in what the emitter wrote.
func TestAnEmitterOnlyChangeIsDetected(t *testing.T) {
	model, err := gc.BuildModel()
	if err != nil {
		t.Fatalf("build model: %v", err)
	}
	fresh := gc.Generate(model, "").OpenAPI
	if len(fresh) < 2000 {
		t.Fatalf("VACUITY FLOOR: the generated contract is only %d bytes — every comparison below would "+
			"pass on a stub", len(fresh))
	}

	// An emitter-only change: drop the mTLS scheme block, exactly the shape that slipped through.
	stale := strings.Replace(fresh, "    tgMTLS:\n      type: mutualTLS\n", "", 1)
	if stale == fresh {
		t.Fatal("could not construct an emitter-only difference — the tgMTLS block is not in the generated " +
			"document, so this oracle is testing nothing. Re-anchor it on a block that is emitted.")
	}

	if normaliseContract(stale) == normaliseContract(fresh) {
		t.Error("a document missing an entire security scheme compares EQUAL to the generated one. The " +
			"gate would report `no drift` while the published contract omits a scheme the generator " +
			"emits — which is exactly what happened.")
	}

	// And the model hash is blind to it, which is why the body comparison is necessary rather than
	// belt-and-braces. Asserting this pins the REASON the gate needs both checks.
	if !strings.Contains(stale, model.SourceHash()) || !strings.Contains(fresh, model.SourceHash()) {
		t.Fatalf("both documents should still carry the same source hash %s — if they do not, this test is "+
			"not demonstrating an emitter-ONLY change", model.SourceHash()[:12])
	}
}

// TestTimestampAloneIsNotDrift is the false-positive control. If the gate fired on generated_at, every
// CI run would fail and the check would be disabled within a day — the failure mode that quietly removes
// a control is worse than the one it catches.
func TestTimestampAloneIsNotDrift(t *testing.T) {
	model, err := gc.BuildModel()
	if err != nil {
		t.Fatalf("build model: %v", err)
	}
	a := gc.Generate(model, "2026-08-05T02:27:40Z").OpenAPI
	b := gc.Generate(model, "2026-08-06T19:00:00Z").OpenAPI
	if a == b {
		t.Fatal("two generations with different timestamps produced identical documents — generated_at is " +
			"not being emitted, so this control proves nothing")
	}
	if normaliseContract(a) != normaliseContract(b) {
		t.Error("two generations differing ONLY in generated_at compare as drift. The gate would fail on " +
			"every run and be switched off.")
	}
}

// TestTheCheckPathItselfDetectsEmitterDrift closes the gap the first round of mutations exposed:
// deleting the body comparison from main() left every test above GREEN, because they exercised
// normaliseContract and not the decision. This drives the decision function itself.
func TestTheCheckPathItselfDetectsEmitterDrift(t *testing.T) {
	model, err := gc.BuildModel()
	if err != nil {
		t.Fatalf("build model: %v", err)
	}
	fresh := gc.Generate(model, "2026-08-05T02:27:40Z").OpenAPI

	if err := documentMatchesGenerator(fresh, model); err != nil {
		t.Fatalf("a freshly generated document was reported as drift: %v", err)
	}

	stale := strings.Replace(fresh, "    tgMTLS:\n      type: mutualTLS\n", "", 1)
	if stale == fresh {
		t.Fatal("could not construct emitter-only drift — re-anchor this oracle on a block that is emitted")
	}
	if err := documentMatchesGenerator(stale, model); err == nil {
		t.Error("the check path accepted a committed document missing an entire security scheme. This is " +
			"the defect: the gate reported `no drift` while the published contract omitted a scheme the " +
			"generator emits.")
	}
}

// stripComments removes Go line/block comments so the wiring assertion below cannot be satisfied by
// prose. A guard in this repo has passed on its own comment before.
func stripComments(src string) string {
	var out []string
	inBlock := false
	for _, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case inBlock:
			if strings.Contains(t, "*/") {
				inBlock = false
			}
			continue
		case strings.HasPrefix(t, "/*"):
			if !strings.Contains(t, "*/") {
				inBlock = true
			}
			continue
		case strings.HasPrefix(t, "//"):
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestStripCommentsActuallyStrips(t *testing.T) {
	got := stripComments("// documentMatchesGenerator(x)\nreal()\n/* documentMatchesGenerator(y) */\n")
	if strings.Contains(got, "documentMatchesGenerator") {
		t.Fatalf("the stripper left commented-out code, so the wiring assertion can be satisfied by a "+
			"comment. got %q", got)
	}
	if !strings.Contains(got, "real()") {
		t.Fatalf("the stripper removed real code: %q", got)
	}
}

// TestTheCheckPathIsActuallyWired is the composition-root guard. Gutting the CALL in main() left every
// behavioural test above green — the decision function was still correct and still tested, and simply
// no longer consulted. That is the resolver-guarded/wiring-unguarded shape, and it is the reason the
// original defect shipped: correct code that nothing ran.
func TestTheCheckPathIsActuallyWired(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := stripComments(string(raw))

	i := strings.Index(src, "if *check {")
	if i < 0 {
		t.Fatal("VACUITY FLOOR: no `if *check {` block in main.go — this guard is anchored on a shape that " +
			"no longer exists and would pass while checking nothing")
	}
	j := strings.Index(src[i:], "\n\tart :=")
	if j < 0 {
		j = len(src) - i
	}
	block := src[i : i+j]

	if !strings.Contains(block, "documentMatchesGenerator(") {
		t.Fatal("the -check block does not call documentMatchesGenerator. The gate would compare only the " +
			"route/entity source hash again — which is blind to any change in what the emitter WRITES, and " +
			"is exactly how a published contract came to be missing a security scheme while CI said `no drift`.")
	}
	// It must ACT on the result, and the assertion is SCOPED to the call. Checking the whole block for
	// "fatal(" passed while the error was discarded, because the block already fatals on other things
	// (unreadable file, missing provenance) — a Contains satisfied by an unrelated occurrence, which is
	// how a guard of mine survived its own killing mutation before.
	k := strings.Index(block, "documentMatchesGenerator(")
	window := block[k:]
	if len(window) > 200 {
		window = window[:200]
	}
	if !strings.Contains(window, "fatal(") {
		t.Fatalf("the -check block calls documentMatchesGenerator but does not fatal() on its verdict "+
			"within the same statement. A drift verdict that is computed and discarded is not a gate.\n"+
			"window:\n%s", window)
	}
}
