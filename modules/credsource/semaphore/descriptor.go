package semaphore

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the Semaphore credential source's configuration schema so the console GENERATES its
// dialog rather than a hand-written form that drifts from the binary.
//
// THE SECRET IS EffectRestart, NOT LIVE. Client.token() (semaphore.go:130) resolves the token reference
// ONCE and caches it for the process lifetime — `if c.cached != "" { return c.cached }`. A token saved here
// is stored durably, but the running worker keeps sending the OLD one until it restarts. Claiming "live"
// would tell an operator a revoked token was out of use while Semaphore was still accepting it, which is
// the exact class of silent-success defect this surface exists to remove. Every other field is read once at
// boot (cmd/worker/main.go:983-990) and captured in the client and Source at construction.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "credsource",
		SourceType: SourceType,
		Title:      "Semaphore (credential inventory)",
		Summary: "Machine-plane credential source: pulls Semaphore project inventories into host identities " +
			"carrying SecretRef pointers. Read-only — it runs no task.",
		Fields: []desc.Field{
			{
				Name: "addr", EnvKey: "TG_SEMAPHORE_ADDR", Label: "Semaphore base URL",
				Help: "Base URL of the Semaphore API, e.g. https://semaphore.example. Empty means this source " +
					"is never registered and Semaphore contributes no host identities at all.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				Name: "token_ref", EnvKey: "TG_SEMAPHORE_TOKEN_REF", Label: "API-token reference",
				Help: "Where the Semaphore API token is read from. Displayed for provenance: set the token " +
					"itself below, not this pointer. Defaults to env:SEMAPHORE_TOKEN when unset.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "ca_cert_path", EnvKey: "TG_SEMAPHORE_CA", Label: "CA certificate path",
				Help: "Path to the private-CA PEM that verifies the Semaphore server certificate. Empty uses " +
					"the system roots. TLS is never skipped, so a missing CA fails the sync rather than " +
					"downgrading the connection.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				Name: "project_id", EnvKey: "TG_SEMAPHORE_PROJECT_ID", Label: "Project id",
				Help: "Optional Semaphore project id to scope the sync to. Empty syncs every project the token " +
					"can see. A non-numeric value fails the boot rather than silently widening the scope.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^[0-9]*$`, MaxLen: 16,
			},
			{
				Name: "ref_scheme", EnvKey: "TG_SEMAPHORE_REF_SCHEME", Label: "Emitted SecretRef scheme",
				Help: "Scheme of the SecretRefs this source puts in synced bundles (default \"store\"). It " +
					"must name a scheme something actually resolves — a bundle of references nobody " +
					"dereferences fails closed at use time, long after the sync looked healthy.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^[a-z][a-z0-9+.-]*$`, MaxLen: 32,
			},
			{
				Name: "ref_prefix", EnvKey: "TG_SEMAPHORE_REF_PREFIX", Label: "Emitted SecretRef prefix",
				Help: "Path prefix prepended to each emitted SecretRef, so a synced host resolves to the right " +
					"location in the backing store.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				Name: "ref_field", EnvKey: "TG_SEMAPHORE_REF_FIELD", Label: "Emitted SecretRef field",
				Help: "Field name appended after '#' in each emitted SecretRef (the key inside the stored " +
					"secret). Wrong here and every resolve returns a not-found, never a blank credential.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 256,
			},
			{
				// EffectRestart, deliberately — see the header note on semaphore.go:130.
				Name: "token", Label: "Semaphore API token",
				Help: "The Semaphore API token TG reads inventories with — a read-only-scoped token is enough. " +
					"Write-only: stored in the secret backend and never read back into this dialog. The worker " +
					"caches its token at first use, so a save here takes effect on the next restart.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectRestart, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: desc.Validate refuses a module that names its own secret path.
		// TG_SEMAPHORE_TOKEN_REF must point here — once — after which every rotation is a Save.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("credsource", SourceType), Field: "token"},
		Test: desc.TestSpec{
			// The verb states what happens in BOTH configurations, because the module has two: with a project
			// id it reads that project, without one it lists every project the token can see and then reads
			// the first. The original wording ("the configured project's inventories") described only the
			// scoped case and silently promised nothing for the unscoped one, which is the commoner setup.
			Verb: "read the configured Semaphore project — or, when none is set, list the projects the saved " +
				"token can see and read the first — then list that project's inventories (authenticated " +
				"read-only GETs; no task is run)",
			Mutating: false,
		},
	}
}
