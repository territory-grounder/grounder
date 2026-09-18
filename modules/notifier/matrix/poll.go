package matrix

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
)

// MATRIX POLLS (MSC3381) — a governed decision that asks for a vote must LOOK like a vote.
//
// Until 2026-08-02 this module posted every notice as a flat `m.room.message`, ignoring Notice.Approval
// entirely. A POLL_PAUSE proposal therefore arrived as prose, and the only way to answer it was to type
// a command whose exact syntax nothing had disclosed — first word a verb, second word the decision id.
// An approver who replied "yes, go ahead" produced no vote at all, and the poll timed out to DENY.
//
// Three MSCs are in play, and TG emits the same combination the predecessor does so both systems render
// identically in every client:
//
//	MSC3381  org.matrix.msc3381.poll.start / .response, kind org.matrix.msc3381.poll.disclosed
//	MSC1767  org.matrix.msc1767.text on the question and each answer (Extensible Events)
//	m.room.message   the plain-text fallback body, for clients that cannot render a poll
//
// THE ANSWER ID CARRIES ITS OWN DECISION BINDING ("approve|INC-123"). A poll response event contains
// only the chosen answer's id — no text, no decision reference — so binding a vote to a decision would
// otherwise require server-side state mapping poll events back to decisions, and any gap in that map is
// a vote counted against the wrong decision. Encoding the binding in the id makes INV-12 structural: a
// response either carries its decision or is refused.
const (
	pollStartType    = "org.matrix.msc3381.poll.start"
	pollDisclosed    = "org.matrix.msc3381.poll.disclosed"
	msc1767Text      = "org.matrix.msc1767.text"
	pollMaxSelection = 1
)

// pollStartContent is the MSC3381 poll.start event body.
type pollStartContent struct {
	Poll     pollDefinition `json:"org.matrix.msc3381.poll.start"`
	Fallback string         `json:"org.matrix.msc1767.text"`
}

type pollDefinition struct {
	Kind          string       `json:"kind"`
	MaxSelections int          `json:"max_selections"`
	Question      textBlock    `json:"question"`
	Answers       []pollAnswer `json:"answers"`
}

type textBlock struct {
	Text string `json:"org.matrix.msc1767.text"`
}

type pollAnswer struct {
	ID   string `json:"id"`
	Text string `json:"org.matrix.msc1767.text"`
}

// sendPoll posts an MSC3381 disclosed poll. The fallback body is the SAME redacted prose a text-only
// deployment would receive, so a client that cannot render polls still shows the operator the decision
// rather than an empty event.
func (m *Module) sendPoll(ctx context.Context, room string, n notifier.Notice) error {
	answers := make([]pollAnswer, 0, len(n.Choices))
	for _, c := range n.Choices {
		if c.ID == "" || c.Label == "" {
			// A nameless or unlabelled option is refused rather than posted: an answer a human cannot
			// read, or that carries no binding, is a vote waiting to be misattributed.
			return fmt.Errorf("matrix: poll choice for %s has an empty id or label", n.DecisionID)
		}
		answers = append(answers, pollAnswer{ID: c.ID, Text: notifier.Redact(c.Label)})
	}
	if len(answers) == 0 {
		return fmt.Errorf("matrix: approval notice %s carries no choices", n.DecisionID)
	}
	body := pollStartContent{
		Poll: pollDefinition{
			Kind:          pollDisclosed, // disclosed: the room sees the tally, which is what makes it an audit surface
			MaxSelections: pollMaxSelection,
			Question:      textBlock{Text: notifier.Redact(n.Body)},
			Answers:       answers,
		},
		Fallback: notifier.Redact(n.Body),
	}
	_, err := m.do(ctx, http.MethodPut,
		"/_matrix/client/v3/rooms/"+room+"/send/"+pollStartType+"/"+url.PathEscape(txnID(n.DecisionID)), body)
	return err
}

// txnID derives the send transaction id from the decision id, so a retried send is IDEMPOTENT at the
// homeserver rather than posting a second poll for the same decision — two open polls for one decision
// is two vote surfaces that can disagree.
func txnID(decisionID string) string { return "tg-" + decisionID }
