// Package teams is the loadable Microsoft Teams notifier-and-approval module (spec/008 REQ-809) — a
// reference backend for the notifier surface.
//
// It implements adapters/notifier.Notifier: it renders a governance decision as an out-of-band Teams
// activity and resolves a returned vote against the specific pending decision id it answers (INV-12).
// Sender authentication against the approver set and credential/PII redaction are interface obligations
// shared by every backend (adapters/notifier.Authenticate / Redact). The HTTP transport is injectable (a
// Doer) so the oracle drives the real code path against a fake bot service. The access token is a secret
// reference, resolved per request, never a literal (INV-13).
//
// Provenance: [O] INV-12/INV-13/INV-19, spec/008.
package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/core/config"
)

// SourceType is the vendor slug this module serves.
const SourceType = "teams"

// Doer is the minimal HTTP contract; *http.Client satisfies it, and tests inject a fake bot service.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Module is the Microsoft Teams notifier-and-approval adapter. Construct with New.
type Module struct {
	baseURL        string
	conversationID string
	tokenRef       config.SecretRef
	approvers      []string
	http           Doer
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// New builds a Teams module for a bot-service base URL, the destination conversation id the bot posts
// activities into (the MANDATORY {conversationId} segment of the Bot Framework send-activity route), an
// access-token secret reference (e.g. "env:TG_TEAMS_TOKEN"), and the approver set whose members may cast a
// vote.
func New(baseURL string, conversationID string, tokenRef config.SecretRef, approvers []string, opts ...Option) *Module {
	m := &Module{baseURL: strings.TrimRight(baseURL, "/"), conversationID: conversationID, tokenRef: tokenRef, approvers: approvers, http: http.DefaultClient}
	for _, o := range opts {
		o(m)
	}
	return m
}

// SourceType implements adapters/notifier.Notifier.
func (m *Module) SourceType() string { return SourceType }

// compile-time proof the module satisfies the stable notifier interface.
var _ notifier.Notifier = (*Module)(nil)

// do issues an authenticated JSON request against the Teams bot service. The access token is resolved from
// its secret reference at call time (INV-13); a non-2xx response is an error.
func (m *Module) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("teams: resolve token: %w", err)
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("teams: %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// activity is the message activity posted to a Teams conversation. DecisionID rides alongside the visible
// text so the pending decision id round-trips to the channel and a human's reply ("approve <id>") binds the
// returned vote back to exactly this decision (INV-12).
type activity struct {
	Type       string `json:"type"`
	Text       string `json:"text"`
	DecisionID string `json:"decisionId"`
}

// Notify renders a notice/poll and posts it as a Teams message activity to the module's conversation. The
// body is credential- and PII-redacted before it leaves the process (INV-19) — a redacted body is the only
// thing that reaches the channel. The pending decision id is appended (in the clear — it is an identifier,
// not a secret) so the human sees which decision to cite and the returned vote binds back to it (INV-12).
func (m *Module) Notify(ctx context.Context, n notifier.Notice) error {
	if n.DecisionID == "" {
		return fmt.Errorf("teams: notice has no decision id")
	}
	text := notifier.Redact(n.Body) + "\n\nTo vote, reply: approve " + n.DecisionID + " · deny " + n.DecisionID
	body := activity{Type: "message", Text: text, DecisionID: n.DecisionID}
	// Bot Framework send-activity route: POST /v3/conversations/{conversationId}/activities — the
	// {conversationId} segment is MANDATORY (an empty one 404s and nothing is delivered). Path-escape the id
	// so its ':'/'@'/'#' cannot be reparsed as URL structure (mirrors matrix escaping its room id).
	path := "/v3/conversations/" + url.PathEscape(m.conversationID) + "/activities"
	_, err := m.do(ctx, http.MethodPost, path, body)
	return err
}

// teamsActivity is the inbound reply activity this module parses to resolve a vote: the sender (from.id)
// and the text carrying the vote verb and the pending decision id.
type teamsActivity struct {
	From struct {
		ID string `json:"id"`
	} `json:"from"`
	Text string `json:"text"`
}

// parseVote extracts the decision id and the approve/deny intent from a reply text like
// "approve <decisionID>" or "deny <decisionID>". A text that does not begin with a recognized verb and
// cite a decision id is not a vote.
func parseVote(text string) (decisionID string, approve bool, ok bool) {
	fields := strings.Fields(text)
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

// ResolveVote parses an inbound Teams activity, authenticates the sender (from.id) against the approver
// set, and binds the vote to the pending decision id referenced in the text. An unauthenticated sender
// (not in the approver set) or a reply that cites no decision id is rejected — a vote is never counted for
// the wrong decision or from the wrong person (INV-12).
func (m *Module) ResolveVote(_ context.Context, raw []byte) (notifier.Vote, error) {
	var ev teamsActivity
	if err := json.Unmarshal(raw, &ev); err != nil {
		return notifier.Vote{}, fmt.Errorf("teams: malformed activity: %w", err)
	}
	if !notifier.Authenticate(ev.From.ID, m.approvers) {
		return notifier.Vote{}, fmt.Errorf("teams: sender %q is not in the approver set", ev.From.ID)
	}
	decisionID, approve, ok := parseVote(ev.Text)
	if !ok {
		return notifier.Vote{}, fmt.Errorf("teams: reply cites no pending decision id")
	}
	return notifier.Vote{DecisionID: decisionID, Sender: ev.From.ID, Approve: approve}, nil
}
