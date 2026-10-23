// Package mattermost is the loadable Mattermost notifier-and-approval module (spec/008 REQ-808).
//
// It implements adapters/notifier.Notifier: it renders a governance decision as an out-of-band post and
// resolves a returned vote against the specific pending decision id it answers (INV-12). Sender
// authentication against the approver set and credential/PII redaction are interface obligations shared by
// every backend (adapters/notifier.Authenticate / Redact). The HTTP transport is injectable (a Doer) so
// the oracle drives the real code path against a fake server. The access token is a secret reference,
// resolved per request, never a literal (INV-13).
//
// Provenance: [O] INV-12/INV-13/INV-19, spec/008.
package mattermost

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
const SourceType = "mattermost"

// Doer is the minimal HTTP contract; *http.Client satisfies it, and tests inject a fake server.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Module is the Mattermost notifier-and-approval adapter. Construct with New.
type Module struct {
	baseURL   string
	tokenRef  config.SecretRef
	approvers []string
	// channels maps a channel name (as computed by channelFor, e.g. "tg-approvals") to the opaque 26-char
	// Mattermost channel ID that create-post requires. Mattermost's POST /api/v4/posts rejects a channel
	// *name* in channel_id with 400 (unlike Slack it has no name-tolerant create-post), so the opaque id is
	// injected at construction and resolved here — a name is never sent on the wire.
	channels map[string]string
	http     Doer
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// New builds a Mattermost module for a server base URL, an access-token secret reference (e.g.
// "env:TG_MM_TOKEN"), the approver set whose members may cast a vote, and a channel name->id map that
// resolves each routed channel name (see channelFor) to the opaque 26-char channel ID create-post requires.
func New(baseURL string, tokenRef config.SecretRef, approvers []string, channels map[string]string, opts ...Option) *Module {
	m := &Module{baseURL: strings.TrimRight(baseURL, "/"), tokenRef: tokenRef, approvers: approvers, channels: channels, http: http.DefaultClient}
	for _, o := range opts {
		o(m)
	}
	return m
}

// SourceType implements adapters/notifier.Notifier.
func (m *Module) SourceType() string { return SourceType }

// compile-time proof the module satisfies the stable notifier interface.
var _ notifier.Notifier = (*Module)(nil)

// channelFor resolves the destination channel *name* from the decision id's project prefix (the part
// before the first '-', '#', or ':'), so an incident's approvals route to that project's channel. A
// prefixless id routes to a default channel. The name is looked up to its opaque channel ID before posting
// (Notify); it is never sent to the API as-is (Mattermost create-post 400s a name).
func channelFor(decisionID string) string {
	prefix := decisionID
	if i := strings.IndexAny(decisionID, "-#:"); i > 0 {
		prefix = decisionID[:i]
	}
	if prefix == "" {
		prefix = "default"
	}
	return strings.ToLower(prefix) + "-approvals"
}

// do issues an authenticated JSON request against the Mattermost REST API. The access token is resolved
// from its secret reference at call time (INV-13); a non-2xx response is an error.
func (m *Module) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("mattermost: resolve token: %w", err)
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
		return nil, fmt.Errorf("mattermost: %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// postBody is the create-post payload sent to /api/v4/posts. channel_id MUST be the opaque 26-char channel
// ID (Mattermost rejects a channel name here with 400).
type postBody struct {
	ChannelID string `json:"channel_id"`
	Message   string `json:"message"`
}

// Notify renders a notice/poll and posts it to the resolved channel. The body is credential- and
// PII-redacted before it leaves the process (INV-19) — a redacted body is the only thing that reaches the
// channel.
func (m *Module) Notify(ctx context.Context, n notifier.Notice) error {
	if n.DecisionID == "" {
		return fmt.Errorf("mattermost: notice has no decision id")
	}
	name := channelFor(n.DecisionID)
	channelID, ok := m.channels[name]
	if !ok {
		// Fail closed: never fall back to posting a channel name (create-post 400s on a name), and never
		// leak a redacted decision body to an unintended destination.
		return fmt.Errorf("mattermost: no channel id configured for channel %q (decision %q)", name, n.DecisionID)
	}
	body := postBody{ChannelID: channelID, Message: notifier.Redact(n.Body)}
	_, err := m.do(ctx, http.MethodPost, "/api/v4/posts", body)
	return err
}

// inboundPost is the reply post this module parses to resolve a vote: the poster's user_name and the
// message body carrying the vote verb and the pending decision id.
type inboundPost struct {
	UserName string `json:"user_name"`
	Message  string `json:"message"`
}

// parseVote extracts the decision id and the approve/deny intent from a message body like
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

// ResolveVote parses an inbound Mattermost post, authenticates the poster against the approver set, and
// binds the vote to the pending decision id referenced in the message. An unauthenticated sender (not in
// the approver set) or a message that cites no decision id is rejected — a vote is never counted for the
// wrong decision or from the wrong person (INV-12).
func (m *Module) ResolveVote(_ context.Context, raw []byte) (notifier.Vote, error) {
	var p inboundPost
	if err := json.Unmarshal(raw, &p); err != nil {
		return notifier.Vote{}, fmt.Errorf("mattermost: malformed post: %w", err)
	}
	if !notifier.Authenticate(p.UserName, m.approvers) {
		return notifier.Vote{}, fmt.Errorf("mattermost: sender %q is not in the approver set", p.UserName)
	}
	decisionID, approve, ok := parseVote(p.Message)
	if !ok {
		return notifier.Vote{}, fmt.Errorf("mattermost: post cites no pending decision id")
	}
	return notifier.Vote{DecisionID: decisionID, Sender: p.UserName, Approve: approve}, nil
}
