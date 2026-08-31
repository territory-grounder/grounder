package email

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the SMTP notifier's configuration schema so the console can GENERATE its dialog
// rather than hand-render one that drifts from the binary.
//
// EFFECT IS STATED PER FIELD AND IT IS NOT DECORATION. email.go:116 resolves the password reference inside
// Notify, on every send, so a credential written to the lane below takes effect on the NEXT message with no
// restart. Everything else is captured at construction — cmd/worker/main.go reads TG_EMAIL_* once and hands
// the values to email.New through bootstrap.RegisterNotifiers, and no per-use accessor is wired for this
// module — so a save is durable but INERT until the worker restarts, and the dialog must say so rather than
// implying success it did not achieve.
//
// EMAIL IS SEND-ONLY TODAY AND THE DIALOG MAY NOT PRETEND OTHERWISE. This module implements ResolveVote and
// parses a real inbound reply in its tests, but cmd/worker/main.go:4013 arms the vote.inbound seam for
// MATRIX ONLY — email needs an inbound route, "declared dark rather than half-built" — so no composition
// root reads a mailbox and nothing calls ResolveVote. The approver set below stays an AUTHORITY field,
// because it is the trust boundary the moment such a route exists; but neither the summary nor any help
// sentence may imply TG reads replies, and the console must render the field's ARMED status from
// wiring.SeamVoteInbound (core/wiring/seam.go:114).
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "notifier",
		SourceType: SourceType,
		Title:      "Email (SMTP)",
		Summary: "Governance notices and approval polls as plain email to a fixed recipient set. Send-only " +
			"today: TG mails out, and the votes come back through the console — no mailbox is read.",
		Fields: []desc.Field{
			{
				Name: "smtp_addr", EnvKey: "TG_EMAIL_SMTP", Label: "SMTP relay address",
				Help: "The relay as host:port, e.g. smtp.example:587. THE PORT IS NOT OPTIONAL — the host " +
					"part is split off at the last colon and used as the auth server identity, and a bare " +
					"host fails to dial. Empty means this notifier is not registered at all, so no notice " +
					"or poll is ever mailed.",
				// The pattern demands the PORT and nothing more. A tighter host part (\S minus ':') would
				// refuse "[::1]:587", which email.New splits correctly and net/smtp dials — a dialog that
				// rejects a value the binary accepts is the same lie as one that accepts a value it ignores.
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, Pattern: `^\S+:[0-9]+$`, MaxLen: 256,
			},
			{
				Name: "from", EnvKey: "TG_EMAIL_FROM", Label: "Envelope From",
				Help: "The address TG sends as, and the one a human's reply would land in — TG itself reads " +
					"no mailbox. Most relays refuse a message whose From they do not own, so a wrong value " +
					"here fails every send.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, Pattern: `^[^@\s]+@[^@\s]+$`, MaxLen: 256,
			},
			{
				// Recipients decide WHO SEES a decision, not who may release it — a wrong entry here leaks a
				// (redacted) notice, whereas a wrong entry in approvers moves the trust boundary. Ordinary.
				Name: "to", EnvKey: "TG_EMAIL_TO", Label: "Recipients",
				Help: "Every address a notice or poll is delivered to. There is no per-project routing: this " +
					"one set receives all of them. Empty means the mail is composed and delivered to nobody.",
				Type: desc.TypeIDList, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, Pattern: `^[^@\s]+@[^@\s]+$`, MaxItems: 32, MaxLen: 256,
			},
			{
				// AUTHORITY, not ordinary text: ResolveVote refuses any sender not listed here (INV-12). It
				// is separate from the recipient list on purpose — being told about a decision is not
				// permission to approve it, and an operator must be able to widen one without silently
				// widening the other. But TODAY IT GOVERNS NOTHING — no composition root calls
				// email.ResolveVote — and the field's own non-emptiness can never reveal that, which is why
				// the console takes its ARMED status from the vote.inbound seam rather than from this value.
				//
				// NOT Required, for that same reason: the worker registers this notifier on TG_EMAIL_SMTP
				// alone and mails every notice with the set empty, so demanding a value would block a working
				// send-only deployment behind an authority box that decides nothing. Empty also fails CLOSED
				// the day an inbound route is armed — Authenticate refuses every sender.
				Name: "approvers", EnvKey: "TG_EMAIL_APPROVERS", Label: "Allowed responders",
				Help: "Addresses whose reply TG would accept as a vote, matched exactly against the inbound " +
					"sender — a recipient above who is not listed here could not approve. TG reads no " +
					"mailbox today: the inbound vote reader is armed for Matrix only, so votes are cast in " +
					"the console and this set gates nothing until an email inbound route exists.",
				Type: desc.TypeIDList, Security: desc.SecAuthority, Effect: desc.EffectRestart,
				Pattern: `^[^@\s]+@[^@\s]+$`, MaxItems: 32, MaxLen: 256,
			},
			{
				Name: "smtp_user", EnvKey: "TG_EMAIL_SMTP_USER", Label: "SMTP username",
				Help: "Username for PLAIN authentication to the relay. Leave EMPTY only for a relay that " +
					"accepts unauthenticated mail — an empty username takes the no-auth path and the " +
					"password below is never used. When set, the credential is only transmitted over a " +
					"TLS-upgraded link, so a relay without STARTTLS will refuse to send rather than leak it.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 256,
			},
			{
				Name: "password_ref", EnvKey: "TG_EMAIL_SMTP_TOKEN_REF", Label: "SMTP-password reference",
				Help: "Where the SMTP password is read from. Displayed for provenance: set the password " +
					"itself below, not this pointer.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "password", Label: "SMTP password",
				Help: "The password for the username above. Write-only: it is stored in the secret backend " +
					"and never read back into this dialog. If it cannot be resolved at send time TG refuses " +
					"to send rather than fall back to an unauthenticated message.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it). The
		// lane's key is the uniform "token" — the same name the existing TG_EMAIL_SMTP_TOKEN_REF already
		// uses for this credential — so the console's writer needs no per-module special case.
		//
		// TG_EMAIL_SMTP_TOKEN_REF must point HERE for the live effect above to be real: while the ref still
		// reads env:EMAIL_SMTP_PASSWORD, a saved password lands in the lane and the running module keeps
		// resolving the old environment value. Repointing it is a one-time change per deployment; every
		// rotation after that is a Save, with no OpenBao or .env work.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("notifier", SourceType), Field: "token"},
		// A read, and the honest one for SMTP: open the session, STARTTLS, authenticate, then quit before
		// any RCPT. It proves the relay, the port, the username and the password without putting a message
		// in anyone's inbox — a Test that mailed the recipient set would be an unreviewed broadcast fired
		// from a settings dialog.
		Test: desc.TestSpec{
			// This probe POSTS. See desc.TestSpec.Emits: a scheduled sweep must skip it, or monitoring
			// becomes noise aimed at the room that has to stay readable during an incident.
			Emits: true, Verb: "connect to the relay and authenticate, then disconnect without sending mail", Mutating: false},
	}
}
