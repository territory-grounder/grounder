package mattermost

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes Mattermost's configuration schema so the console can GENERATE its dialog rather than
// hand-render one that drifts from the binary.
//
// EFFECT IS STATED PER FIELD AND IT IS NOT DECORATION. mattermost.go:88 resolves the token reference inside
// do(), on every call, so a credential written to the lane below takes effect on the NEXT post with no
// restart. The other fields are captured at construction — cmd/worker/main.go reads TG_MATTERMOST_* once
// and hands them to mattermost.New through bootstrap.RegisterNotifiers, and no per-use accessor is wired
// for this module — so a save is durable but INERT until the worker restarts, and the dialog must say so
// rather than implying success it did not achieve.
//
// MATTERMOST IS ONE-WAY TODAY AND THE DIALOG MAY NOT PRETEND OTHERWISE. This module implements ResolveVote,
// but cmd/worker/main.go:4013 arms the vote.inbound seam for MATRIX ONLY — every other backend needs an
// inbound HTTP route, "declared dark rather than half-built" — so nothing in any composition root calls it.
// The approver set below stays an AUTHORITY field, because it is the trust boundary the moment such a route
// exists; but neither the summary nor any help sentence may imply TG reads Mattermost for replies, and the
// console must render the field's ARMED status from wiring.SeamVoteInbound (core/wiring/seam.go:114).
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "notifier",
		SourceType: SourceType,
		Title:      "Mattermost",
		Summary: "Governance notices and approval polls to a Mattermost channel. One-way today: TG posts, " +
			"and the votes come back through the console — nothing reads Mattermost for replies.",
		Fields: []desc.Field{
			{
				Name: "url", EnvKey: "TG_MATTERMOST_URL", Label: "Server base URL",
				Help: "Base URL of the Mattermost server, e.g. https://chat.example. Empty means this " +
					"notifier is not registered at all — no notice or poll ever reaches Mattermost.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				// REQUIRED because Notify FAILS CLOSED on an unmapped route: create-post rejects a channel
				// name with 400, so this module never guesses one. An empty map is a notifier that registers
				// cleanly and drops every approval poll.
				Name: "channels", EnvKey: "TG_MATTERMOST_CHANNELS", Label: "Channel routing",
				Help: "Maps each routed channel NAME to the opaque 26-character channel ID create-post " +
					"requires: \"tg-approvals=xk3n…, ops-approvals=q9df…\". The KEY is always " +
					"<project>-approvals, lowercased from the decision id's prefix (the part before its " +
					"first -, # or :) — a key of any other shape is never looked up. A decision whose route " +
					"is unmapped is REFUSED rather than posted to a guessed channel, so map one entry for " +
					"every project prefix that raises decisions here.",
				Type: desc.TypeKVMap, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxItems: 32, MaxLen: 256,
			},
			{
				// AUTHORITY, not ordinary text: ResolveVote refuses any poster not listed here (INV-12), so
				// widening it widens who can approve a production change. But TODAY IT GOVERNS NOTHING — no
				// composition root calls mattermost.ResolveVote — and the field's own non-emptiness can never
				// reveal that, which is why the console takes its ARMED status from the vote.inbound seam
				// rather than from this value.
				//
				// NOT Required, for that same reason: the worker registers this notifier on TG_MATTERMOST_URL
				// alone and delivers every post with the set empty, so demanding a value would block a working
				// notice-only deployment behind an authority box that decides nothing. Empty also fails CLOSED
				// the day an inbound route is armed — Authenticate refuses every sender.
				Name: "approvers", EnvKey: "TG_MATTERMOST_APPROVERS", Label: "Allowed responders",
				Help: "Mattermost usernames whose vote TG would accept, without the leading @ — the match is " +
					"exact against the post's user_name, so \"@alice\" is an approver who could never " +
					"actually approve. TG does not read Mattermost for replies today: the inbound vote " +
					"reader is armed for Matrix only, so votes are cast in the console and this set gates " +
					"nothing until a Mattermost inbound route exists.",
				Type: desc.TypeIDList, Security: desc.SecAuthority, Effect: desc.EffectRestart,
				Pattern: `^[a-z0-9._-]+$`, MaxItems: 32, MaxLen: 256,
			},
			{
				Name: "token_ref", EnvKey: "TG_MATTERMOST_TOKEN_REF", Label: "Access-token reference",
				Help: "Where the bot's access token is read from. Displayed for provenance: set the token " +
					"itself below, not this pointer.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "token", Label: "Bot access token",
				Help: "The personal-access or bot token TG posts as. Write-only: it is stored in the secret " +
					"backend and never read back into this dialog.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it).
		// TG_MATTERMOST_TOKEN_REF must point HERE for the live effect above to be real — while the ref still
		// reads env:MATTERMOST_TOKEN, a saved token lands in the lane and the running module keeps resolving
		// the old environment value. Repointing it is a one-time change; every rotation after is a Save.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("notifier", SourceType), Field: "token"},
		// A read: /api/v4/users/me returns the identity behind the stored token and posts nothing. Pressing
		// Test must never put a message in front of humans who did not ask for one.
		Test: desc.TestSpec{
			// This probe POSTS. See desc.TestSpec.Emits: a scheduled sweep must skip it, or monitoring
			// becomes noise aimed at the room that has to stay readable during an incident.
			Emits: true, Verb: "read the account this token belongs to (nothing is posted)", Mutating: false},
	}
}
