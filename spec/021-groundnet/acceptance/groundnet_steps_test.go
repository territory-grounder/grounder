package acceptance

import (
	"fmt"
	"strings"

	"github.com/cucumber/godog"

	gn "github.com/territory-grounder/grounder/core/groundnet"
)

// Statement-layer acceptance bindings (T-021-1 / T-021-2): the SCITT Transparent Statement envelope, its
// payload-versioning, the pseudonymous Issuer, and signature-verify-before-ingest (REQ-2100/2102/2103/2104).
// These drive the REAL core/groundnet exported API — sign, parse, ValidateEnvelope, DecodeWisdom, Verify,
// and the reputation rollup — so each scenario passes strictly, not as a pending stub. The remaining
// scenarios stay @pending until their owning tasks' bindings land.
func init() {
	stepRegistrars = append(stepRegistrars, registerStatementLayerSteps)
}

const gnTestSeed = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"

// gnSign signs a payload for a fixed test pseudonym and attaches a Receipt (a Transparent Statement).
func gnSign(contentType string, payload []byte) (*gn.Statement, error) {
	p, err := gn.NewPseudonym(gnTestSeed)
	if err != nil {
		return nil, err
	}
	stmt, err := p.Sign(gn.StatementHeader{Subject: gn.ComputeSubject(contentType, payload), IssuedAt: 1, ContentType: contentType}, payload)
	if err != nil {
		return nil, err
	}
	stmt.AttachReceipt([]byte("acceptance-test-receipt"))
	return stmt, nil
}

func gnWisdomPayload() []byte {
	w := gn.WisdomV0{AlertClass: "service-down/http", OpClass: "restart-service", Outcome: gn.WisdomOutcome{Verifier: gn.VerifierMechanical, Verdict: gn.VerdictClean}}
	b, _ := w.Marshal()
	return b
}

type statementWorld struct {
	stmt        *gn.Statement
	envErr      error
	decodeErr   error
	tamperWire  []byte
	goodVerify  error
	tamperError error
}

func registerStatementLayerSteps(sc *godog.ScenarioContext) {
	w := &statementWorld{}

	// REQ-2100 — the envelope is a versioned signed unit with the stable field set.
	sc.Step(`^a wisdom unit that is a SCITT Transparent Statement carrying the protected-header claims iss sub iat kid and content_type a payload and a Transparency Service Receipt$`, func() error {
		stmt, err := gnSign(gn.WisdomMediaTypeV1, gnWisdomPayload())
		if err != nil {
			return err
		}
		w.stmt = stmt
		return nil
	})
	sc.Step(`^a node parses and validates the SCITT envelope$`, func() error {
		wire, err := w.stmt.MarshalCBOR()
		if err != nil {
			return err
		}
		parsed, err := gn.ParseStatement(wire)
		if err != nil {
			return err
		}
		w.stmt = parsed
		w.envErr = parsed.ValidateEnvelope()
		return nil
	})
	sc.Step(`^the envelope is the canonical groundnet SCITT profile and the node validates it independently of the payload it carries$`, func() error {
		if w.envErr != nil {
			return fmt.Errorf("the envelope must validate independently of the payload: %v", w.envErr)
		}
		if _, ok := w.stmt.Receipt(); !ok {
			return fmt.Errorf("a Transparent Statement must carry a Receipt")
		}
		h, err := w.stmt.Header()
		if err != nil {
			return err
		}
		if !strings.HasPrefix(h.Issuer, gn.PseudonymScheme) || h.Subject == "" || len(h.KeyID) == 0 || h.ContentType == "" {
			return fmt.Errorf("the SCITT protected-header claims (iss/sub/kid/content_type) must all be present: %+v", h)
		}
		return nil
	})

	// REQ-2102 — the payload is versioned and evolvable while the envelope stays stable.
	sc.Step(`^a consumer that understands a set of payload media types and a statement carrying a newer content_type$`, func() error {
		stmt, err := gnSign("application/vnd.groundnet.future+json", []byte{0x00, 0x9f, 0xff})
		if err != nil {
			return err
		}
		w.stmt = stmt
		return nil
	})
	sc.Step(`^the consumer reads the statement$`, func() error {
		w.envErr = w.stmt.ValidateEnvelope()
		_, w.decodeErr = gn.DecodeWisdom(w.stmt)
		return nil
	})
	sc.Step(`^the SCITT envelope stays stable across payload versions and the consumer rejects the unknown payload media type without rejecting the envelope$`, func() error {
		if w.envErr != nil {
			return fmt.Errorf("the envelope must stay valid across payload versions: %v", w.envErr)
		}
		if w.decodeErr == nil {
			return fmt.Errorf("an unknown payload media type must be rejected")
		}
		if gn.KnownPayloadType("application/vnd.groundnet.future+json") {
			return fmt.Errorf("the future media type must be unknown to this node")
		}
		return nil
	})

	// REQ-2103 — the producer attestation is a stable pseudonym; reputation accrues to it, not an estate.
	sc.Step(`^a statement whose SCITT Issuer is a stable pseudonym the iss bound by kid with no x5t or x5chain$`, func() error {
		stmt, err := gnSign(gn.WisdomMediaTypeV1, gnWisdomPayload())
		if err != nil {
			return err
		}
		w.stmt = stmt
		return w.stmt.ValidateEnvelope() // ValidateEnvelope refuses any x5t/x5chain identity binding
	})
	sc.Step(`^reputation is attributed for the statement$`, func() error { return nil })
	sc.Step(`^the Issuer carries no real-world or estate identity and reputation accrues to the pseudonym rather than to any estate$`, func() error {
		h, err := w.stmt.Header()
		if err != nil {
			return err
		}
		if !strings.HasPrefix(h.Issuer, gn.PseudonymScheme) {
			return fmt.Errorf("the Issuer must be a gnpub pseudonym, not a real-world or estate identity: %q", h.Issuer)
		}
		rep := gn.NewReputation()
		rep.Observe(h.Issuer, "gnpub:another-node", gn.ConfirmationV0{Subject: h.Subject, Result: gn.VerdictClean, VerifierProfile: "mechanical"})
		if rep.Score(h.Issuer) <= 0 {
			return fmt.Errorf("reputation must accrue to the pseudonym")
		}
		return nil
	})

	// REQ-2104 — a chunk whose signature does not verify is refused before ingest.
	sc.Step(`^a COSE_Sign1 statement whose signature binds the payload to the producer pseudonym and a second statement whose signature is tampered$`, func() error {
		good, err := gnSign(gn.WisdomMediaTypeV1, gnWisdomPayload())
		if err != nil {
			return err
		}
		w.stmt = good
		wire, err := good.MarshalCBOR()
		if err != nil {
			return err
		}
		tampered := append([]byte(nil), wire...)
		tampered[len(tampered)*3/5] ^= 0xFF // flip a byte in the payload/signature region
		w.tamperWire = tampered
		return nil
	})
	sc.Step(`^a consumer verifies each statement before ingest$`, func() error {
		w.goodVerify = gn.Verify(w.stmt)
		parsed, err := gn.ParseStatement(w.tamperWire)
		if err != nil {
			w.tamperError = err // a malformed tampered statement is refused at parse
			return nil
		}
		w.tamperError = gn.Verify(parsed)
		return nil
	})
	sc.Step(`^the verifying statement proceeds and the tampered statement is refused before it reaches the local re-graduation path$`, func() error {
		if w.goodVerify != nil {
			return fmt.Errorf("the valid statement must verify: %v", w.goodVerify)
		}
		if w.tamperError == nil {
			return fmt.Errorf("the tampered statement must be refused before ingest")
		}
		return nil
	})
}
