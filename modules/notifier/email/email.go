// Package email is the reference SMTP notifier-and-approval module (spec/008 REQ-809).
//
// It implements adapters/notifier.Notifier: it renders a governance decision as an out-of-band email and
// resolves a returned vote against the specific pending decision id it answers (INV-12). Sender
// authentication against the approver set and credential/PII redaction are interface obligations shared by
// every backend (adapters/notifier.Authenticate / Redact). SMTP is not HTTP, so the injectable transport is
// a send func (a SendFunc), not a Doer — the oracle drives the real Notify path against a recording sender
// while production sends through net/smtp against the configured address.
//
// Provenance: [O] INV-12/INV-19, spec/008.
package email

import (
	"context"
	"encoding/json"
	"fmt"
	"net/smtp"
	"strings"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/core/config"
)

// SourceType is the vendor slug this module serves.
const SourceType = "email"

// SendFunc is the minimal SMTP send contract; net/smtp.SendMail satisfies its shape via defaultSender, and
// tests inject a recording sender. It replaces the HTTP Doer used by HTTP-transport backends because SMTP
// is not HTTP. The auth is passed per send (nil = the no-auth reference path) so the credential is resolved
// at send time, never held.
type SendFunc func(auth smtp.Auth, from string, to []string, msg []byte) error

// Module is the SMTP notifier-and-approval adapter. Construct with New.
type Module struct {
	smtpAddr    string
	host        string // the SMTP host (smtpAddr without its port) — the PLAIN-auth server identity
	from        string
	to          []string
	approvers   []string
	send        SendFunc
	username    string           // SMTP AUTH username; empty ⇒ the no-auth reference path
	passwordRef config.SecretRef // resolved to the SMTP password at send time (INV-13), never a literal
}

// Option configures a Module.
type Option func(*Module)

// WithSender injects the SMTP send func (a recording sender in tests, net/smtp.SendMail in production).
func WithSender(s SendFunc) Option { return func(m *Module) { m.send = s } }

// WithAuth makes the module authenticate to the SMTP relay with PLAIN auth: a username plus a password
// resolved from a secret reference at send time (INV-13, never a literal). Most real relays require this —
// without it the reference no-auth path is refused. net/smtp's PLAIN auth refuses to transmit credentials
// over an unencrypted, non-localhost connection, so this only sends over a STARTTLS-upgraded link (a safe
// default). An empty username keeps the no-auth path.
func WithAuth(username string, passwordRef config.SecretRef) Option {
	return func(m *Module) {
		m.username = username
		m.passwordRef = passwordRef
	}
}

// defaultSender sends through net/smtp against smtpAddr with the given auth (nil = no auth, the reference
// path). The credential, when present, was resolved by Notify immediately before the call.
func defaultSender(smtpAddr string) SendFunc {
	return func(auth smtp.Auth, from string, to []string, msg []byte) error {
		return smtp.SendMail(smtpAddr, auth, from, to, msg)
	}
}

// New builds an email module for an SMTP address ("host:port"), the envelope From, the recipient set, and
// the approver set whose members may cast a vote.
func New(smtpAddr, from string, to []string, approvers []string, opts ...Option) *Module {
	host := smtpAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	m := &Module{smtpAddr: smtpAddr, host: host, from: from, to: to, approvers: approvers}
	m.send = defaultSender(m.smtpAddr)
	for _, o := range opts {
		o(m)
	}
	return m
}

// SourceType implements adapters/notifier.Notifier.
func (m *Module) SourceType() string { return SourceType }

// compile-time proof the module satisfies the stable notifier interface.
var _ notifier.Notifier = (*Module)(nil)

// compose renders a simple RFC822 message: routing headers, a subject derived from the decision id, and the
// already-redacted body. The body is the only human-authored content; headers are the delivery envelope.
func compose(from string, to []string, decisionID, body string) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: Grounder decision " + decisionID + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	b.WriteString("\r\n")
	return []byte(b.String())
}

// Notify renders a notice/poll and sends it to the recipient set. The body is credential- and PII-redacted
// before it leaves the process (INV-19) — a redacted body is the only thing that reaches the channel.
func (m *Module) Notify(_ context.Context, n notifier.Notice) error {
	if n.DecisionID == "" {
		return fmt.Errorf("email: notice has no decision id")
	}
	// Resolve the SMTP credential at send time (never held). No username ⇒ the no-auth reference path.
	var auth smtp.Auth
	if m.username != "" {
		pw, err := m.passwordRef.Resolve()
		if err != nil {
			return fmt.Errorf("email: cannot resolve SMTP password: %w", err) // fail closed — do not send unauthenticated
		}
		auth = smtp.PlainAuth("", m.username, pw, m.host)
	}
	msg := compose(m.from, m.to, n.DecisionID, notifier.Redact(n.Body))
	return m.send(auth, m.from, m.to, msg)
}

// inboundEmail is the inbound reply this module parses to resolve a vote: the sender address and the body
// carrying the vote verb and the pending decision id.
type inboundEmail struct {
	From string `json:"from"`
	Body string `json:"body"`
}

// parseVote extracts the decision id and the approve/deny intent from a reply body like "approve
// <decisionID>" or "deny <decisionID>". A body that does not begin with a recognized verb and cite a
// decision id is not a vote.
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

// ResolveVote parses an inbound email, authenticates the sender against the approver set, and binds the vote
// to the pending decision id referenced in the body. An unauthenticated sender (not in the approver set) or
// a reply that cites no decision id is rejected — a vote is never counted for the wrong decision or from the
// wrong person (INV-12).
func (m *Module) ResolveVote(_ context.Context, raw []byte) (notifier.Vote, error) {
	var in inboundEmail
	if err := json.Unmarshal(raw, &in); err != nil {
		return notifier.Vote{}, fmt.Errorf("email: malformed inbound email: %w", err)
	}
	if !notifier.Authenticate(in.From, m.approvers) {
		return notifier.Vote{}, fmt.Errorf("email: sender %q is not in the approver set", in.From)
	}
	decisionID, approve, ok := parseVote(in.Body)
	if !ok {
		return notifier.Vote{}, fmt.Errorf("email: reply cites no pending decision id")
	}
	return notifier.Vote{DecisionID: decisionID, Sender: in.From, Approve: approve}, nil
}
