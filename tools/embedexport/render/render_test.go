package render

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
)

// A ratified overlay spec: argv-template data (never a compiled builder), a real family, an auto-eligible
// tier, and a non-destructive verb — i.e. exactly the shape that can legitimately climb to the ceiling.
func earnedSpec() opschema.OpClassSpec {
	return opschema.OpClassSpec{
		OpClass:    "reload-proxy",
		Op:         "reload",
		Family:     opschema.FamilyServiceLifecycle,
		SafetyTier: opschema.TierLowReversible,
		Params: []opschema.ParamSpec{
			{Name: "unit", Required: true},
		},
		ArgvTemplate: []string{"systemctl", "reload", "{unit}"},
	}
}

func mustJSON(t *testing.T, s opschema.OpClassSpec) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// O-2809 (the export half). REQ-2808/REQ-2818: the embed-export artifact is the ONLY road to the silent AUTO
// rung, so it must carry BOTH halves of the obligation — the snippet a reviewer pastes, and the spec/013
// restamp checklist that keeps the paste legal. An artifact with only the snippet would send a reviewer to a
// red pipeline; one with only the checklist would send them nowhere.
func TestMRBodyCarriesTheSnippetAndTheFullRestampObligation(t *testing.T) {
	out, err := Render(mustJSON(t, earnedSpec()), "mr")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// The snippet half: the exact JSON a reviewer pastes, in a fenced block, naming the target file.
	if !strings.Contains(out, "core/actuate/opschema/opschema.json") {
		t.Fatalf("the artifact must name the file the snippet is pasted into:\n%s", out)
	}
	if !strings.Contains(out, "```json") || !strings.Contains(out, `"op_class": "reload-proxy"`) {
		t.Fatalf("the artifact must carry the pasteable snippet:\n%s", out)
	}
	// The obligation half. Each of these is a step whose omission reds CI or defeats a governance gate;
	// the artifact is worthless as a review aid if it lets a reviewer discover them one pipeline at a time.
	for _, must := range []string{
		"lockstep --restamp",     // spec/013 is governed — the paste alone reds the build
		"Law-Change-Approved-By", // the protected-path gate
		"CONTIGUOUS",             // the blank-line trailer trap that silently voids the approval
		"opcover",                // REQ-2818: an AUTO-capable class with no fault pairing
		"Revoke the overlay row", // the tidy-up, with its honest "harmless but untidy" consequence
	} {
		if !strings.Contains(out, must) {
			t.Fatalf("the restamp checklist omits %q — a reviewer would meet it as a red pipeline instead:\n%s", must, out)
		}
	}
	// The artifact must be honest about what merging does NOT do: it does not grant autonomy. The ladder
	// still has to promote the class on its own evidence.
	if !strings.Contains(out, "Nothing here grants autonomy directly") {
		t.Fatalf("the artifact must state that merging does not itself grant autonomy:\n%s", out)
	}
}

// REQ-2808: the tool must refuse a spec the EMBEDDED registry would refuse. This is the one attack the
// tool's existence opens — hand-craft its input and let a plausible-looking diff carry an inadmissible spec
// into the strongest tamper domain. Re-running admission here closes it before a human ever reads the diff.
func TestASpecTheRegistryWouldRefuseIsNeverRenderedAsASnippet(t *testing.T) {
	bad := earnedSpec()
	bad.Family = "invented-family" // outside the CLOSED family set — a new ladder nobody watches
	if _, err := Render(mustJSON(t, bad), "mr"); err == nil {
		t.Fatal("a spec with an unknown family must be REFUSED, not rendered into a pasteable snippet")
	} else if !strings.Contains(err.Error(), "REFUSED by the embedded registry") {
		t.Fatalf("the refusal must say WHY the snippet is unsafe to paste, got: %v", err)
	}
}

// An already-embedded slug means the export is a no-op whose snippet would create a DUPLICATE key and break
// registry init. Refused loudly rather than emitted.
func TestAnAlreadyEmbeddedClassIsRefusedRatherThanDuplicated(t *testing.T) {
	specs := opschema.Specs()
	if len(specs) == 0 {
		t.Skip("no embedded specs in this build")
	}
	dup := earnedSpec()
	dup.OpClass = specs[0].OpClass
	_, err := Render(mustJSON(t, dup), "mr")
	if err == nil {
		t.Fatalf("exporting the already-embedded class %q must be refused (a duplicate key breaks init)", dup.OpClass)
	}
	if !strings.Contains(err.Error(), "ALREADY embedded") {
		t.Fatalf("the refusal must name the duplicate-key hazard, got: %v", err)
	}
}

// The snippet must be valid JSON that round-trips back into the SAME spec — a snippet that renders prettily
// but decodes differently would paste a subtly different capability than the one that was earned.
func TestTheSnippetRoundTripsIntoTheIdenticalSpec(t *testing.T) {
	want := earnedSpec()
	snip, err := Render(mustJSON(t, want), "snippet")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var got opschema.OpClassSpec
	if err := json.Unmarshal([]byte(snip), &got); err != nil {
		t.Fatalf("the snippet is not valid JSON — it cannot be pasted: %v\n%s", err, snip)
	}
	if got.OpClass != want.OpClass || got.Family != want.Family || got.SafetyTier != want.SafetyTier ||
		strings.Join(got.ArgvTemplate, " ") != strings.Join(want.ArgvTemplate, " ") {
		t.Fatalf("the snippet decodes to a DIFFERENT capability than the one earned:\nwant %+v\ngot  %+v", want, got)
	}
}
