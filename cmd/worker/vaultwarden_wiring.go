package main

// wireVaultwarden registers the homelab-tier `vw:` SecretRef scheme (spec/024 T-024-6) — OUT of main()
// per the TG-501 ratchet. Substrate OFF by default (TG_VAULTWARDEN_ADDR unset) ⇒ vw: references fail
// closed at the config registry and nothing else changes; configured-but-unbuildable is FATAL, because
// a declared vw: secret must never degrade to a plaintext fallback. Its client owns its own transport,
// so — exactly like the OpenBao delivery client (TG-415) — it is handed this process's egress meter
// explicitly rather than silently bypassing it.

import (
	"log"
	"net/http"

	"github.com/territory-grounder/grounder/core/preflight"
	"github.com/territory-grounder/grounder/modules/credsource/vaultwarden"
)

// reportSecretTiers logs the deployment's secret-backend posture by tier (spec/024 T-024-8, REQ-2408):
// which backends are configured, what irreducible on-host credential each RELOCATES secret-zero to, what
// none of them provide, and which tier this process is actually running. It decides nothing — every
// reference still resolves through the scheme it names — and it offers no plaintext tier, by
// construction. Reads only presence, never a value, so the line is safe to log verbatim (INV-13).
func reportSecretTiers(getenv func(string, string) string, sealedStore bool) {
	log.Print(preflight.TierReport(preflight.Availability{
		BaoAddr:        getenv("TG_OPENBAO_ADDR", ""),
		BaoCert:        getenv("TG_OPENBAO_CERT", ""),
		BaoWrapToken:   getenv("TG_OPENBAO_WRAP_TOKEN_REF", ""),
		BaoJWT:         getenv("TG_OPENBAO_JWT_REF", ""),
		VaultwardenURL: getenv("TG_VAULTWARDEN_ADDR", ""),
		PassboltURL:    getenv("TG_PASSBOLT_ADDR", ""),
		SealedStore:    sealedStore,
	}))
}

func wireVaultwarden(getenv func(string, string) string) {
	var wrap func(http.RoundTripper) http.RoundTripper
	if egressMeter != nil {
		wrap = egressMeter.Wrap
	}
	if err := vaultwarden.WireDelivery(
		getenv("TG_VAULTWARDEN_ADDR", ""),
		getenv("TG_VAULTWARDEN_EMAIL_REF", ""),
		getenv("TG_VAULTWARDEN_PASSWORD_REF", ""),
		log.Printf, wrap,
	); err != nil {
		log.Fatalf("vaultwarden delivery: %v — a declared vw: secret must never fall back to plaintext", err)
	}
}
