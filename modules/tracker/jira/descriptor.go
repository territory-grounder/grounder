package jira

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes Jira's configuration schema so the console can GENERATE its dialog rather than
// hand-render one that drifts from the binary.
//
// Effect is stated per field and it is not decoration. jira.go:110 resolves the token reference inside
// do(), on every request, so a secret written to the lane below takes effect on the NEXT call with no
// restart. EVERY OTHER FIELD IS BOOT-ONLY: cmd/worker/main.go:827-833 reads them once and passes them to
// New/WithTransitions, which capture them in the Module — a save is durable but inert until the worker
// restarts, and the dialog must say so rather than implying success.
//
// The transition ids are here because a real Jira workflow almost never uses the reference ids 21/31/11
// this module falls back to, and POSTing a transition id the target workflow lacks 404s — so a deployment
// that never sets them gets a tracker that reads correctly and fails every single close-out.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "tracker",
		SourceType: SourceType,
		Title:      "Jira",
		Summary: "Jira Cloud as the incident tracker: the issue TG is triggered by, transitions through " +
			"the workflow, and comments on as the session's terminal audit sink.",
		Fields: []desc.Field{
			{
				Name: "base_url", EnvKey: "TG_JIRA_URL", Label: "Site URL",
				Help: "Base URL of the Jira site, e.g. https://example.atlassian.net. Empty means this " +
					"tracker is NOT a capability of this deployment: it is never registered and no session " +
					"can be anchored to a Jira issue.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				// Not a display nicety: Jira Cloud API-token auth is HTTP Basic base64(email:api_token),
				// so this address is the username half of every request's credential. Wrong or blank and
				// the token cannot authenticate anything — every call 401s with a correct token set.
				Name: "email", EnvKey: "TG_JIRA_EMAIL", Label: "Account email",
				Help: "Email of the Jira account the API token belongs to. It is half of the credential, " +
					"not a contact address: a mismatch here 401s every request even with a valid token.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 256,
			},
			{
				Name: "transition_in_progress", EnvKey: "TG_JIRA_TRANSITION_INPROGRESS", Label: "Transition: in progress",
				Help: "Numeric workflow transition id that moves an issue to in-progress in THIS project's " +
					"workflow (see Project settings → Workflows). Not a status name — the API takes an id.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^[0-9]+$`, MaxLen: 32,
			},
			{
				Name: "transition_resolved", EnvKey: "TG_JIRA_TRANSITION_RESOLVED", Label: "Transition: resolved",
				Help: "Numeric transition id that closes an issue out. Leaving it at the reference default " +
					"is the common failure: an id the workflow does not contain 404s, so the close-out is " +
					"lost and the issue silently stays open.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^[0-9]+$`, MaxLen: 32,
			},
			{
				Name: "transition_open", EnvKey: "TG_JIRA_TRANSITION_OPEN", Label: "Transition: open",
				Help: "Numeric transition id that returns an issue to open/backlog.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^[0-9]+$`, MaxLen: 32,
			},
			{
				Name: "token_ref", EnvKey: "TG_JIRA_TOKEN_REF", Label: "API-token reference",
				Help: "Where the API token is read from. Displayed for provenance: set the token itself " +
					"below, not this pointer.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "token", Label: "API token",
				Help: "The Atlassian API token for the account above. What that account may see and change " +
					"is the real ceiling on what TG can do here. Write-only: it is stored in the secret " +
					"backend and never read back into this dialog.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it).
		// TG_JIRA_TOKEN_REF must point here — that pointer is read at boot and nothing rewrites it at
		// runtime, so adopting the prefix is a one-time reference change. Every rotation after that is a Save.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("tracker", SourceType), Field: "token"},
		// The verb says what selftest.go actually does, because the dialog shows it BEFORE the press and a
		// promise the probe cannot honour is the defect this capability exists to close. It no longer says
		// "read one issue back": no field here names an issue, so that would have meant a hardcoded key —
		// and a 404 from "no such issue" is indistinguishable from a 404 from "wrong Jira site entirely".
		Test: desc.TestSpec{
			Verb: "ask Jira which account this email and token authenticate as, and count the projects " +
				"that account may browse — two GETs, no issue is read, transitioned, or commented on",
			Mutating: false,
		},
	}
}
