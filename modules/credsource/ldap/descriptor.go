package ldap

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the LDAP/FreeIPA credential source's configuration schema so the console GENERATES
// its dialog rather than a hand-written form that drifts from the binary.
//
// THE BIND PASSWORD IS GENUINELY EffectLive. Source.Sync (ldap.go:223-231) calls Resolve() on the bind DN
// and bind password references at the top of EVERY sync — nothing is cached — so a password saved to the
// lane below is used by the next sync with no restart. Everything else is read once at boot
// (cmd/worker/main.go:991-998) and captured in the Source at construction, so those saves are durable but
// inert until the worker restarts and the dialog must say so.
//
// THREE FIELDS ARE AUTHORITY, NOT ORDINARY TEXT, because this source feeds the HUMAN plane: the principals
// it returns are the approver identities and the groups that carry their memberships.
//
//   - urls. THIS IS THE TRUST ROOT AND IT IS NOT AN ORDINARY ENDPOINT. Sync (ldap.go:237-240) walks the
//     replicas in fixed order and returns the FIRST that binds and searches successfully — its entries
//     become the approver set outright. So a wrong-but-reachable entry does not fail closed; it SUBSTITUTES
//     the directory that decides who can release a governed action, and hands that server the service bind
//     password on the way (Sync resolves and sends it before any answer is judged). With no CA pinned
//     (ca_cert_ref empty ⇒ system roots, ldap.go:408-419) any publicly-trusted certificate for the
//     attacker's own hostname is enough. A first draft of this descriptor called the list ordinary "because
//     an endpoint" and justified it with fail-closed behaviour the code does not have.
//   - user_base_dn / group_base_dn. These scope which subtree's accounts become principals and which groups
//     carry their eligibility — a narrower move than urls (it stays inside an already-trusted directory)
//     but still a change to WHO can approve.
//
// WHAT IS DELIBERATELY ABSENT: the console's own LDAP LOGIN keys (TG_LDAP_AUTH_ENABLED, the admin and
// operator group CNs, the bind-DN template) are read by cmd/grounder (main.go:132-138), not by this
// connector, and they decide who gets an admin session. Omitting them is an AUTHORIAL choice and NOTHING
// ENFORCES IT — desc.Validate's reserved-namespace rule covers the safety./gateway./session./operator./
// net./ingest. prefixes and would not refuse a field named auth_admin_group. Do not add them here.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "credsource",
		SourceType: SourceType,
		Title:      "LDAP / FreeIPA (approver directory)",
		Summary: "Human-plane credential source: binds read-only to the directory and syncs user and group " +
			"principals, which is where console approver identities come from.",
		Fields: []desc.Field{
			{
				// AUTHORITY. Whichever replica answers first IS the approver directory — see the header.
				Name: "urls", EnvKey: "TG_LDAP_URLS", Label: "Replica URLs",
				Help: "Ordered replica list, e.g. \"ldaps://ipa01.example, ldaps://ipa02.example\". They are " +
					"tried in this exact order and the FIRST one that binds and searches wins — its users " +
					"and groups become the approver set, and it is handed the service bind password to get " +
					"there. Adding a server you do not control, especially at the front, replaces who can " +
					"approve; only the last replica failing makes the sync fail closed. Empty means this " +
					"source is never registered and no directory principal is learned.",
				Type: desc.TypeIDList, Security: desc.SecAuthority, Effect: desc.EffectRestart,
				Required: true, Pattern: `^ldaps?://[^\s,]+$`, MaxItems: 8, MaxLen: 256,
			},
			{
				Name: "start_tls", EnvKey: "TG_LDAP_STARTTLS", Label: "Upgrade with StartTLS",
				Help: "Upgrade a plain ldap:// connection to TLS before binding. Off with a plain ldap:// URL " +
					"sends the service bind password in clear text across the estate.",
				Type: desc.TypeBool, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
			},
			{
				Name: "ca_cert_ref", EnvKey: "TG_LDAP_CA", Label: "CA certificate reference",
				Help: "Where the PEM bundle that verifies each replica's certificate is read from (normally a " +
					"file: reference). Empty uses the system roots. Certificate verification is never skipped, " +
					"so a wrong CA fails the bind instead of trusting an unverified server.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "bind_dn_ref", EnvKey: "TG_LDAP_BIND_DN", Label: "Service bind-DN reference",
				Help: "Where the read-only service account's bind DN is read from. Displayed for provenance " +
					"and set outside this dialog — the one credential this dialog writes is the password " +
					"below. Defaults to env:LDAP_BIND_DN when unset.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "bind_password_ref", EnvKey: "TG_LDAP_BIND_PW", Label: "Bind-password reference",
				Help: "Where the service bind password is read from. Displayed for provenance: set the " +
					"password itself below, not this pointer. Defaults to env:LDAP_BIND_PW when unset.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				// AUTHORITY. This DN scopes which subtree's users become approver principals.
				Name: "user_base_dn", EnvKey: "TG_LDAP_USER_BASE", Label: "User base DN",
				Help: "Subtree searched for user principals, e.g. \"cn=users,cn=accounts,dc=example,dc=net\". " +
					"This decides which accounts can become approvers, so widening it widens the trust " +
					"boundary. Empty falls back to the generic FreeIPA layout with no site suffix, which on " +
					"most estates matches nothing and yields an empty sync.",
				Type: desc.TypeText, Security: desc.SecAuthority, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				// AUTHORITY. Group membership is how approver eligibility is carried.
				Name: "group_base_dn", EnvKey: "TG_LDAP_GROUP_BASE", Label: "Group base DN",
				Help: "Subtree searched for groups, e.g. \"cn=groups,cn=accounts,dc=example,dc=net\". Group " +
					"membership is what carries approver eligibility, so pointing this at a subtree someone " +
					"else controls hands them the ability to grant it.",
				Type: desc.TypeText, Security: desc.SecAuthority, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				// LIVE, and provably so: ldap.go:228 resolves this reference at the top of every Sync.
				Name: "bind_password", Label: "Service bind password",
				Help: "The read-only service account's password. Write-only: stored in the secret backend and " +
					"never read back into this dialog, and never logged. Re-read on every sync, so a save " +
					"takes effect on the next one — no restart.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: desc.Validate refuses a module that names its own secret path.
		// TG_LDAP_BIND_PW must point here — once — after which every rotation is a Save.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("credsource", SourceType), Field: "password"},
		Test: desc.TestSpec{
			// BOTH base DNs, because both are searched and both are AUTHORITY fields. Sync fails closed when
			// users+groups is zero and it is the GROUP subtree that carries approver eligibility, so a verb
			// promising only the user base would leave the half that decides who can approve untested — and
			// a verb must not describe less than the button does.
			Verb: "bind as the service account and search the user and group base DNs, reporting how many " +
				"principals come back (read-only, bounded to a sample)",
			Mutating: false,
		},
	}
}
