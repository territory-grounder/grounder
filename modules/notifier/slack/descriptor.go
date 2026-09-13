package slack

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes Slack's configuration schema so the console can GENERATE its dialog rather than
// hand-render one that drifts from the binary.
//
// EFFECT IS STATED PER FIELD AND IT IS NOT DECORATION. slack.go:130 resolves the token reference inside
// do(), on every call, so a credential written to the lane below takes effect on the NEXT post with no
// restart. Everything else is captured at construction: cmd/worker/main.go reads TG_SLACK_* once and hands
// the values to slack.New via bootstrap.RegisterNotifiers, and — unlike matrix — no per-use accessor is
// wired for this module. A save is therefore durable but INERT until the worker restarts, and the dialog
// must say so rather than implying success it did not achieve.
//
// SLACK IS ONE-WAY TODAY AND THE DIALOG MAY NOT PRETEND OTHERWISE. This module implements ResolveVote, but
// cmd/worker/main.go:4013 arms the vote.inbound seam for MATRIX ONLY — Slack needs an inbound HTTP route,
// "declared dark rather than half-built" — so nothing in any composition root calls it. The approver set
// below stays an AUTHORITY field, because it is the trust boundary the moment such a route exists; but
// neither the summary nor any help sentence may imply TG reads Slack for replies, and the console must
// render the field's ARMED status from wiring.SeamVoteInbound (core/wiring/seam.go:114).
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "notifier",
		SourceType: SourceType,
		Title:      "Slack",
		Summary: "Governance notices and approval polls to a Slack channel. One-way today: TG posts, and " +
			"the votes come back through the console — nothing reads Slack for replies.",
		Fields: []desc.Field{
			{
				Name: "url", EnvKey: "TG_SLACK_URL", Label: "Slack API base URL",
				Help: "Base URL of the Slack Web API, normally https://slack.com. Empty means this notifier " +
					"is not registered at all — no notice or poll ever reaches Slack.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				Name: "default_channel", EnvKey: "TG_SLACK_DEFAULT_CHANNEL", Label: "Approvals channel",
				Help: "Channel every notice and poll posts to unless a per-project route overrides it. Prefer " +
					"the channel ID (C0123ABCD…), which is stable across renames. Left empty, TG posts to a " +
					"computed #<project>-approvals name — and if no such channel exists with the bot in it, " +
					"Slack answers channel_not_found and the poll never reaches a human.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 256,
			},
			{
				Name: "channels", EnvKey: "TG_SLACK_CHANNELS", Label: "Routed channels",
				Help: "Optional per-project routing. The KEY is the computed route name and nothing else: " +
					"#<project>-approvals, lowercased from the decision id's prefix (the part before its " +
					"first -, # or :). So \"#tg-approvals=C0123ABCD, #ops-approvals=C0456EFGH\" — a key of " +
					"any other shape is never looked up. An unmapped route falls back to the approvals " +
					"channel above.",
				Type: desc.TypeKVMap, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				MaxItems: 32, MaxLen: 256,
			},
			{
				// AUTHORITY, not ordinary text: ResolveVote refuses any sender not listed here (INV-12), so
				// widening it widens who can approve a production change and it must never render as a plain
				// text box. But TODAY IT GOVERNS NOTHING — no composition root calls slack.ResolveVote — and
				// the field's own non-emptiness can never reveal that, which is precisely why the console
				// takes its ARMED status from the vote.inbound seam and not from this value.
				//
				// NOT Required, for that same reason: the worker registers this notifier on TG_SLACK_URL
				// alone and delivers every notice with the set empty, so demanding a value would block a
				// working notice-only deployment behind an authority box that decides nothing. Empty also
				// fails CLOSED the day an inbound route is armed — Authenticate refuses every sender.
				Name: "approvers", EnvKey: "TG_SLACK_APPROVERS", Label: "Allowed responders",
				Help: "Slack USER IDS whose vote TG would accept (U0123ABCD…, not @handles — the match is " +
					"exact against the id Slack puts in the reply, so a handle or an email address here is " +
					"an approver who could never actually approve). TG does not read Slack for replies " +
					"today: the inbound vote reader is armed for Matrix only, so votes are cast in the " +
					"console and this set gates nothing until a Slack inbound route exists.",
				Type: desc.TypeIDList, Security: desc.SecAuthority, Effect: desc.EffectRestart,
				Pattern: `^[UW][A-Z0-9]+$`, MaxItems: 32, MaxLen: 256,
			},
			{
				Name: "token_ref", EnvKey: "TG_SLACK_TOKEN_REF", Label: "Bot-token reference",
				Help: "Where the bot token is read from. Displayed for provenance: set the token itself " +
					"below, not this pointer.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "token", Label: "Bot token",
				Help: "The xoxb- bot token TG posts as. Write-only: it is stored in the secret backend and " +
					"never read back into this dialog.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it).
		// TG_SLACK_TOKEN_REF must point HERE for the live effect above to be real — while the ref still
		// reads env:SLACK_TOKEN, a saved token lands in the lane and the running module keeps resolving the
		// old environment value. Repointing the ref is a one-time change; every rotation after it is a Save.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("notifier", SourceType), Field: "token"},
		// A read: auth.test returns the bot identity and workspace for the stored token. It posts nothing —
		// pressing Test must never put a message in front of humans who did not ask for one.
		Test: desc.TestSpec{
			// This probe POSTS. See desc.TestSpec.Emits: a scheduled sweep must skip it, or monitoring
			// becomes noise aimed at the room that has to stay readable during an incident.
			Emits: true, Verb: "ask Slack who this token authenticates as (no message is posted)", Mutating: false},
	}
}
