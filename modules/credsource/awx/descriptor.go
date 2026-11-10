package awx

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the AWX credential source's configuration schema so the console GENERATES its dialog
// rather than a hand-written form that drifts from the binary.
//
// THE SECRET IS EffectRestart, NOT LIVE, AND THAT DIFFERENCE IS THE POINT OF THE FIELD. Client.token()
// (awx.go:138) resolves the token reference ONCE and caches it for the life of the process — `if c.cached
// != "" { return c.cached }`. So a token saved here is durable, but the running worker keeps presenting the
// OLD one until it restarts. Matrix can honestly claim live because matrix.go re-resolves inside every
// send; this connector cannot, and a dialog that said "live" would report a rotation that had not
// happened — leaving an operator to believe a revoked token was out of use while AWX was still accepting
// it. Every other field is read once at boot (cmd/worker/main.go:973-982) and captured at construction.
//
// SCOPE: these are the CREDENTIAL-SOURCE keys (TG_AWX_*). The AWX job actuator (TG_AWXJOB_*) and the
// runbook knowledge lane (TG_AWXPLAYBOOKS_*) are separate modules with separate tokens and separate
// dialogs, deliberately: this one is read-only inventory, and it must not become the place an operator
// configures a launch token.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "credsource",
		SourceType: SourceType,
		Title:      "AWX (credential inventory)",
		Summary: "Machine-plane credential source: pulls AWX inventory hosts and job-template bindings into " +
			"host identities carrying SecretRef pointers. Read-only — it launches nothing.",
		Fields: []desc.Field{
			{
				Name: "addr", EnvKey: "TG_AWX_ADDR", Label: "AWX base URL",
				Help: "Base URL of the AWX API, e.g. https://awx.example. Empty means this source is never " +
					"registered and AWX contributes no host identities at all.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				Name: "token_ref", EnvKey: "TG_AWX_TOKEN_REF", Label: "API-token reference",
				Help: "Where the AWX OAuth2 token is read from. Displayed for provenance: set the token itself " +
					"below, not this pointer. Defaults to env:AWX_TOKEN when unset.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "ca_cert_path", EnvKey: "TG_AWX_CA", Label: "CA certificate path",
				Help: "Path to the private-CA PEM that verifies the AWX server certificate. Empty uses the " +
					"system roots. TLS is never skipped, so a missing CA fails the sync rather than " +
					"downgrading the connection.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				Name: "inventory_id", EnvKey: "TG_AWX_INVENTORY_ID", Label: "Inventory id",
				Help: "Optional AWX inventory id to scope the sync to. Empty syncs every inventory the token " +
					"can see. A non-numeric value fails the boot rather than silently syncing everything.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^[0-9]*$`, MaxLen: 16,
			},
			{
				Name: "ref_scheme", EnvKey: "TG_AWX_REF_SCHEME", Label: "Emitted SecretRef scheme",
				Help: "Scheme of the SecretRefs this source puts in synced bundles (default \"store\"). It must " +
					"name a scheme something actually resolves — a bundle full of references nobody " +
					"dereferences fails closed at use time, long after the sync looked healthy.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^[a-z][a-z0-9+.-]*$`, MaxLen: 32,
			},
			{
				Name: "ref_prefix", EnvKey: "TG_AWX_REF_PREFIX", Label: "Emitted SecretRef prefix",
				Help: "Path prefix prepended to each emitted SecretRef, so a synced host resolves to the right " +
					"location in the backing store.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				Name: "ref_field", EnvKey: "TG_AWX_REF_FIELD", Label: "Emitted SecretRef field",
				Help: "Field name appended after '#' in each emitted SecretRef (the key inside the stored " +
					"secret). Wrong here and every resolve returns a not-found, never a blank credential.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 256,
			},
			{
				// TypeText, NOT TypeKVMap, and the reason is the separator. This map is split on ';'
				// (bootstrap.parseAWXCredRefMap) precisely because AWX credential names routinely contain
				// commas and parentheses — "SSH ED25519 (one_key)". A kvmap widget serialises with commas,
				// so rendering this as one would silently split a credential name in half and drop the
				// binding, which surfaces as a host with no identity rather than as an error.
				Name: "cred_ref_map", EnvKey: "TG_AWX_CRED_REF_MAP", Label: "Credential → SecretRef map",
				Help: "Job-template mode only. Semicolon-separated \"AWX credential name=SecretRef\" pairs, " +
					"e.g. \"SSH ED25519 (one_key)=file:/secrets/one_key;SSH Lab Common=file:/secrets/lab\". " +
					"Empty disables job-template bundles. A pair with no '=' fails the boot rather than " +
					"dropping a binding.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 4096,
			},
			{
				Name: "default_user", EnvKey: "TG_AWX_DEFAULT_USER", Label: "Default login user",
				Help: "Login user for job-template bindings whose AWX credential carries no username " +
					"(default \"root\"). Wrong here and every affected host authenticates as the wrong account.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 64,
			},
			{
				// EffectRestart, deliberately — see the header note on awx.go:138. Saving rotates the stored
				// credential; the running worker keeps the cached one until it restarts.
				Name: "token", Label: "AWX API token",
				Help: "The AWX OAuth2 token TG reads inventory with — a read-only-scoped token is enough. " +
					"Write-only: stored in the secret backend and never read back into this dialog. The " +
					"worker caches its token at first use, so a save here takes effect on the next restart.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectRestart, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: desc.Validate refuses a module that names its own secret path.
		// TG_AWX_TOKEN_REF must point here — once — after which every rotation is a Save.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("credsource", SourceType), Field: "token"},
		Test: desc.TestSpec{
			// The verb NAMES BOTH READS because both happen, and a consent contract that discloses one of two
			// requests is the same defect as a button that performs none. The account lookup is what lets the
			// dialog answer "which credential is this?" — the fault this estate actually hits is the wrong
			// token, not a broken one — and it is what makes the hosts read's 403 attributable to a
			// permission rather than to the token itself.
			Verb: "look up the account the saved token belongs to, then list the configured inventory's " +
				"hosts — authenticated read-only GETs that launch nothing",
			Mutating: false,
		},
	}
}
