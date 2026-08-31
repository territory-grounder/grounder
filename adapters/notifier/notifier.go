// Package notifier is the stable interface for the notifier-and-approval surface: it renders a governance
// decision as an out-of-band message and resolves a returned vote against the specific pending decision id
// it answers.
//
// Provenance: [O] INV-12 (a vote binds to exactly the decision it answers — no global cursor, no
// cross-room misattribution), INV-19 (approval is an audited act), spec/008. Sender authentication against
// the approver set and credential/PII redaction are interface obligations EVERY backend inherits — Matrix,
// Twilio SMS, and Mattermost day-1; Slack, Teams, and email as reference.
package notifier

import (
	"context"
	"strings"
)

// Notice is a decision rendered for an out-of-band channel, bound to the decision it concerns.
type Notice struct {
	DecisionID string // the pending decision this notice concerns; a vote MUST bind back to it (INV-12)
	Body       string // the rendered body; the backend redacts credentials and PII before posting
	Approval   bool   // true when this notice solicits an approval vote (a poll); false for a page/notice
	// Choices are the options an approver may pick, in order. Empty on a page.
	//
	// This is a LIST because a governed decision is not inherently binary: the interesting case is several
	// candidate PLANS for one incident, where the approver is choosing between remediations rather than
	// waving one through. A backend that can render a real poll (Matrix MSC3381) puts one option per
	// answer; a text-only backend falls back to prose.
	//
	// Until 2026-08-02 this did not exist and Approval was ACCEPTED AND IGNORED: every notice, poll or
	// page, went out as the same flat message, and the approver had to type an exact command nothing had
	// told them about. TG's agent proposes ONE action today, so a live poll carries that plan plus the
	// governance options — the plural is not aspiration, it is what keeps the wire format from having to
	// change when alternatives arrive.
	Choices []Choice
}

// Choice is one selectable option on an approval poll.
type Choice struct {
	// ID is the stable, machine-readable option id. It MUST embed the decision id (see ChoiceID), because
	// a Matrix poll response carries only the chosen answer's id — no text, no context. Encoding the
	// decision there is what lets a vote bind to exactly the decision it answers (INV-12) with no
	// server-side state mapping poll events back to decisions.
	ID string
	// Label is the human-facing text of the option.
	Label string
	// Approve marks the options that AUTHORIZE the action. Anything else stands the action down; there is
	// no third truth value, because the gate is binary even when the menu is not.
	Approve bool
}

// ChoiceID builds an answer id that carries its own decision binding: "<verb>|<decisionID>".
func ChoiceID(verb, decisionID string) string { return verb + "|" + decisionID }

// SplitChoiceID recovers (verb, decisionID) from an answer id. ok is false when the id carries no
// binding — which must be REFUSED rather than guessed at, since an unbound vote is a vote for an unknown
// decision.
func SplitChoiceID(id string) (verb, decisionID string, ok bool) {
	i := strings.IndexByte(id, '|')
	if i <= 0 || i == len(id)-1 {
		return "", "", false
	}
	return id[:i], id[i+1:], true
}

// Vote is an approval response bound to the exact decision it answers.
type Vote struct {
	DecisionID string // MUST equal the Notice.DecisionID it answers — no global cursor, no misattribution
	Sender     string // the responder; authenticated against the approver set before the vote is accepted
	Approve    bool
	// Choice is the id of the option the approver selected, when the backend offered a menu. Empty for a
	// plain text reply. It exists so a multi-PLAN poll can say WHICH plan was chosen rather than only
	// that something was approved.
	Choice string
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
