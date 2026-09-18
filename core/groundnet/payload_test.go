package groundnet

import (
	"testing"
)

// REQ-2102: the set of understood media types gates the payload, not the envelope.
func TestKnownPayloadType(t *testing.T) {
	for _, mt := range []string{WisdomMediaTypeV1, ConfirmationMediaTypeV1, DeviationMediaTypeV1, WithdrawalMediaTypeV1} {
		if !KnownPayloadType(mt) {
			t.Errorf("KnownPayloadType(%q) = false, want true", mt)
		}
	}
	for _, mt := range []string{"application/vnd.groundnet.future+json", "text/plain", ""} {
		if KnownPayloadType(mt) {
			t.Errorf("KnownPayloadType(%q) = true, want false", mt)
		}
	}
}

// INV-5: the outcome verifier must be mechanical; an LLM-adjudicated outcome is rejected.
func TestVerifierMustBeMechanical(t *testing.T) {
	w := testWisdom()
	w.Outcome.Verifier = "llm"
	if err := w.Validate(); err == nil {
		t.Fatal("a non-mechanical verifier must be rejected (INV-5)")
	}
}

// The verdict must be one of the closed vocabulary.
func TestVerdictVocabulary(t *testing.T) {
	w := testWisdom()
	w.Outcome.Verdict = "maybe"
	if err := w.Validate(); err == nil {
		t.Fatal("an out-of-vocabulary verdict must be rejected")
	}
}

// A payload missing its generalizable classes is rejected.
func TestWisdomRequiresClasses(t *testing.T) {
	w := testWisdom()
	w.OpClass = ""
	if err := w.Validate(); err == nil {
		t.Fatal("a payload with no op_class must be rejected")
	}
}

// REQ-2115 foundation: content-addressing is stable and content-sensitive — identical
// content yields an identical sub; any change yields a different sub.
func TestComputeSubjectStable(t *testing.T) {
	payload, _ := testWisdom().Marshal()
	a := ComputeSubject(WisdomMediaTypeV1, payload)
	b := ComputeSubject(WisdomMediaTypeV1, payload)
	if a != b {
		t.Errorf("ComputeSubject not deterministic: %q != %q", a, b)
	}
	if a == ComputeSubject(WisdomMediaTypeV1, append(payload, ' ')) {
		t.Errorf("ComputeSubject collided on different content")
	}
	if a == ComputeSubject(ConfirmationMediaTypeV1, payload) {
		t.Errorf("ComputeSubject collided across media types")
	}
	if len(a) <= len("sha256:") || a[:7] != "sha256:" {
		t.Errorf("ComputeSubject = %q, want sha256: prefix", a)
	}
}
