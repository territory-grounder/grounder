package trace

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// a valid 64-hex-char sha256 content address for fixtures
var testHash256 = "sha256:" + strings.Repeat("3c96deadbeefa0df", 4)

// REQ-2017: the generalizable projection carries NONE of the estate-specific layer. A trace
// loaded with hosts, a ticket id, a rule id, a target, a free-text reason, and a credential ref
// projects to a layer in which none of those strings appear — de-identification by TYPE.
func TestProjectDropsEstateData(t *testing.T) {
	tr := SessionTrace{
		ExternalRef: "TG-9999-secret-ticket",
		Host:        "dc1edge03.estate.internal",
		AlertRule:   "BGPSessionDown_edge03",
		ActionID:    "act-abc123",
		PlanHash:    "plan-deadbeef",
		Band:        "AUTO_NOTICE",
		Verdict:     "clean",
		Confidence:  0.91,
		Steps: []Step{{
			Kind:          StepPropose,
			Reason:        "restart bgpd on dc1edge03 to clear the flap",
			Rule:          "BGPSessionDown_edge03",
			CredentialRef: "ssh",
			PlanOps:       []PlanOp{{Op: "change", T: "dc1edge03:bgpd"}},
		}},
	}
	got := ProjectGeneralizable(tr, GeneralizableClasses{
		OpClass:           "restart-service",
		AlertClass:        "service-down/http",
		Reversible:        true,
		BlastClass:        "single-host",
		Artifacts:         []ArtifactRef{{Kind: "runbook", Ref: testHash256}},
		KnownOpClasses:    []string{"restart-service"},
		KnownAlertClasses: []string{"service-down/http"},
		KnownBlastClasses: []string{"single-host"},
	})

	blob, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(blob)
	for _, leak := range []string{
		"dc1", "edge03", "TG-9999", "act-abc123", "plan-deadbeef",
		"bgpd", "estate.internal", "BGPSessionDown", "ssh",
	} {
		if strings.Contains(s, leak) {
			t.Errorf("estate identifier %q leaked into the generalizable layer: %s", leak, s)
		}
	}
	// The generalizable fields ARE preserved.
	if got.OpClass != "restart-service" || got.AlertClass != "service-down/http" || got.BlastClass != "single-host" {
		t.Errorf("classes dropped: op=%q alert=%q blast=%q", got.OpClass, got.AlertClass, got.BlastClass)
	}
	if got.Verdict != "clean" || got.Band != "AUTO_NOTICE" || got.Confidence != 0.91 {
		t.Errorf("generalizable governance fields dropped: %+v", got)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0].Ref != testHash256 {
		t.Errorf("graduated-artifact ref dropped: %+v", got.Artifacts)
	}
}

// The critical regression guard: EVERY class field — including BlastClass — is folded against
// its allowlist, so an estate-influenced or mis-supplied class string cannot pass through. (A
// prior version copied BlastClass unfolded, a de-identification bypass.)
func TestClassesFoldedNotPassedThrough(t *testing.T) {
	leaky := "dc1edge03.estate.internal is down, ticket TG-9999-secret"
	got := ProjectGeneralizable(SessionTrace{Band: "AUTO", Verdict: "clean"}, GeneralizableClasses{
		AlertClass:        leaky,
		OpClass:           leaky,
		BlastClass:        leaky,
		KnownAlertClasses: []string{"service-down/http"},
		KnownOpClasses:    []string{"restart-service"},
		KnownBlastClasses: []string{"single-host"},
	})
	if got.AlertClass != ClassOther || got.OpClass != ClassOther || got.BlastClass != ClassOther {
		t.Fatalf("an estate-influenced class was not folded to %q: %+v", ClassOther, got)
	}
	blob, _ := json.Marshal(got)
	for _, leak := range []string{"dc1", "edge03", "TG-9999", "estate.internal"} {
		if strings.Contains(string(blob), leak) {
			t.Errorf("estate identifier %q passed through a class field: %s", leak, blob)
		}
	}
}

// A mis-supplied or estate-influenced class string cannot pass through: the fold contains it to
// "other" — even (especially) when the caller forgot the allowlist.
func TestFoldClassContainment(t *testing.T) {
	known := []string{"service-down/http", "device-down"}
	if got := foldClass("service-down/http", known); got != "service-down/http" {
		t.Errorf("known class must pass, got %q", got)
	}
	if got := foldClass("dc1edge03-is-down", known); got != ClassOther {
		t.Errorf("unknown (estate-shaped) class must fold to %q, got %q", ClassOther, got)
	}
	if got := foldClass("", known); got != ClassUnset {
		t.Errorf("empty must be %q, got %q", ClassUnset, got)
	}
	if got := foldClass("service-down/http", nil); got != ClassOther {
		t.Errorf("empty allowlist must fold to %q (containment over fidelity), got %q", ClassOther, got)
	}
}

// An artifact ref that is a URL, a path, a wrong-length hex, or a hex-encoded estate string —
// anything but a genuine fixed-width content hash — is dropped, so an estate reference cannot
// ride out disguised as an artifact ref; unknown kinds fold to "other".
func TestSanitizeArtifactsDropsNonHash(t *testing.T) {
	hexURL := "sha256:" + hex.EncodeToString([]byte("https://console.estate.internal/host/dc1edge03")) // wrong length, substring hidden by hex
	in := []ArtifactRef{
		{Kind: "runbook", Ref: testHash256},                             // valid sha256
		{Kind: "runbook", Ref: "https://console.estate.internal/rb/42"}, // URL
		{Kind: "runbook", Ref: "/etc/estate/secret-runbook"},            // path
		{Kind: "evil-kind", Ref: "sha512:" + strings.Repeat("ab", 64)},  // valid sha512, unknown kind
		{Kind: "skill", Ref: "sha256:tooShort"},                         // wrong length
		{Kind: "skill", Ref: hexURL},                                    // hex-encoded URL — wrong length
	}
	out := sanitizeArtifacts(in)
	if len(out) != 2 {
		t.Fatalf("want 2 valid hash refs kept, got %d: %+v", len(out), out)
	}
	if out[0].Ref != testHash256 || out[0].Kind != "runbook" {
		t.Errorf("valid hash ref altered/dropped: %+v", out[0])
	}
	if out[1].Kind != ClassOther {
		t.Errorf("unknown artifact kind must fold to %q, got %q", ClassOther, out[1].Kind)
	}
	blob, _ := json.Marshal(out)
	if strings.Contains(string(blob), "estate") {
		t.Errorf("an estate URL/path leaked through as an artifact ref: %s", blob)
	}
}

// isContentAddress accepts a fixed-width hex digest per algo and rejects URLs, paths, wrong
// lengths, non-hex bodies, and unknown algos.
func TestIsContentAddress(t *testing.T) {
	for _, ok := range []string{
		"sha256:" + strings.Repeat("a", 64),
		"sha384:" + strings.Repeat("0", 96),
		"sha512:" + strings.Repeat("F", 128),
	} {
		if !isContentAddress(ok) {
			t.Errorf("%q should be a content address", ok)
		}
	}
	for _, bad := range []string{
		"https://x/y", "/path/to/file", "sha256:", "abc123",
		"md5:" + strings.Repeat("a", 32),  // unknown algo
		"sha256:" + strings.Repeat("a", 63), // too short
		"sha256:" + strings.Repeat("a", 65), // too long
		"sha256:" + strings.Repeat("z", 64), // non-hex
	} {
		if isContentAddress(bad) {
			t.Errorf("%q should NOT be a content address", bad)
		}
	}
}
