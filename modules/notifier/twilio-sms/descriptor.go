package twiliosms

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes Twilio SMS's configuration schema so the console can GENERATE its dialog rather than
// hand-render one that drifts from the binary.
//
// THERE IS NO APPROVER FIELD AND THAT IS DELIBERATE. This module is a SEND-ONLY pager: ResolveVote rejects
// every inbound payload, so no identity set could ever authorize anything here. Declaring an "allowed
// responders" box because the other notifiers have one would render an authority control that governs
// nothing — the exact class of defect this surface exists to remove.
//
// EFFECT IS STATED PER FIELD AND IT IS NOT DECORATION. twilio_sms.go:83 resolves the token reference inside
// do(), on every call, so a credential written to the lane below takes effect on the NEXT page with no
// restart. The other fields are captured at construction — cmd/worker/main.go reads TG_TWILIO_* once and
// hands them to twiliosms.New through bootstrap.RegisterNotifiers — so a save is durable but INERT until
// the worker restarts, and the dialog must say so rather than implying success it did not achieve.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "notifier",
		SourceType: SourceType,
		Title:      "Twilio SMS",
		Summary: "A send-only pager: it texts a redacted governance decision to one on-call number, at " +
			"most once per decision, and accepts no votes back.",
		Fields: []desc.Field{
			{
				Name: "url", EnvKey: "TG_TWILIO_URL", Label: "Twilio API base URL",
				Help: "Base URL of the Twilio REST API, normally https://api.twilio.com. Empty means this " +
					"notifier is not registered at all — no page is ever sent.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				Name: "account_sid", EnvKey: "TG_TWILIO_SID", Label: "Account SID",
				Help: "The Twilio account the message is sent from (ACxxxxxxxx…). It is both the request " +
					"path segment and the Basic-auth username, so a wrong or empty value fails every send " +
					"with 401/404 — the page is never delivered and the on-call is never woken.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, Pattern: `^AC[0-9a-fA-F]{32}$`, MaxLen: 64,
			},
			{
				Name: "from_number", EnvKey: "TG_TWILIO_FROM", Label: "Sender number",
				Help: "The E.164 number the page is sent FROM (+15551234567). It must be a number this " +
					"Twilio account owns; anything else is rejected by Twilio at send time.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, Pattern: `^\+[1-9]\d{6,14}$`, MaxLen: 32,
			},
			{
				Name: "to_number", EnvKey: "TG_TWILIO_TO", Label: "On-call number",
				Help: "The single E.164 number every page is delivered to (+15559876543). There is no " +
					"routing and no list: whoever holds this number is the person TG wakes.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, Pattern: `^\+[1-9]\d{6,14}$`, MaxLen: 32,
			},
			{
				Name: "token_ref", EnvKey: "TG_TWILIO_TOKEN_REF", Label: "Auth-token reference",
				Help: "Where the Twilio auth token is read from. Displayed for provenance: set the token " +
					"itself below, not this pointer.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "token", Label: "Auth token",
				Help: "The Twilio auth token, paired with the account SID as Basic-auth credentials. " +
					"Write-only: it is stored in the secret backend and never read back into this dialog.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it).
		// TG_TWILIO_TOKEN_REF must point HERE for the live effect above to be real — while the ref still
		// reads env:TWILIO_TOKEN, a saved token lands in the lane and the running module keeps resolving the
		// old environment value. Repointing it is a one-time change; every rotation after is a Save.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("notifier", SourceType), Field: "token"},
		// This module's ONLY call is a send, and a Test that sends is an unreviewed page to a human's phone
		// triggered from a settings dialog. So Test reads instead: fetching the account record exercises the
		// SID and the token — the two things that actually break — and delivers no SMS.
		Test: desc.TestSpec{
			// This probe POSTS. See desc.TestSpec.Emits: a scheduled sweep must skip it, or monitoring
			// becomes noise aimed at the room that has to stay readable during an incident.
			Emits: true, Verb: "read the Twilio account record for this SID (no SMS is sent)", Mutating: false},
	}
}
