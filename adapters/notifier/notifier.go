// Package notifier is the stable interface for the notifier-and-approval surface: it renders a governance
// decision as an out-of-band message and resolves a returned vote against the specific pending decision id
// it answers.
//
// Provenance: [O] INV-12 (a vote binds to exactly the decision it answers — no global cursor, no
// cross-room misattribution), INV-19 (approval is an audited act), spec/008. Sender authentication against
// the approver set and credential/PII redaction are interface obligations EVERY backend inherits — Matrix,
// Twilio SMS, and Mattermost day-1; Slack, Teams, and email as reference.
package notifier

import "context"

// Notice is a decision rendered for an out-of-band channel, bound to the decision it concerns.
type Notice struct {
	DecisionID string // the pending decision this notice concerns; a vote MUST bind back to it (INV-12)
	Body       string // the rendered body; the backend redacts credentials and PII before posting
	Approval   bool   // true when this notice solicits an approval vote (a poll); false for a page/notice
}

// Vote is an approval response bound to the exact decision it answers.
type Vote struct {
	DecisionID string // MUST equal the Notice.DecisionID it answers — no global cursor, no misattribution
	Sender     string // the responder; authenticated against the approver set before the vote is accepted
	Approve    bool
}

// Notifier renders a decision as a message and resolves a returned vote. A backend MUST authenticate the
// sender against the approver set and bind each vote to its decision id before accepting it, and MUST
// redact credentials and PII from every posted body.
type Notifier interface {
	// SourceType is the source/vendor slug (e.g. "matrix", "twilio-sms", "mattermost", "slack").
	SourceType() string
	// Notify posts a notice or approval poll to the resolved destination.
	Notify(ctx context.Context, n Notice) error
	// ResolveVote parses and authenticates an inbound response into a Vote bound to its decision id. A
	// response whose sender is not in the approver set, or that binds to no pending decision, is rejected.
	ResolveVote(ctx context.Context, raw []byte) (Vote, error)
}
