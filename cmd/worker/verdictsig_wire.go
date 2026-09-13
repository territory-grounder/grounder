package main

// verdictsig_wire.go — the TG-81 b3 verdict-provenance wiring, OUT of main() (the TG-501 ratchet: new
// features land in wire functions, not back into the 6,000-line builder).

import (
	"log"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/verdictsig"
)

// wireVerdictSigning arms Ed25519 provenance on the action-verdict WRITER when the seed ref is
// configured (TG_VERDICT_SIGNING_SEED_REF — secret, actuation-plane). Unset ⇒ dormant, rows unsigned,
// byte-identical pre-b3 shape. Set-but-unresolvable REFUSES the boot rather than booting an unsigned
// writer that looks armed.
func wireVerdictSigning(vstore *db.VerdictStore) *db.VerdictStore {
	seedRef := strings.TrimSpace(getenv("TG_VERDICT_SIGNING_SEED_REF", ""))
	if seedRef == "" {
		log.Print("verdict signing: dormant (TG_VERDICT_SIGNING_SEED_REF unset) — rows unsigned, pre-b3 shape")
		return vstore
	}
	seed, err := config.SecretRef(seedRef).Resolve()
	if err != nil {
		log.Fatalf("verdict signing: seed ref set but unresolvable — refusing to boot an unsigned writer that looks armed: %v", err)
	}
	signer, err := verdictsig.NewSigner(seed)
	if err != nil {
		log.Fatalf("verdict signing: %v", err)
	}
	log.Printf("verdict signing: ARMED — action_verdict rows carry Ed25519 provenance (public key %s)", signer.PublicKeyHex())
	return vstore.WithSigner(signer.Sign)
}

// wireVerdictVerification arms read-side signature checking on the prior-verdict reader when the PUBLIC
// key is configured (TG_VERDICT_PUBLIC_KEY — hex, not a secret; pairs with the signing seed). A signed
// row that fails verification is dropped from the classifier's prior-verdict evidence; unsigned rows
// stay pre-feature history. Unset ⇒ byte-identical reads.
func wireVerdictVerification(pv *db.PriorVerdictStore) *db.PriorVerdictStore {
	pub := strings.TrimSpace(getenv("TG_VERDICT_PUBLIC_KEY", ""))
	if pub == "" {
		log.Print("verdict verification: dormant (TG_VERDICT_PUBLIC_KEY unset) — all rows accepted, pre-b3 shape")
		return pv
	}
	verifier, err := verdictsig.NewVerifier(pub)
	if err != nil {
		log.Fatalf("verdict verification: TG_VERDICT_PUBLIC_KEY invalid: %v", err)
	}
	log.Print("verdict verification: ARMED — signed action_verdict rows must verify to count as prior-verdict evidence")
	return pv.WithVerifier(verifier.Verify)
}
