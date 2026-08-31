package vaultwarden

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the Vaultwarden credential source's configuration schema so the console GENERATES
// its dialog instead of a hand-written form that drifts from the binary.
//
// THE ADDRESS IS EffectRestart, and for the same reason the OpenBao descriptor states: cmd/worker's
// wireCredentialDelivery reads these three keys ONCE at boot and captures them in the client; the
// unlocked (enc, mac) pair is then cached in that client for the process's life. A dialog claiming
// "live" here would report a success it did not achieve. The two REFERENCE fields are EffectReadOnly
// because a reference is provenance, not a control: the operator sets the secret in the backend the
// reference names, never the pointer through this dialog.
//
// NO SECRET VALUE FIELD, and unlike OpenBao's the reason is not circularity — it is TIER HONESTY. This
// module's own credential is a MASTER PASSWORD that unlocks an entire vault, so the console must not
// offer to store it as one more module secret alongside a connector API token: it is the root of trust
// for every vw: reference in the process. It is declared as a REFERENCE (TypeSecretRef) and resolves
// through whatever backend that reference names — an operator running the machine-plane substrate should
// point it at bao:, which is exactly the migration the tiered selector recommends.
//
// THE ADDRESS IS AUTHORITY, not an ordinary endpoint: every vw: reference in the process resolves
// against whatever this names, so pointing it elsewhere changes what TG trusts to hand it credentials.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "credsource",
		SourceType: SourceType,
		Title:      "Vaultwarden / Bitwarden (homelab tier)",
		Summary: "Homelab-tier secret backend: dereferences vw:<item>#<field> SecretRefs from a Vaultwarden " +
			"or Bitwarden vault, decrypting natively in Go. Read-only — it resolves secrets, never writes them. " +
			"Second-tier assurance: the vault unlocks from a master password, not a machine identity.",
		Fields: []desc.Field{
			{
				Name: "addr", EnvKey: "TG_VAULTWARDEN_ADDR", Label: "Vault address",
				Help: "Base URL of the Vaultwarden/Bitwarden server, e.g. https://vault.example.net. Every " +
					"vw: reference in this process resolves against whatever this names, so it is the root of " +
					"trust for those secrets rather than one connector's endpoint. Empty means the substrate " +
					"is OFF: the vw: scheme is never registered and every vw: reference fails closed — no " +
					"other scheme is affected.",
				Type: desc.TypeURL, Security: desc.SecAuthority, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				Name: "email_ref", EnvKey: "TG_VAULTWARDEN_EMAIL_REF", Label: "Account email reference",
				Help: "A REFERENCE (env:/file:/bao:/store:) to the vault account's email — the PBKDF2 salt as " +
					"well as the login identity, so a changed email changes the derived keys. Never an inline " +
					"literal (INV-13).",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "password_ref", EnvKey: "TG_VAULTWARDEN_PASSWORD_REF", Label: "Master password reference",
				Help: "A REFERENCE to the vault account's MASTER PASSWORD. It unlocks the whole vault, so it is " +
					"declared as a reference and never stored here as a value: point it at bao: once the " +
					"machine-plane substrate is running, which is the migration that raises this backend out " +
					"of the homelab tier. The password itself never reaches the server — only a derived login " +
					"hash does.",
				Type: desc.TypeSecretRef, Security: desc.SecSecret, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
		},
	}
}
