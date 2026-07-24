package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
)

// matrixEvent is the inbound reply event this module parses to resolve a vote: the sender (an @user:server
// mxid) and the message body carrying the vote and the pending decision id.
type matrixEvent struct {
	Sender  string `json:"sender"`
	Content struct {
		Body string `json:"body"`
	} `json:"content"`
}

// parseVote extracts the decision id and the approve/deny intent from a reply body like
// "approve <decisionID>" or "deny <decisionID>". A body that does not begin with a recognized verb and
// cite a decision id is not a vote.
func parseVote(body string) (decisionID string, approve bool, ok bool) {
	fields := strings.Fields(body)
	if len(fields) < 2 {
		return "", false, false
	}
	switch strings.ToLower(fields[0]) {
	case "approve", "approved", "yes", "+1", "ack":
		approve = true
	case "deny", "denied", "no", "reject", "-1", "nack":
		approve = false
	default:
		return "", false, false
	}
	return fields[1], approve, true
}

// ResolveVote parses an inbound Matrix reply, authenticates the sender against the approver set, and binds
// the vote to the pending decision id referenced in the body. An unauthenticated sender (not in the
// approver set) or a reply that cites no decision id is rejected — a vote is never counted for the wrong
// decision or from the wrong person (INV-12).
func (m *Module) ResolveVote(_ context.Context, raw []byte) (notifier.Vote, error) {
	var ev matrixEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return notifier.Vote{}, fmt.Errorf("matrix: malformed event: %w", err)
	}
	if !notifier.Authenticate(ev.Sender, m.approvers) {
		return notifier.Vote{}, fmt.Errorf("matrix: sender %q is not in the approver set", ev.Sender)
	}
	decisionID, approve, ok := parseVote(ev.Content.Body)
	if !ok {
		return notifier.Vote{}, fmt.Errorf("matrix: reply cites no pending decision id")
	}
	return notifier.Vote{DecisionID: decisionID, Sender: ev.Sender, Approve: approve}, nil
}
