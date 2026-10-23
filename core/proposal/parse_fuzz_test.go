package proposal

import (
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/fuzzcorpus"
)

// FuzzParseProposal hammers the MODEL-OUTPUT boundary — the untrusted-input path where a model tool-call
// becomes a typed, actuatable Proposal (TG-5 Phase 4, "enumerate every parser path"; INV-06). The
// predecessor's actual bypass lived in an EXPORTED alternate grammar its tests never drove; the founding
// lesson is that a test is only as strong as the code path it drives. parse_test.go's TestNoSecondGrammarAccepts
// checks NAMED examples; this generalizes to ARBITRARY bytes, driving the real ParseProposal with three
// properties, every one a fail-closed guarantee downstream code relies on:
//
//  1. ParseProposal NEVER PANICS on any bytes. A panic on the parse path is a denial of service delivered by
//     a single malformed model reply.
//  2. Every REJECTION is a KNOWN fail-closed sentinel (ErrUnparseable / ErrIncompleteProposal /
//     ErrConfidenceRange), so the caller can ALWAYS branch to POLL_PAUSE (fail closed, INV-06). An
//     un-classified error would be an unhandled path that slips past the fail-closed default.
//  3. Every ACCEPT is WELL-FORMED: all four required fields present and confidence in [0,1]. A malformed
//     ACCEPT is worse than a REJECT — the prediction gate, the ledger, and the content-hashed manifest
//     downstream all then trust it as an actuatable proposal.
//
// A REJECT is always a safe outcome (fail-closed by design). The fuzzer fails ONLY on a panic, an
// un-sentineled error, or a malformed accept, so it runs in CI over the seed corpus and drives wide with
// `go test -fuzz=FuzzParseProposal ./core/proposal`.
func FuzzParseProposal(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"external_ref":"TG-1","target":"dc1web01","op_class":"restart-service","op":"restart","confidence":0.82,"approval_choice":"AUTO-RESOLVE"}`),
		[]byte(``),
		[]byte(`not json at all`),
		[]byte(`{"external_ref":"TG-1"}`), // missing required fields
		[]byte(`{"external_ref":"e","target":"t","op_class":"c","op":"o","confidence":1.5}`),                                // confidence above 1
		[]byte(`{"external_ref":"e","target":"t","op_class":"c","op":"o","confidence":-0.1}`),                               // confidence below 0
		[]byte(`{"external_ref":"e","target":"t","op_class":"c","op":"o","evil":"rm -rf /"}`),                               // unknown field (a smuggled second grammar)
		[]byte(`{"external_ref":"e","target":"t","op_class":"c","op":"o"}{"external_ref":"e2"}`),                            // trailing second object
		[]byte("Here is my plan.\n\n[AUTO-RESOLVE] restart nginx"),                                                          // markdown sentinel (the predecessor grammar)
		[]byte("{\"external_ref\":\"e\x00\",\"target\":\"t\n\",\"op_class\":\"c\",\"op\":\"o\",\"confidence\":0.5}"),        // control bytes (NUL, newline) in fields
		[]byte(`{"external_ref":"réf-ünïcode-💥","target":"t","op_class":"c","op":"o","confidence":0.5}`),                    // multibyte fields
		[]byte(`[{"external_ref":"e","target":"t","op_class":"c","op":"o"}]`),                                               // array wrapper, not an object
		[]byte(`{"external_ref":"e","target":"t","op_class":"c","op":"o","confidence":"high"}`),                             // wrong type for confidence
		[]byte(`{"external_ref":"e","target":"t","op_class":"c","op":"o","confidence":0.5,"diagnosis":{"root_cause":"x"}}`), // the one optional nested field
	}
	for _, s := range seeds {
		f.Add(s)
	}
	for _, hostile := range fuzzcorpus.Strings() {
		f.Add([]byte(hostile)) // the shared §3.2 battery, mapped onto this boundary's []byte input
	}

	f.Fuzz(func(t *testing.T, resp []byte) {
		p, err := ParseProposal(resp) // property 1: must never panic, whatever the bytes
		if err != nil {
			// property 2: every rejection routes to a KNOWN fail-closed sentinel (INV-06). If this fires, some
			// input produces an error class the caller does not know to treat as POLL_PAUSE.
			if !errors.Is(err, ErrUnparseable) && !errors.Is(err, ErrIncompleteProposal) && !errors.Is(err, ErrConfidenceRange) {
				t.Fatalf("rejection is not a known fail-closed sentinel (caller cannot branch to POLL_PAUSE): %v\ninput: %q", err, resp)
			}
			return
		}
		// property 3: an ACCEPT is well-formed — the invariants ParseProposal exists to enforce hold, so no
		// malformed proposal reaches the gate / ledger / manifest.
		if p.ExternalRef == "" || p.Action.Target == "" || p.Action.OpClass == "" || p.Action.Op == "" {
			t.Fatalf("accepted a proposal with an empty REQUIRED field (external_ref/target/op_class/op): %+v\ninput: %q", p, resp)
		}
		if p.Confidence < 0 || p.Confidence > 1 {
			t.Fatalf("accepted a proposal with confidence outside [0,1]: %v\ninput: %q", p.Confidence, resp)
		}
	})
}
