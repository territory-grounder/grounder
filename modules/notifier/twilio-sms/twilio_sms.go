// Package twiliosms is the loadable Twilio SMS notifier module (spec/008 REQ-807): a SEND-ONLY pager that
// delivers a governance decision as a plain SMS to an on-call number.
//
// It implements adapters/notifier.Notifier as a one-way channel — it posts a redacted page and accepts no
// votes (ResolveVote always errors). Credential/PII redaction is the interface obligation every backend
// inherits (adapters/notifier.Redact); the page carries only redacted plain text and never any
// command-executing content. Delivery is deduplicated by decision id: a decision is paged at most once, so
// a retried Notify for the same decision is a silent no-op. The HTTP transport is injectable (a Doer) so
// the oracle drives the real code path against a fake Twilio endpoint. The Twilio auth token is a secret
// reference, resolved per request, never a literal (INV-13).
//
// Provenance: [O] INV-13/INV-19, spec/008.
package twiliosms

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/core/config"
)

// SourceType is the vendor slug this module serves.
const SourceType = "twilio-sms"

// Doer is the minimal HTTP contract; *http.Client satisfies it, and tests inject a fake Twilio endpoint.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Module is the Twilio SMS send-only pager adapter. Construct with New.
type Module struct {
	apiBaseURL string
	accountSID string
	fromNumber string
	toNumber   string
	tokenRef   config.SecretRef
	http       Doer

	mu    sync.Mutex      // guards paged
	paged map[string]bool // decision ids already delivered — dedup so each decision pages at most once
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// New builds a Twilio SMS module for a REST API base URL, the account SID, the sender and recipient E.164
// numbers, and an auth-token secret reference (e.g. "env:TG_TWILIO_TOKEN").
func New(apiBaseURL, accountSID, fromNumber, toNumber string, tokenRef config.SecretRef, opts ...Option) *Module {
	m := &Module{
		apiBaseURL: strings.TrimRight(apiBaseURL, "/"),
		accountSID: accountSID,
		fromNumber: fromNumber,
		toNumber:   toNumber,
		tokenRef:   tokenRef,
		http:       http.DefaultClient,
		paged:      make(map[string]bool),
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// SourceType implements adapters/notifier.Notifier.
func (m *Module) SourceType() string { return SourceType }

// compile-time proof the module satisfies the stable notifier interface.
var _ notifier.Notifier = (*Module)(nil)

// do issues an authenticated form POST against the Twilio REST API. The auth token is resolved from its
// secret reference at call time (INV-13) and paired with the account SID as HTTP Basic credentials; a
// non-2xx response is an error.
func (m *Module) do(ctx context.Context, path string, form url.Values) error {
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return fmt.Errorf("twilio-sms: resolve token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.apiBaseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(m.accountSID, token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("twilio-sms: POST %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return nil
}

// Notify pages the decision to the configured number. Delivery is deduplicated by decision id: if this
// decision was already paged, Notify returns nil WITHOUT sending. Otherwise the body is credential- and
// PII-redacted before it leaves the process (INV-19) and posted as plain form-encoded text — the page
// carries no command-executing content, only redacted text.
func (m *Module) Notify(ctx context.Context, n notifier.Notice) error {
	if n.DecisionID == "" {
		return fmt.Errorf("twilio-sms: notice has no decision id")
	}
	// Claim the decision id under the lock so a retry or a concurrent call cannot page it twice.
	m.mu.Lock()
	if m.paged[n.DecisionID] {
		m.mu.Unlock()
		return nil // already paged — dedup
	}
	m.paged[n.DecisionID] = true
	m.mu.Unlock()

	form := url.Values{}
	form.Set("To", m.toNumber)
	form.Set("From", m.fromNumber)
	form.Set("Body", notifier.Redact(n.Body))
	path := "/2010-04-01/Accounts/" + url.PathEscape(m.accountSID) + "/Messages.json"
	if err := m.do(ctx, path, form); err != nil {
		// The page did not go out; release the claim so a later Notify can retry this decision.
		m.mu.Lock()
		delete(m.paged, n.DecisionID)
		m.mu.Unlock()
		return err
	}
	return nil
}

// ResolveVote rejects every inbound payload: Twilio SMS is a one-way pager with no approval channel, so no
// reply can ever be a vote.
func (m *Module) ResolveVote(context.Context, []byte) (notifier.Vote, error) {
	return notifier.Vote{}, fmt.Errorf("twilio-sms is a send-only channel; it accepts no votes")
}
