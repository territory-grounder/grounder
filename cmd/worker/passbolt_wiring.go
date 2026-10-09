package main

// wirePassbolt registers the homelab-tier `passbolt:` SecretRef scheme (spec/024 T-024-5) — OUT of
// main() per the TG-501 ratchet, and called from wireCredentialDelivery beside its two siblings.
// Substrate OFF by default (TG_PASSBOLT_ADDR unset) ⇒ passbolt: references fail closed and nothing else
// changes; configured-but-unbuildable is FATAL, because a declared passbolt: secret must never degrade
// to a plaintext fallback.

import (
	"log"

	"github.com/territory-grounder/grounder/modules/credsource/passbolt"
)

func wirePassbolt(getenv func(string, string) string) {
	if err := passbolt.WireDelivery(
		getenv("TG_PASSBOLT_ADDR", ""),
		getenv("TG_PASSBOLT_KEY_REF", ""),
		getenv("TG_PASSBOLT_PASSPHRASE_REF", ""),
		log.Printf,
	); err != nil {
		log.Fatalf("passbolt delivery: %v — a declared passbolt: secret must never fall back to plaintext", err)
	}
}
