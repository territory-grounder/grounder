// Package slack is the loadable Slack notifier-and-approval reference module (spec/008 REQ-809).
//
// It implements adapters/notifier.Notifier: it renders a governance decision as an out-of-band Slack
// message and resolves a returned vote against the specific pending decision id it answers (INV-12). Sender
// authentication against the approver set and credential/PII redaction are interface obligations shared by
// every backend (adapters/notifier.Authenticate / Redact). The HTTP transport is injectable (a Doer) so
// the oracle drives the real code path against a fake Slack API. The bot token is a secret reference,
// resolved per request, never a literal (INV-13).
//
// Provenance: [O] INV-12/INV-13/INV-19, spec/008.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/core/config"
)

// SourceType is the vendor slug this module serves.
const SourceType = "slack"

// Doer is the minimal HTTP contract; *http.Client satisfies it, and tests inject a fake Slack API.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Module is the Slack notifier-and-approval adapter. Construct with New.
type Module struct {
	baseURL        string
	tokenRef       config.SecretRef
	approvers      []string
	http           Doer
	channels       map[string]string // routed channel name (channelFor) -> the deployment's real channel id/name
	defaultChannel string            // where an unmapped route posts; empty ⇒ the computed #<prefix>-approvals name
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// WithChannels maps each routed channel name (as computed by channelFor, e.g. "#tg-approvals") to the
// deployment's REAL Slack channel — a channel id (`C0123…`, the reliable form) or a `#name` the bot has
// joined. Config-not-code: unless a deployment literally has a `#<project>-approvals` channel the bot is in,
// posting the computed name returns channel_not_found/not_in_channel and the approval poll never reaches
// humans. An unmapped route falls back to WithDefaultChannel, then to the computed name (today's behavior).
func WithChannels(channels map[string]string) Option {
	return func(m *Module) {
		if len(channels) > 0 {
			m.channels = channels
		}
	}
}

// WithDefaultChannel sets the channel every unmapped route posts to — the common single-approvals-channel
// deployment declares only this. Empty keeps the computed #<prefix>-approvals name (backward-compatible).
func WithDefaultChannel(channel string) Option {
	return func(m *Module) {
		if channel != "" {
			m.defaultChannel = channel
		}
	}
}

// resolveChannel maps a decision to the destination Slack channel: the per-route mapping if present, else the
// default channel, else the computed #<prefix>-approvals name (so an unconfigured deployment behaves as before).
func (m *Module) resolveChannel(decisionID string) string {
	name := channelFor(decisionID)
	if id, ok := m.channels[name]; ok {
		return id
	}
	if m.defaultChannel != "" {
		return m.defaultChannel
	}
	return name
}

// New builds a Slack module for the API base URL, a bot-token secret reference (e.g.
// "env:TG_SLACK_TOKEN"), and the approver set whose members may cast a vote.
func New(baseURL string, tokenRef config.SecretRef, approvers []string, opts ...Option) *Module {
	m := &Module{baseURL: strings.TrimRight(baseURL, "/"), tokenRef: tokenRef, approvers: approvers, http: http.DefaultClient}
	for _, o := range opts {
		o(m)
	}
	return m
}

// SourceType implements adapters/notifier.Notifier.
func (m *Module) SourceType() string { return SourceType }

// compile-time proof the module satisfies the stable notifier interface.
var _ notifier.Notifier = (*Module)(nil)

// channelFor resolves the destination channel from the decision id's project prefix (the part before the
// first '-', '#', or ':'), so an incident's approvals route to that project's channel. A prefixless id
// routes to a default channel.
func channelFor(decisionID string) string {
	prefix := decisionID
	if i := strings.IndexAny(decisionID, "-#:"); i > 0 {
		prefix = decisionID[:i]
	}
	if prefix == "" {
		prefix = "default"
	}
	return "#" + strings.ToLower(prefix) + "-approvals"
}

// apiResponse is the Slack Web API envelope every method returns: OK signals app-level success and Error
// carries the machine-readable failure code (e.g. "not_in_channel", "channel_not_found", "invalid_auth").
// Slack returns HTTP 200 even for these app-level failures, so OK — not the HTTP status — is the authoritative
// success signal. Gating on the status alone fails OPEN on the approval path (a dropped poll reported as
// delivered). This envelope is Slack-specific: matrix/mattermost correctly signal failure with a non-2xx status.
type apiResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// do issues an authenticated JSON request against the Slack Web API. The bot token is resolved from its
// secret reference at call time (INV-13); a non-2xx status is an error, and so is an HTTP 200 whose body
// reports ok:false (Slack's app-level failure envelope).
func (m *Module) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("slack: resolve token: %w", err)
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
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("slack: %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	// Slack returns HTTP 200 + {"ok":false,"error":"..."} for app-level failures (not_in_channel,
	// channel_not_found, invalid_auth, …). Decoding the envelope and demanding ok:true is what keeps
	// Notify from reporting a dropped approval poll as delivered (fail-closed on the approval path).
	var env apiResponse
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("slack: %s %s: undecodable response: %w", method, path, err)
	}
	if !env.OK {
		return nil, fmt.Errorf("slack: %s %s: %s", method, path, env.Error)
	}
	return out, nil
}

// postMessage is the chat.postMessage payload posted to a channel.
type postMessage struct {
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

// Notify renders a notice/poll and posts it to the resolved channel. The body is credential- and
// PII-redacted before it leaves the process (INV-19) — a redacted body is the only thing that reaches the
// channel.
func (m *Module) Notify(ctx context.Context, n notifier.Notice) error {
	if n.DecisionID == "" {
		return fmt.Errorf("slack: notice has no decision id")
	}
	body := postMessage{Channel: m.resolveChannel(n.DecisionID), Text: notifier.Redact(n.Body)}
	_, err := m.do(ctx, http.MethodPost, "/api/chat.postMessage", body)
	return err
}

// slackReply is the inbound reply this module parses to resolve a vote: the sender (a Slack user id) and
// the message text carrying the vote verb and the pending decision id.
type slackReply struct {
	User string `json:"user"`
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

// ResolveVote parses an inbound Slack reply, authenticates the sender against the approver set, and binds
// the vote to the pending decision id referenced in the text. An unauthenticated sender (not in the
// approver set) or a reply that cites no decision id is rejected — a vote is never counted for the wrong
// decision or from the wrong person (INV-12).
func (m *Module) ResolveVote(_ context.Context, raw []byte) (notifier.Vote, error) {
	var r slackReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return notifier.Vote{}, fmt.Errorf("slack: malformed reply: %w", err)
	}
	if !notifier.Authenticate(r.User, m.approvers) {
		return notifier.Vote{}, fmt.Errorf("slack: sender %q is not in the approver set", r.User)
	}
	decisionID, approve, ok := parseVote(r.Text)
	if !ok {
		return notifier.Vote{}, fmt.Errorf("slack: reply cites no pending decision id")
	}
	return notifier.Vote{DecisionID: decisionID, Sender: r.User, Approve: approve}, nil
}
