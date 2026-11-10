package ansible

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the un-controlled Ansible credential source's configuration schema so the console
// GENERATES its dialog rather than a hand-written form that drifts from the binary.
//
// THE VAULT PASSWORD IS GENUINELY EffectLive, and it is the only field here that is. Resolver.decrypt
// (ansible.go:821) calls Resolve() on the password reference on EVERY decrypt — nothing is cached — so a
// password saved to the lane below takes effect on the next ansible-vault dereference with no restart. The
// path fields are read once at boot (cmd/worker/main.go:1006-1010) and captured in the Tree and Source at
// construction, so a save there is durable but inert until the worker restarts; the dialog must say so
// rather than implying success.
//
// This is the PLAIN-FILES case: no AWX or Semaphore controller, just an inventory tree TG parses and inline
// !vault secrets it decrypts natively at use time. Every credential it emits is a SecretRef, never a
// literal.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "credsource",
		SourceType: SourceType,
		Title:      "Ansible (files + vault)",
		Summary: "Machine-plane credential source for an un-controlled Ansible tree: parses inventory, " +
			"group_vars and host_vars into host identities and decrypts inline ansible-vault values at use " +
			"time. Read-only — nothing under the root is ever written.",
		Fields: []desc.Field{
			{
				Name: "root", EnvKey: "TG_ANSIBLE_ROOT", Label: "Ansible tree root",
				Help: "Directory holding the inventory plus group_vars/host_vars. Empty means this source is " +
					"never registered and the tree contributes no host identities. Set it with no vault " +
					"password reference and the boot fails closed — an ansible-vault: reference that could " +
					"never decrypt is worse than no source.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				Name: "inventory_path", EnvKey: "TG_ANSIBLE_INVENTORY", Label: "Inventory file",
				Help: "Optional path pinning the inventory file; a relative path is resolved under the root. " +
					"Empty searches the conventional names in a fixed order and takes the first that " +
					"exists — which quietly picks a different file if someone adds one earlier in that " +
					"order, so pin it when the tree has more than one.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				Name: "vault_pass_ref", EnvKey: "TG_ANSIBLE_VAULT_PASS_REF", Label: "Vault-password reference",
				Help: "Where the ansible-vault password is read from. Displayed for provenance: set the " +
					"password itself below, not this pointer.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "default_user", EnvKey: "TG_ANSIBLE_DEFAULT_USER", Label: "Default login user",
				Help: "Login user for hosts that declare no ansible_user (default \"root\"). Wrong here and " +
					"every such host authenticates as the wrong account.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 64,
			},
			{
				// LIVE, and provably so: ansible.go:821 resolves this reference inside decrypt(), per call.
				Name: "vault_password", Label: "Ansible-vault password",
				Help: "The password that decrypts the tree's !vault values. Write-only: stored in the secret " +
					"backend and never read back into this dialog. Re-read on every decrypt, so a save takes " +
					"effect immediately — no restart. A wrong password fails the message authentication and " +
					"yields an error, never a garbage credential.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: desc.Validate refuses a module that names its own secret path.
		// TG_ANSIBLE_VAULT_PASS_REF must point here — once — after which every rotation is a Save.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("credsource", SourceType), Field: "password"},
		Test: desc.TestSpec{
			// The verb now promises the DECRYPT, because without it the button would be a directory listing
			// wearing a test's name: parsing the inventory passes with a wrong vault password, which is the
			// one credential this dialog writes and the one whose failure is otherwise invisible until a
			// governed action tries to escalate. The verb also says the plaintext is discarded, since an
			// operator is entitled to know a real secret is decrypted in memory when they press it.
			Verb: "parse the inventory under the configured root, report how many hosts it declares, and " +
				"decrypt ONE inline ansible-vault value with the saved password to prove it is right — the " +
				"plaintext is discarded and never displayed (read-only; nothing under the root is written)",
			Mutating: false,
		},
	}
}
