// Package matrix is the loadable Matrix notifier-and-approval module (spec/008 REQ-806, T-008-8) and the
// family lead for the notifier surface.
//
// It implements adapters/notifier.Notifier: it renders a governance decision as an out-of-band message
// and resolves a returned vote against the specific pending decision id it answers (INV-12). Sender
// authentication against the approver set and credential/PII redaction are interface obligations shared by
// every backend (adapters/notifier.Authenticate / Redact). The HTTP transport is injectable (a Doer) so
// the oracle drives the real code path against a fake homeserver. The access token is a secret reference,
// resolved per request, never a literal (INV-13).
//
// Provenance: [O] INV-12/INV-13/INV-19, spec/008.
package matrix

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
const SourceType = "matrix"

// Doer is the minimal HTTP contract; *http.Client satisfies it, and tests inject a fake homeserver.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Module is the Matrix notifier-and-approval adapter. Construct with New.
type Module struct {
	homeserver  string
	tokenRef    config.SecretRef
	approvers   []string
	http        Doer
	rooms       map[string]string // routed room name (roomFor) -> the deployment's real room id/alias
	defaultRoom string            // where an unmapped route posts; empty ⇒ the computed #<prefix>-approvals alias
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// WithRooms maps each routed room name (as computed by roomFor, e.g. "#tg-approvals") to the deployment's
// REAL Matrix room — a room id (`!abc:server`) or a proper alias (`#room:homeserver`). Config-not-code: the
// computed `#<project>-approvals` is NOT a valid alias (it has no `:homeserver`), so without a mapping the
// send route resolves nothing and the approval poll is undeliverable. An unmapped route falls back to
// WithDefaultRoom, then to the computed name (today's behavior).
func WithRooms(rooms map[string]string) Option {
	return func(m *Module) {
		if len(rooms) > 0 {
			m.rooms = rooms
		}
	}
}

// WithDefaultRoom sets the room every unmapped route posts to — the common single-approvals-room deployment
// declares only this. Empty keeps the computed #<prefix>-approvals name (backward-compatible).
func WithDefaultRoom(room string) Option {
	return func(m *Module) {
		if room != "" {
			m.defaultRoom = room
		}
	}
}

// resolveRoom maps a decision to the destination Matrix room: the per-route mapping if present, else the
// default room, else the computed #<prefix>-approvals alias (so an unconfigured deployment behaves as before).
func (m *Module) resolveRoom(decisionID string) string {
	name := roomFor(decisionID)
	if id, ok := m.rooms[name]; ok {
		return id
	}
	if m.defaultRoom != "" {
		return m.defaultRoom
	}
	return name
}

// New builds a Matrix module for a homeserver base URL, an access-token secret reference (e.g.
// "env:MATRIX_TOKEN"), and the approver set whose members may cast a vote.
func New(homeserver string, tokenRef config.SecretRef, approvers []string, opts ...Option) *Module {
	m := &Module{homeserver: strings.TrimRight(homeserver, "/"), tokenRef: tokenRef, approvers: approvers, http: http.DefaultClient}
	for _, o := range opts {
		o(m)
	}
	return m
}

// SourceType implements adapters/notifier.Notifier.
func (m *Module) SourceType() string { return SourceType }

// compile-time proof the module satisfies the stable notifier interface.
var _ notifier.Notifier = (*Module)(nil)

// roomFor resolves the destination room from the decision id's project prefix (the part before the first
// '-' or '#'), so an incident's approvals route to that project's room. A prefixless id routes to a
// default room.
func roomFor(decisionID string) string {
	prefix := decisionID
	if i := strings.IndexAny(decisionID, "-#:"); i > 0 {
		prefix = decisionID[:i]
	}
	if prefix == "" {
		prefix = "default"
	}
	return "#" + strings.ToLower(prefix) + "-approvals"
}

// do issues an authenticated JSON request against the Matrix client-server API. The access token is
// resolved from its secret reference at call time (INV-13); a non-2xx response is an error.
func (m *Module) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("matrix: resolve token: %w", err)
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.homeserver+path, rdr)
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
		return nil, fmt.Errorf("matrix: %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// messageBody is the m.room.message content posted to a room.
type messageBody struct {
	MsgType string `json:"msgtype"`
	Body    string `json:"body"`
}

// Notify renders a notice/poll and posts it to the resolved room. The body is credential- and PII-redacted
// before it leaves the process (INV-19) — a redacted body is the only thing that reaches the channel.
func (m *Module) Notify(ctx context.Context, n notifier.Notice) error {
	if n.DecisionID == "" {
		return fmt.Errorf("matrix: notice has no decision id")
	}
	// The room alias begins with '#'; it MUST be path-escaped or it would be parsed as a URL fragment.
	room := url.PathEscape(m.resolveRoom(n.DecisionID))
	body := messageBody{MsgType: "m.text", Body: notifier.Redact(n.Body)}
	_, err := m.do(ctx, http.MethodPost, "/_matrix/client/v3/rooms/"+room+"/send/m.room.message", body)
	return err
}
