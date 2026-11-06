package oidctoken

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the OIDC token minter's configuration schema so the console GENERATES its dialog
// rather than a hand-written form that drifts from the binary.
//
// THIS MODULE REGISTERS NO SYNC SOURCE. It is resolver-only: it makes an oidc: SecretRef MINT a short-lived
// Bearer token at use time. So the coverage view will never list it as a credential source, and an operator
// looking for it there is not looking at a fault.
//
// THE CLIENT SECRET IS EffectLive, with one honest caveat the help text carries. mintLocked
// (oidctoken.go:224-231) resolves the client_id and client_secret references at CALL time, per mint — so a
// secret saved to the lane below is used by the next mint with no restart. It is not instant, though: an
// already-minted token is cached until its advertised expiry, so the change lands when that token lapses.
// Saying "live" without saying "on the next mint" would let an operator believe a rotated secret had
// already displaced the old token. Every other field is read once at boot (cmd/worker/main.go:999-1005) and
// captured in the Minter at construction.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "credsource",
		SourceType: SourceType,
		Title:      "OIDC client-credentials token",
		Summary: "Machine-plane token minter: an oidc: SecretRef mints a short-lived Bearer token from the " +
			"provider's client-credentials endpoint at use time. Read-only and fail-closed — it registers no " +
			"sync source and stores no long-lived credential in the estate.",
		Fields: []desc.Field{
			{
				Name: "token_url", EnvKey: "TG_OIDC_TOKEN_URL", Label: "Token endpoint",
				Help: "The provider's client-credentials token endpoint, e.g. " +
					"https://idp.example/realms/tg/protocol/openid-connect/token. Empty means the oidc: scheme " +
					"is never wired and every oidc: reference fails closed.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				Name: "client_id_ref", EnvKey: "TG_OIDC_CLIENT_ID_REF", Label: "client_id reference",
				Help: "Where the OAuth2 client_id is read from. Displayed for provenance and set outside this " +
					"dialog — the one credential this dialog writes is the client secret below. Absent with a " +
					"token endpoint set fails the boot rather than minting anonymously.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "client_secret_ref", EnvKey: "TG_OIDC_CLIENT_SECRET_REF", Label: "client_secret reference",
				Help: "Where the OAuth2 client_secret is read from. Displayed for provenance: set the secret " +
					"itself below, not this pointer.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "scope", EnvKey: "TG_OIDC_SCOPE", Label: "Default scope",
				Help: "Space-separated scopes requested at mint. This bounds what the minted token can do, so " +
					"asking for more than the estate needs hands a wider token to everything that resolves an " +
					"oidc: reference. Empty requests the client's default scope.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				Name: "audience", EnvKey: "TG_OIDC_AUDIENCE", Label: "Audience",
				Help: "Optional audience parameter, when the provider requires one to issue a token the target " +
					"API will accept. Wrong here and the mint succeeds but the target rejects the token.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				Name: "ca_cert_path", EnvKey: "TG_OIDC_CA", Label: "CA certificate path",
				Help: "Path to the private-CA PEM that verifies the provider's certificate. Empty uses the " +
					"system roots. TLS is always verified, so a missing CA fails the mint rather than sending " +
					"the client secret over an unverified connection.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				Name: "auth_style", EnvKey: "TG_OIDC_AUTH_STYLE", Label: "Client authentication style",
				Help: "How the client credentials are presented: post (client_secret_post, the default) or " +
					"basic (client_secret_basic). Providers accept one or the other; the wrong choice returns " +
					"invalid_client on every mint. An unrecognised value fails the boot.",
				// Admits EMPTY because the binary does: credential.go:450 folds "" into post. A pattern
				// that could not match "" would leave an operator unable to clear the box back to the
				// default it already had.
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^(post|basic|client_secret_post|client_secret_basic)?$`, MaxLen: 32,
			},
			{
				// LIVE with a caveat: oidctoken.go:226-231 resolves this per mint, but an unexpired cached
				// token is reused, so the rotation lands on the next mint rather than the next call.
				Name: "client_secret", Label: "OAuth2 client secret",
				Help: "The client_secret TG authenticates to the provider with. Write-only: stored in the " +
					"secret backend and never read back into this dialog. Re-read on every mint, so a save " +
					"takes effect on the next one — the currently cached token is still used until it expires.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: desc.Validate refuses a module that names its own secret path.
		// TG_OIDC_CLIENT_SECRET_REF must point here — once — after which every rotation is a Save.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("credsource", SourceType), Field: "client_secret"},
		Test: desc.TestSpec{
			Verb: "mint one short-lived Bearer token from the provider's client-credentials endpoint and " +
				"discard it (the provider records the issuance in its own audit log)",
			Mutating: false,
		},
	}
}
