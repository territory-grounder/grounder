package awxjob

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the AWX-job lane's configuration schema so the console can GENERATE its dialog
// rather than hand-render one that drifts from the binary.
//
// EFFECT IS RESTART FOR EVERY FIELD HERE, INCLUDING THE TOKEN — and that is the honest answer, not a
// conservative one. cmd/worker/main.go:4900-4930 reads all four env keys ONCE and captures them: the base
// URL and CA path go into the *Client at NewClient, the allowlist is parsed and frozen into the Actuator at
// New. The token is the one worth stating explicitly, because matrix's is LIVE and the difference is not
// visible from the dialog: client.go token() caches the resolved value in c.cached for the client's
// lifetime, and the client is built once at boot. A rotation saved to the lane below therefore does NOT
// take effect on the next launch — the worker keeps presenting the old token until it restarts. Rendering
// that as "live" would give an operator a rotation that reports success while the revoked credential is
// still in use, which is worse than telling them to restart.
//
// A SEPARATE HONESTY, stated in the Summary because an operator who fills this form in will otherwise
// expect launches: configuring this lane does not ARM it. The launch is a mutating effect and reaches the
// network only beneath the mode chokepoint, which refuses at Shadow through two independent gates. A fully
// configured lane is still inert until the owner-present live-mode flip.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "actuation",
		SourceType: SourceType,
		Title:      "AWX job launch",
		Summary: "The AWX-job effect lane: TG launches an allowlisted AWX job template instead of a shell " +
			"command. Configuring it does not arm it — the launch sits beneath the mode chokepoint and " +
			"refuses at Shadow until the owner flips to a live mode.",
		Fields: []desc.Field{
			{
				Name: "base_url", EnvKey: "TG_AWXJOB_BASE_URL", Label: "AWX base URL",
				Help: "Base URL of AWX / automation-controller, e.g. https://awx.example — no trailing " +
					"slash and no /api. Unset (or with the launch token unset) the lane keeps its " +
					"fail-closed default and can only REFUSE; it never falls back to another channel.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				// AUTHORITY, not ordinary text. This is the operator's declaration of WHICH templates TG
				// may launch at all and WHICH op-class each is bound to (REQ-1704). A template absent from
				// it is unlaunchable; adding an entry grants TG a new mutating capability, and changing an
				// op_class re-points a template at a different policy + graduation bucket. It must never
				// render as an ordinary text box.
				Name: "allowlist", EnvKey: "TG_AWXJOB_ALLOWLIST", Label: "Sanctioned job templates",
				Help: "JSON array of {\"template_id\":N,\"op_class\":\"...\",\"extra_vars\":{\"name\":\"string|number|bool\"}}. " +
					"ONLY these templates are launchable, and only under the op_class bound here — the " +
					"model cannot name any other template. extra_vars is the CLOSED set of launch " +
					"variables; an undeclared key is rejected. Empty means the lane can only refuse. A " +
					"malformed entry or an illegal var type stops the worker at boot rather than " +
					"silently narrowing the allowlist. Binding the SAME op_class to TWO templates " +
					"makes that op unlaunchable — the runner cannot choose between them, so it refuses.",
				Type: desc.TypeText, Security: desc.SecAuthority, Effect: desc.EffectRestart,
				MaxLen: 16384,
			},
			{
				Name: "ca_cert", EnvKey: "TG_AWXJOB_CA", Label: "Private CA certificate path",
				Help: "Path to a PEM CA bundle to trust when AWX is behind a private CA. Leave empty for a " +
					"publicly trusted certificate. A path that cannot be read or contains no certificate " +
					"stops the worker at boot — it never falls back to skipping verification.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				Name: "launch_token_ref", EnvKey: "TG_AWXJOB_LAUNCH_TOKEN_REF", Label: "Launch-token reference",
				Help: "Where the launch-capable token is read from. Displayed for provenance: set the token " +
					"itself below, not this pointer. It must stay DISTINCT from the read-only sensor token " +
					"the playbooks knowledge lane uses — one credential doing both jobs erases the " +
					"read/write separation this lane is built on.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "launch_token", Label: "AWX launch token",
				Help: "The launch-capable AWX OAuth2 token. Write-only: stored in the secret backend and " +
					"never read back into this dialog. The client caches it after first use, so a rotation " +
					"saved here is only picked up when the worker restarts.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectRestart, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it).
		// TG_AWXJOB_LAUNCH_TOKEN_REF must point here — that pointer is read at boot and nothing rewrites it
		// at runtime, so adopting the prefix is a one-time reference change. Every rotation after that is a
		// Save (plus a restart, per the effect above), with no OpenBao or .env work.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("actuation", SourceType), Field: "token"},
		// THE VERB IS THE CONSENT CONTRACT, and for THIS module it is the most load-bearing sentence in the
		// file: the only mutating thing this lane can do is start an AWX job, so an operator pressing Test
		// must be certain, before the press, that it will not. It says GET and it says "launches nothing"
		// because both halves are enforced in selftest.go — the probe calls c.do with http.MethodGet twice
		// and never reaches Launch.
		//
		// The previous verb ("read one AWX job's status") described a read this probe cannot perform:
		// Client.GetJob needs a job id, and a lane that has never launched anything has none to offer. A verb
		// naming an action nothing can carry out is the same defect as a dialog with no probe behind it, so it
		// was replaced with what selftest.go actually does — identity plus a re-read of the sanctioned
		// templates, which is also the check that catches an allowlist pointed at the wrong AWX.
		Test: desc.TestSpec{
			Verb: "ask AWX which account the launch token belongs to and re-read each sanctioned job template " +
				"by id — GET only, it launches nothing",
			Mutating: false,
		},
	}
}
