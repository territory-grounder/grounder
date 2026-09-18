package openbao

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the OpenBao/Vault credential source's configuration schema so the console GENERATES
// its dialog instead of a hand-written form that drifts from the binary.
//
// EVERY FIELD IS EffectRestart, AND THAT IS NOT LAZINESS. cmd/worker/main.go:958-972 reads each key once
// into bootstrap.CredentialConfig, and bootstrap.buildOpenBaoSource captures the values in the client and
// the Source at construction. The auth token is cached inside that client (vault.go:312 tokenNow), so even
// the credential is not re-read from config after boot. A dialog claiming "live" here would report a
// success it did not achieve.
//
// THIS MODULE DELIBERATELY DECLARES NO SECRET VALUE FIELD, and the reason is a bootstrap circularity, not
// an oversight. desc.ModuleSecretPath would put this module's secret at
// secret/data/tg/modules/credsource/openbao — INSIDE OpenBao. Reading it requires an authenticated OpenBao
// client, which is the very thing this credential configures. Pointing TG_OPENBAO_TOKEN_REF at a bao: ref
// served by the client that ref is meant to build cannot resolve; it would fail closed at boot and look
// like a broken backend. The binary already says so in its own voice: the secret-policy preflight lists
// TG_OPENBAO_{TOKEN,ROLE_ID,SECRET_ID,WRAP_TOKEN,JWT}_REF as EXEMPT, with the reason "none can come from
// the backend they authenticate, REQ-2401" (cmd/worker/main.go:439-441, 464-466). So the reference fields
// here are provenance ONLY. No secret lane is declared, catalog.Lane returns ErrNoSecretLane, and the
// console renders this module's secret field disabled — the honest state, and strictly better than a Save
// that could never take effect.
//
// ONE ADDRESS, TWO CONSUMERS — WHICH IS WHY addr IS AUTHORITY. TG_OPENBAO_ADDR/_CA/_CERT/_KEY/_TOKEN_REF
// also drive the PROCESS's own bao: delivery wiring (cmd/worker/main.go:681-687, cmd/grounder/main.go:
// 508-513) and the console's module-secret writer (cmd/grounder/main.go:715). Every bao: reference in the
// process resolves through whatever this address names — including TG's admin and operator tokens and the
// session key. That makes it the root of trust for the process's own identity, not a connector endpoint,
// and it must not render as a plain text box beside "KV v2 mount".
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "credsource",
		SourceType: SourceType,
		Title:      "OpenBao / HashiCorp Vault",
		Summary: "Machine-plane credential source over KV v2: syncs a path prefix into host bindings and " +
			"dereferences bao: SecretRefs at use time. Read-only — it resolves identities, never mutates them.",
		Fields: []desc.Field{
			{
				// AUTHORITY, not an ordinary endpoint — see the two-consumers note in the header.
				Name: "addr", EnvKey: "TG_OPENBAO_ADDR", Label: "OpenBao address",
				Help: "Base URL of the OpenBao/Vault API, e.g. https://openbao01.example:8200. THIS IS THE " +
					"WHOLE PROCESS'S SECRET BACKEND, not just this connector's: every bao: reference " +
					"resolves here, including TG's own admin and operator tokens and the session key, and " +
					"it is also where the console writes every other module's rotated credential. Pointing " +
					"it somewhere else changes what TG trusts to tell it who its own operators are. Empty " +
					"means this source is never registered: every bao: reference then fails closed and no " +
					"synced credential resolves.",
				Type: desc.TypeURL, Security: desc.SecAuthority, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				// The method decides WHICH of the reference fields below the boot requires. A method whose
				// refs are absent aborts the boot rather than starting unauthenticated — so the commonest
				// 3am failure here is "auth_method says approle and role_id_ref is empty".
				Name: "auth_method", EnvKey: "TG_OPENBAO_AUTH_METHOD", Label: "Auth method",
				Help: "How TG authenticates: token, approle, wrapped-approle, jwt or cert (default token). " +
					"Whichever you pick, its reference fields below must be set — a half-configured method " +
					"fails the worker boot instead of running with no identity.",
				// The pattern ADMITS EMPTY because the binary does: credential.go:258-260 maps "" to
				// token. A pattern that could not match "" would leave an operator unable to clear the
				// box back to the default it already had.
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^(token|approle|wrapped-approle|jwt|cert)?$`, MaxLen: 32,
			},
			{
				Name: "token_ref", EnvKey: "TG_OPENBAO_TOKEN_REF", Label: "Static-token reference",
				Help: "auth_method=token: where the static OpenBao token is read from (env:/file:). Displayed " +
					"for provenance — OpenBao's own credential cannot be stored in OpenBao, so it is set " +
					"outside this dialog.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "role_id_ref", EnvKey: "TG_OPENBAO_ROLE_ID_REF", Label: "AppRole role_id reference",
				Help: "auth_method=approle or wrapped-approle: where the role_id is read from. The role_id is " +
					"not secret on its own; it is the SecretID beside it that is.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "secret_id_ref", EnvKey: "TG_OPENBAO_SECRET_ID_REF", Label: "AppRole secret_id reference",
				Help: "auth_method=approle: where the AppRole SecretID is read from. Empty with " +
					"auth_method=approle fails the boot rather than binding anonymously.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "wrap_token_ref", EnvKey: "TG_OPENBAO_WRAP_TOKEN_REF", Label: "Wrapping-token reference",
				Help: "auth_method=wrapped-approle: the single-use response-wrapping token that delivers the " +
					"SecretID, normally a file: ref on tmpfs so no durable secret ever lands on disk. It is " +
					"consumed at boot; a restart needs a freshly delivered one.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "jwt_ref", EnvKey: "TG_OPENBAO_JWT_REF", Label: "JWT reference",
				Help: "auth_method=jwt: where the signed JWT is read from. Needs jwt_role set as well — either " +
					"one alone fails the boot.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "jwt_role", EnvKey: "TG_OPENBAO_JWT_ROLE", Label: "JWT role",
				Help: "auth_method=jwt: the OpenBao JWT auth role to log in as. The role's policy is what " +
					"bounds everything this source can read.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 256,
			},
			{
				Name: "cert_path", EnvKey: "TG_OPENBAO_CERT", Label: "Client certificate path",
				Help: "auth_method=cert: filesystem path to the FreeIPA-CA client certificate TG presents. " +
					"This is an identity the host IS, so no bootstrap token is stored anywhere.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				Name: "key_path", EnvKey: "TG_OPENBAO_KEY", Label: "Client key path",
				Help: "auth_method=cert: path to the matching private key, expected to be a root-only file. " +
					"Set without cert_path it fails the boot; both are required together.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				Name: "cert_role", EnvKey: "TG_OPENBAO_CERT_ROLE", Label: "Certificate auth role",
				Help: "auth_method=cert: optional OpenBao cert role name. Empty lets OpenBao match the " +
					"certificate against every configured role.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 256,
			},
			{
				Name: "ca_cert_path", EnvKey: "TG_OPENBAO_CA", Label: "CA certificate path",
				Help: "Path to the private-CA PEM that verifies the OpenBao server certificate. Empty uses the " +
					"system roots. TLS verification is never skipped, so a wrong or missing CA shows up as a " +
					"connection failure, not as an insecure connection.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				Name: "kv_mount", EnvKey: "TG_OPENBAO_KV_MOUNT", Label: "KV v2 mount",
				Help: "The KV v2 mount to sync from (default \"secret\"). A wrong mount produces an empty sync, " +
					"not an error — no host binding is learned and every lookup falls through to a " +
					"lower-precedence source.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 128,
			},
			{
				Name: "kv_prefix", EnvKey: "TG_OPENBAO_KV_PREFIX", Label: "KV v2 path prefix",
				Help: "Path prefix under the mount that the sync LISTs, e.g. \"hosts/\". Leave empty to sync " +
					"the mount root.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 512,
			},
		},
		// No desc.SecretLane: see the circularity note above. OpenBao's own credential cannot live inside
		// OpenBao, so this module has no secret to write and the console must not offer one.
		Test: desc.TestSpec{
			Verb:     "LIST the configured KV v2 mount and prefix — key names only, no secret value is read",
			Mutating: false,
		},
	}
}
