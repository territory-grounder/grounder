package servicenow

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes ServiceNow's configuration schema so the console can GENERATE its dialog rather
// than hand-render one that drifts from the binary.
//
// Effect is stated per field and it is not decoration. servicenow.go:102 resolves the password reference
// inside do(), on every request, so a secret written to the lane below takes effect on the NEXT call with
// no restart. EVERY OTHER FIELD IS BOOT-ONLY: cmd/worker/main.go:838-843 reads them once and passes them
// to New/WithStates, which capture them in the Module — a save is durable but inert until the worker
// restarts, and the dialog must say so rather than implying success.
//
// The state codes are here because `incident.state` is a per-instance choice list: 2/6/1 are only the
// out-of-box ITSM values, and an instance that customized its state model will accept a PATCH carrying a
// code its model does not use, leaving the incident in a state nobody's queue is watching.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "tracker",
		SourceType: SourceType,
		Title:      "ServiceNow",
		Summary: "ServiceNow incidents as the tracker, over the Table API: the incident TG is triggered " +
			"by, moves through the state model, and appends work notes to as the session's audit sink.",
		Fields: []desc.Field{
			{
				Name: "base_url", EnvKey: "TG_SERVICENOW_URL", Label: "Instance URL",
				Help: "Base URL of the instance, e.g. https://example.service-now.com. Empty means this " +
					"tracker is NOT a capability of this deployment: it is never registered and no session " +
					"can be anchored to an incident.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				// Not a display name: Table API auth is HTTP Basic base64(username:password), so this is
				// the username half of every request's credential — and the account's ACLs, not anything
				// in this dialog, decide which incidents TG can actually read or change.
				Name: "username", EnvKey: "TG_SERVICENOW_USER", Label: "Instance user",
				Help: "User TG authenticates as. It is half of the credential, not a label: a mismatch " +
					"401s every request even with the right password. Its ACLs bound what TG can touch.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 128,
			},
			{
				Name: "state_in_progress", EnvKey: "TG_SERVICENOW_STATE_INPROGRESS", Label: "State code: in progress",
				Help: "This instance's incident.state code for work in progress (out-of-box ITSM: 2).",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^[0-9]+$`, MaxLen: 8,
			},
			{
				Name: "state_resolved", EnvKey: "TG_SERVICENOW_STATE_RESOLVED", Label: "State code: resolved",
				Help: "This instance's incident.state code for resolved (out-of-box ITSM: 6). A code your " +
					"state model does not use is still accepted by the Table API, so a wrong value here " +
					"parks closed-out incidents in a state no queue is watching. Code 7 (Closed) is always " +
					"read back as resolved regardless of this setting.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^[0-9]+$`, MaxLen: 8,
			},
			{
				Name: "state_open", EnvKey: "TG_SERVICENOW_STATE_OPEN", Label: "State code: open",
				Help: "This instance's incident.state code for new/open (out-of-box ITSM: 1). Any code TG " +
					"does not recognise is read back as open — never as resolved.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^[0-9]+$`, MaxLen: 8,
			},
			{
				Name: "token_ref", EnvKey: "TG_SERVICENOW_TOKEN_REF", Label: "Password reference",
				Help: "Where the user's password is read from. Displayed for provenance: set the password " +
					"itself below, not this pointer.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "token", Label: "Password",
				Help: "Password for the instance user above — Table API auth is HTTP Basic, so this is a " +
					"password, not a bearer token. Write-only: it is stored in the secret backend and never " +
					"read back into this dialog.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it).
		// TG_SERVICENOW_TOKEN_REF must point here — that pointer is read at boot and nothing rewrites it at
		// runtime, so adopting the prefix is a one-time reference change. Every rotation after that is a Save.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("tracker", SourceType), Field: "token"},
		// The verb says what selftest.go actually does, because the dialog shows it BEFORE the press and a
		// promise the probe cannot honour is the defect this capability exists to close. It says QUERY
		// rather than "read one incident back": the probe needs no sys_id, deliberately, because a 404 on
		// a hardcoded id cannot be told apart from a credential pointed at a different instance.
		Test: desc.TestSpec{
			Verb: "query the incident table for the newest record and check the journal table is " +
				"readable — two bounded, query-only GETs; no incident is created, moved, or annotated",
			Mutating: false,
		},
	}
}
