package passbolt

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the Passbolt credential source's configuration schema so the console GENERATES
// its dialog instead of a hand-written form that drifts from the binary.
//
// THE ADDRESS IS EffectRestart: the composition root reads these keys once at boot and the GPGAuth
// session (and the unlocked robot key) is cached in the client for the process's life. The two REFERENCE
// fields are EffectReadOnly because a reference is provenance, not a control — the operator sets the
// secret in the backend the reference names, never the pointer through this dialog.
//
// NO SECRET VALUE FIELD, for the same tier reason the Vaultwarden descriptor gives: this module's own
// credential is an OpenPGP PRIVATE KEY that decrypts every secret the robot can see, so the console must
// not offer to store it as one more module secret. Declared as a reference; point it at bao: once the
// machine-plane substrate runs, which is the migration out of the homelab tier.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "credsource",
		SourceType: SourceType,
		Title:      "Passbolt (homelab tier)",
		Summary: "Homelab-tier secret backend: dereferences passbolt:<resource>#<field> SecretRefs over the " +
			"Passbolt API with an OpenPGP robot identity, decrypting natively in Go. Read-only. Second-tier " +
			"assurance: the robot's private key is an unscopable on-host credential.",
		Fields: []desc.Field{
			{
				Name: "addr", EnvKey: "TG_PASSBOLT_ADDR", Label: "Passbolt address",
				Help: "Base URL of the Passbolt server, e.g. https://passbolt.example.net. Every passbolt: " +
					"reference in this process resolves against whatever this names. Empty means the substrate " +
					"is OFF: the scheme is never registered and every passbolt: reference fails closed.",
				Type: desc.TypeURL, Security: desc.SecAuthority, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				Name: "private_key_ref", EnvKey: "TG_PASSBOLT_KEY_REF", Label: "Robot private-key reference",
				Help: "A REFERENCE (env:/file:/bao:/store:) to the robot's ASCII-armored OpenPGP PRIVATE key. " +
					"It both authenticates the GPGAuth handshake and decrypts every secret the robot can read, " +
					"so it is the irreducible on-host credential of this tier. Never an inline literal (INV-13).",
				Type: desc.TypeSecretRef, Security: desc.SecSecret, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "passphrase_ref", EnvKey: "TG_PASSBOLT_PASSPHRASE_REF", Label: "Key passphrase reference",
				Help: "A REFERENCE to the robot key's passphrase. An encrypted key that will not unlock with it " +
					"refuses at boot-adjacent first use rather than resolving anything.",
				Type: desc.TypeSecretRef, Security: desc.SecSecret, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
		},
	}
}
