package youtrack

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes YouTrack's configuration schema so the console can GENERATE its dialog rather than
// hand-render one that drifts from the binary.
//
// Effect is stated per field and it is not decoration. youtrack.go:145 resolves the token reference inside
// do(), on every request, so a secret written to the lane below takes effect on the NEXT call with no
// restart. EVERY OTHER FIELD IS BOOT-ONLY: cmd/worker/main.go:817-826 reads them once and passes them to
// New/WithStateNames/WithStateFieldName/WithReadOnlyUnless, which capture them in the Module. A save is
// durable but inert until the worker restarts, and the dialog must say that instead of implying success.
//
// The state names are here because the deployment's State bundle is per-project: the stock bundle has no
// `Resolved` value at all (its terminal values are `Fixed`/`Verified`), so a project that leaves these at
// the reference defaults gets a close-out that no-ops against a value that does not exist.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "tracker",
		SourceType: SourceType,
		Title:      "YouTrack",
		Summary: "The issue tracker TG reads incident history from and — only when writes are armed — " +
			"transitions and comments on as the session's terminal audit sink.",
		Fields: []desc.Field{
			{
				Name: "base_url", EnvKey: "TG_YOUTRACK_URL", Label: "Instance URL",
				Help: "Base URL of the YouTrack instance, e.g. https://youtrack.example. Empty means this " +
					"tracker is NOT a capability of this deployment: it is never registered, so nothing " +
					"reads incident history and no session has a ticket to write back to.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				// AUTHORITY, not an ordinary checkbox. This is the single control that decides whether TG
				// may MUTATE the tracker corpus, and the hazard is concrete rather than theoretical: the
				// predecessor is driven by these same issues and reads them, so one TG comment on a live
				// incident contaminates a running comparison in the INPUTS, where no later analysis can
				// undo it. The zero value is the safe posture (WithReadOnlyUnless), which is why the field
				// is phrased as arming writes rather than forbidding them.
				Name: "writes_enabled", EnvKey: "TG_YOUTRACK_WRITES", Label: "Arm tracker writes",
				Help: "Off (the default) makes the module structurally incapable of writing: state " +
					"transitions and comments are refused before a request leaves the process. Turn it on " +
					"only when this instance is not a corpus something else is being measured against. " +
					"Turning it OFF here is not a stop button — it binds at the next worker restart.",
				Type: desc.TypeBool, Security: desc.SecAuthority, Effect: desc.EffectRestart,
			},
			{
				// The name is used in BOTH directions (state.go stateOf reads it, TransitionState writes
				// it), and the READ failure is the one that bites in the default read-only posture: an
				// unmatched field name falls through to Open for every issue, and that read is what the
				// re-fire dedup consults. Help that named only the write consequence would describe the
				// half of the damage that writes-off deployments never see.
				Name: "state_field", EnvKey: "TG_YOUTRACK_STATE_FIELD", Label: "State field name",
				Help: "Custom field holding the workflow state; defaults to \"State\". The name is used in " +
					"BOTH directions, so a project that renamed it and does not say so here gets every " +
					"issue read as open — which suppresses re-fires against already-closed anchors — and " +
					"every transition written to a field that does not exist.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 128,
			},
			{
				Name: "state_in_progress", EnvKey: "TG_YOUTRACK_STATE_INPROGRESS", Label: "Value: in progress",
				Help: "This project's State value meaning work has started (reference default \"In Progress\").",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 128,
			},
			{
				Name: "state_resolved", EnvKey: "TG_YOUTRACK_STATE_RESOLVED", Label: "Value: resolved",
				Help: "This project's State value meaning done. Check it: the stock YouTrack bundle has NO " +
					"\"Resolved\" value — it uses \"Fixed\"/\"Verified\" — so leaving the default in place " +
					"makes every close-out set a value the bundle does not contain.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 128,
			},
			{
				Name: "state_open", EnvKey: "TG_YOUTRACK_STATE_OPEN", Label: "Value: open",
				Help: "This project's State value for a re-opened or untouched issue (reference default \"Open\").",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 128,
			},
			{
				Name: "token_ref", EnvKey: "TG_YOUTRACK_TOKEN_REF", Label: "API-token reference",
				Help: "Where the permanent token is read from. Displayed for provenance: set the token " +
					"itself below, not this pointer.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "token", Label: "API token",
				Help: "The YouTrack permanent token TG authenticates as. Its account's permissions are the " +
					"real ceiling on what TG can do here. Write-only: it is stored in the secret backend " +
					"and never read back into this dialog.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it).
		// TG_YOUTRACK_TOKEN_REF must point here — that pointer is read at boot and nothing rewrites it at
		// runtime, so adopting the prefix is a one-time reference change. Every rotation after that is a Save.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("tracker", SourceType), Field: "token"},
		// The verb says what selftest.go actually does, because the dialog shows it BEFORE the press and a
		// promise the probe cannot honour is the defect this capability exists to close. It no longer says
		// "read one issue back": no field here names an issue, so that would have meant a hardcoded id —
		// and a 404 from "no such issue" is indistinguishable from a 404 from "wrong instance entirely".
		Test: desc.TestSpec{
			Verb: "authenticate to YouTrack and report the account TG acts as and the projects that " +
				"account can see — two GETs, no issue is read, written, or commented on",
			Mutating: false,
		},
	}
}
