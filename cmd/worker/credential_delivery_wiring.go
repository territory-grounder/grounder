package main

// wireCredentialDelivery is the composition root's SECRET-SUBSTRATE bootstrap, carved out of main() per
// the TG-501 ratchet: it registers the schemes this process's own SecretRefs resolve through, BEFORE any
// secret resolves. Two backends, one gate:
//
//   - OpenBao (machine plane, spec/022 REQ-2200/2204): mTLS machine identity > AppRole > bootstrap token,
//     in that precedence, with the rationale for the ordering preserved inline below.
//   - Vaultwarden (homelab tier, spec/024 T-024-6): the vw: scheme, OFF unless TG_VAULTWARDEN_ADDR is set.
//
// Both are fail-closed: a configured-but-unbuildable backend is FATAL rather than a silent fall back to a
// plaintext scheme. Behaviour is byte-identical to the in-main() version this replaces.

import (
	"log"

	"github.com/territory-grounder/grounder/modules/credsource/openbao"
)

func wireCredentialDelivery(getenv func(string, string) string) {
	// The mTLS machine-identity bootstrap (spec/024 Amendment 2026-07-25, T-024-10): where a FreeIPA-CA
	// client cert+key are configured, the worker authenticates to OpenBao by PRESENTING that identity —
	// no bootstrap token on disk. Preferred; the token stays as a transition fallback until retired.
	baoAddr, baoCA := getenv("TG_OPENBAO_ADDR", ""), getenv("TG_OPENBAO_CA", "")
	baoCert, baoKey := getenv("TG_OPENBAO_CERT", ""), getenv("TG_OPENBAO_KEY", "")
	// APPROLE is selected here and not only in modules/bootstrap (TG-153). It is the only bootstrap that
	// gives ONE HOST TWO IDENTITIES: both worker containers present the same host certificate, so mTLS
	// cannot separate the triage plane from the actuation plane — it hands them the same policy and
	// collapses the split back into the blast radius it exists to break.
	//
	// It ranks BELOW mTLS (a certificate is an identity the host IS; a secret-id is a secret it HOLDS), so
	// the cert branch still wins when both are configured. It ranks ABOVE the token because a role-scoped
	// credential beats a bearer token that carries whatever policy it was minted with.
	baoRoleID, baoSecretID := getenv("TG_OPENBAO_ROLE_ID_REF", ""), getenv("TG_OPENBAO_SECRET_ID_REF", "")
	var delErr error
	switch {
	case baoCert != "" || baoKey != "":
		delErr = openbao.WireDeliveryCert(baoAddr, baoCert, baoKey, getenv("TG_OPENBAO_CERT_ROLE", ""), baoCA, log.Printf, meteredBaoTransport()...)
	case baoRoleID != "" || baoSecretID != "":
		delErr = openbao.WireDeliveryAppRole(baoAddr, baoRoleID, baoSecretID, baoCA, log.Printf, meteredBaoTransport()...)
	default:
		delErr = openbao.WireDelivery(baoAddr, getenv("TG_OPENBAO_TOKEN_REF", ""), baoCA, log.Printf, meteredBaoTransport()...)
	}
	if delErr != nil {
		log.Fatalf("credential delivery: %v", delErr)
	} // spec/024 T-024-6: the homelab vw: scheme rides the same gate (OFF unless TG_VAULTWARDEN_ADDR is set)
	wireVaultwarden(getenv)
	wirePassbolt(getenv)
	// The posture report (T-024-8): after both backends are registered, say which tier this deployment
	// actually runs. sealedStore is false here — the store: resolver is wired later, once the DB pool
	// exists — and the report says "not configured" rather than claiming a tier this line cannot see.
	reportSecretTiers(getenv, false)
}
