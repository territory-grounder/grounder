package teams

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes Teams's configuration schema so the console can GENERATE its dialog rather than
// hand-render one that drifts from the binary.
//
// EFFECT IS STATED PER FIELD AND IT IS NOT DECORATION. teams.go:72 resolves the token reference inside
// do(), on every call, so a credential written to the lane below takes effect on the NEXT activity with no
// restart. The other fields are captured at construction — cmd/worker/main.go reads TG_TEAMS_* once and
// hands them to teams.New through bootstrap.RegisterNotifiers, and no per-use accessor is wired for this
// module — so a save is durable but INERT until the worker restarts, and the dialog must say so.
//
// TEAMS IS ONE-WAY TODAY AND THE DIALOG MAY NOT PRETEND OTHERWISE. This module implements ResolveVote, but
// cmd/worker/main.go:4013 arms the vote.inbound seam for MATRIX ONLY — Teams needs an inbound HTTP route,
// "declared dark rather than half-built" — so nothing in any composition root calls it. The approver set
// below stays an AUTHORITY field, because it is the trust boundary the moment such a route exists; but
// neither the summary nor any help sentence may imply TG reads Teams for replies, and the console must
// render the field's ARMED status from wiring.SeamVoteInbound (core/wiring/seam.go:114).
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "notifier",
		SourceType: SourceType,
		Title:      "Microsoft Teams",
		Summary: "Governance notices and approval polls as Bot Framework activities in one Teams " +
			"conversation. One-way today: TG posts, and the votes come back through the console — " +
			"nothing reads the conversation for replies.",
		Fields: []desc.Field{
			{
				Name: "url", EnvKey: "TG_TEAMS_URL", Label: "Bot service base URL",
				Help: "Base URL of the Bot Framework service endpoint for this bot (e.g. " +
					"https://smba.trafficmanager.net/emea/). Empty means this notifier is not registered at " +
					"all — no notice or poll ever reaches Teams.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				Name: "conversation", EnvKey: "TG_TEAMS_CONVERSATION", Label: "Approvals conversation ID",
				Help: "The conversation every notice and poll is posted into, e.g. " +
					"19:abc123…@thread.tacv2. It is a MANDATORY path segment of the send-activity route: " +
					"left empty, every post 404s and nothing is delivered — there is no fallback destination.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 256,
			},
			{
				// AUTHORITY, not ordinary text: ResolveVote refuses any sender whose from.id is not listed
				// here (INV-12), so widening it widens who can approve a production change. But TODAY IT
				// GOVERNS NOTHING — no composition root calls teams.ResolveVote — and the field's own
				// non-emptiness can never reveal that, which is why the console takes its ARMED status from
				// the vote.inbound seam rather than from this value.
				//
				// NOT Required, for that same reason: the worker registers this notifier on TG_TEAMS_URL
				// alone and delivers every activity with the set empty, so demanding a value would block a
				// working notice-only deployment behind an authority box that decides nothing. Empty also
				// fails CLOSED the day an inbound route is armed — Authenticate refuses every sender.
				//
				// NO Pattern, deliberately: from.id is an opaque Bot Framework identifier whose shape varies
				// by channel and tenant, and a regex guessed from one example would refuse valid approvers.
				Name: "approvers", EnvKey: "TG_TEAMS_APPROVERS", Label: "Allowed responders",
				Help: "Teams user IDs whose vote TG would accept, copied exactly as the activity's from.id " +
					"reports them (typically 29:1abc…). The match is byte-for-byte, so a display name or a " +
					"UPN here is an approver who could never actually approve. TG does not read the " +
					"conversation for replies today: the inbound vote reader is armed for Matrix only, so " +
					"votes are cast in the console and this set gates nothing until a Teams inbound route " +
					"exists.",
				Type: desc.TypeIDList, Security: desc.SecAuthority, Effect: desc.EffectRestart,
				MaxItems: 32, MaxLen: 256,
			},
			{
				Name: "token_ref", EnvKey: "TG_TEAMS_TOKEN_REF", Label: "Access-token reference",
				Help: "Where the bot's access token is read from. Displayed for provenance: set the token " +
					"itself below, not this pointer.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "token", Label: "Bot access token",
				Help: "The Bot Framework access token TG posts with. Write-only: it is stored in the secret " +
					"backend and never read back into this dialog.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it).
		// TG_TEAMS_TOKEN_REF must point HERE for the live effect above to be real — while the ref still
		// reads env:TEAMS_TOKEN, a saved token lands in the lane and the running module keeps resolving the
		// old environment value. Repointing the ref is a one-time change; every rotation after it is a Save.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("notifier", SourceType), Field: "token"},
		// A read: listing the conversation's members proves both the token and the conversation id in one
		// call, and delivers nothing. Pressing Test must never put an activity in front of humans.
		Test: desc.TestSpec{
			// This probe POSTS. See desc.TestSpec.Emits: a scheduled sweep must skip it, or monitoring
			// becomes noise aimed at the room that has to stay readable during an incident.
			Emits: true, Verb: "read the members of the approvals conversation (nothing is posted)", Mutating: false},
	}
}
