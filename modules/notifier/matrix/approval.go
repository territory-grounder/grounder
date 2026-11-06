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
	Type    string `json:"type"`
	Sender  string `json:"sender"`
	Content struct {
		Body string `json:"body"`
		// MSC3381 poll response (unstable prefix) and its stabilized name. A client may emit either, so
		// BOTH are read — the predecessor learned the same lesson and accepts both spellings.
		ResponseUnstable pollSelections `json:"org.matrix.msc3381.poll.response"`
		ResponseStable   pollSelections `json:"m.poll.response"`
	} `json:"content"`
}

type pollSelections struct {
	Answers []string `json:"answers"`
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
	// Read the approver set LIVE where one is injected: revoking someone's approval rights is usually
	// urgent, and taking effect at the next deploy is not good enough for a trust-boundary change.
	approvers := m.approvers
	if m.liveApprovers != nil {
		if live := m.liveApprovers(); len(live) > 0 {
			approvers = live
		}
	}
	if !notifier.Authenticate(ev.Sender, approvers) {
		return notifier.Vote{}, fmt.Errorf("matrix: sender %q is not in the approver set", ev.Sender)
	}
	// A POLL RESPONSE first: it is the path a human actually clicks, and it binds to its decision through
	// the answer id rather than through anything the responder typed.
	if sel := firstSelection(ev); sel != "" {
		verb, decisionID, ok := notifier.SplitChoiceID(sel)
		if !ok {
			// An answer id with no embedded decision cannot be attributed. Refuse rather than guess: a
			// vote counted against the wrong decision is worse than a vote lost.
			return notifier.Vote{}, fmt.Errorf("matrix: poll answer %q carries no decision binding", sel)
		}
		return notifier.Vote{
			DecisionID: decisionID, Sender: ev.Sender,
			Approve: approvingVerb(verb), Choice: sel,
		}, nil
	}
	decisionID, approve, ok := parseVote(ev.Content.Body)
	if !ok {
		return notifier.Vote{}, fmt.Errorf("matrix: reply cites no pending decision id")
	}
	return notifier.Vote{DecisionID: decisionID, Sender: ev.Sender, Approve: approve}, nil
}

// firstSelection returns the single chosen answer id from a poll response, under either the unstable or
// the stabilized key. max_selections is 1, so a response carrying several answers is malformed and is
// ignored rather than resolved to its first entry — picking one would be inventing the approver's intent.
func firstSelection(ev matrixEvent) string {
	for _, sel := range [][]string{ev.Content.ResponseUnstable.Answers, ev.Content.ResponseStable.Answers} {
		if len(sel) == 1 {
			return sel[0]
		}
	}
	return ""
}

// approvingVerb reports whether an answer id's verb AUTHORIZES the action. Anything not explicitly
// approving stands it down — "investigate" is not consent, and neither is an unrecognised verb.
func approvingVerb(verb string) bool {
	switch strings.ToLower(verb) {
	case "approve", "approved", "yes", "ack", "plan":
		return true
	default:
		return false
	}
}
