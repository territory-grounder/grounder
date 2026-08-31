package email

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/mail"
	"net/smtp"
	"strings"
	"testing"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/core/config"
)

// recordedSend captures one call to the injected SMTP send seam: the SMTP envelope (MAIL FROM / RCPT TO)
// and the raw RFC822 message bytes. It is the SMTP analogue of the sibling modules' recording fakeDoer —
// it lets the oracle drive the module's REAL compose path and assert the wire message a live MTA would
// receive, without a live SMTP server.
type recordedSend struct {
	auth smtp.Auth
	from string
	to   []string
	msg  []byte
}

type recordingSender struct {
	sends []recordedSend
}

func (r *recordingSender) send(auth smtp.Auth, from string, to []string, msg []byte) error {
	r.sends = append(r.sends, recordedSend{
		auth: auth,
		from: from,
		to:   append([]string(nil), to...),
		msg:  append([]byte(nil), msg...),
	})
	return nil
}

func (r *recordingSender) last() recordedSend { return r.sends[len(r.sends)-1] }

// newFixture builds a module wired to a recording send seam. The reference SMTP path carries no auth (see
// defaultSender), so there is no secret to inject here — a backend that needed an SMTP password would
// resolve it from a config.SecretRef at send time, never a literal.
func newFixture() (*Module, *recordingSender) {
	r := &recordingSender{}
	m := New(
		"smtp.test:25",
		"grounder@example.com",
		[]string{"ops@example.com", "sre@example.com"},
		[]string{"oncall@example.com"},
		WithSender(r.send),
	)
	return m, r
}

func TestSourceType(t *testing.T) {
	m, _ := newFixture()
	if got := m.SourceType(); got != "email" {
		t.Errorf("SourceType() = %q, want email", got)
	}
}

// TestNotifyComposesRFC822WithSubjectTagAndRedactedBody is the core protocol assertion: Notify must build a
// valid RFC822 message whose Subject carries the decision id verbatim (the round-trip carrier that lets an
// inbound reply bind back, INV-12) and whose body is credential- and PII-redacted before it leaves the
// process (INV-19). The message is re-parsed with net/mail — the exact library a real MTA/inbound parser
// uses — so header structure, addressing, and the header/body split are asserted against the real protocol,
// not against my own assumptions.
func TestNotifyComposesRFC822WithSubjectTagAndRedactedBody(t *testing.T) {
	m, r := newFixture()
	err := m.Notify(context.Background(), notifier.Notice{
		DecisionID: "TG-9#restart",
		Body:       "restart web01; password=hunter2 — page oncall@example.com if it flaps",
		Approval:   true,
	})
	if err != nil {
		t.Fatalf("Notify must succeed: %v", err)
	}
	if len(r.sends) != 1 {
		t.Fatalf("Notify must send exactly one message, got %d", len(r.sends))
	}
	s := r.last()

	// SMTP envelope: MAIL FROM is the configured From, RCPT TO is the configured recipient set.
	if s.from != "grounder@example.com" {
		t.Errorf("envelope from = %q, want grounder@example.com", s.from)
	}
	if strings.Join(s.to, ",") != "ops@example.com,sre@example.com" {
		t.Errorf("envelope recipients = %v, want [ops@example.com sre@example.com]", s.to)
	}

	// CRLF line endings are mandatory in RFC822 on the wire; a bare-LF message is rejected by strict MTAs.
	if !strings.Contains(string(s.msg), "\r\n") {
		t.Errorf("message must use CRLF line endings, got %q", string(s.msg))
	}

	msg, err := mail.ReadMessage(bytes.NewReader(s.msg))
	if err != nil {
		t.Fatalf("composed message must be valid RFC822 (net/mail must parse it): %v", err)
	}

	// From / To headers must parse as real addresses (not just literal strings).
	fromAddrs, err := msg.Header.AddressList("From")
	if err != nil || len(fromAddrs) != 1 || fromAddrs[0].Address != "grounder@example.com" {
		t.Errorf("From header must parse to grounder@example.com, got %v err=%v", fromAddrs, err)
	}
	toAddrs, err := msg.Header.AddressList("To")
	if err != nil || len(toAddrs) != 2 {
		t.Fatalf("To header must parse to two recipients, got %v err=%v", toAddrs, err)
	}
	if toAddrs[0].Address != "ops@example.com" || toAddrs[1].Address != "sre@example.com" {
		t.Errorf("To header addresses wrong: %v", toAddrs)
	}

	// The decision id must round-trip into the Subject verbatim — this is the carrier that lets a reply
	// bind back to exactly this decision (INV-12).
	if got := msg.Header.Get("Subject"); got != "Grounder decision TG-9#restart" {
		t.Errorf("Subject = %q, want it to carry the decision id: Grounder decision TG-9#restart", got)
	}

	// MIME framing so a mail client renders the body as text, not an attachment.
	if got := msg.Header.Get("Mime-Version"); got != "1.0" {
		t.Errorf("MIME-Version = %q, want 1.0", got)
	}
	if got := msg.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") || !strings.Contains(got, "charset=utf-8") {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", got)
	}

	// The body — and only the body — is what the redaction obligation guards (INV-19).
	rawBody, err := io.ReadAll(msg.Body)
	if err != nil {
		t.Fatalf("reading parsed body: %v", err)
	}
	body := string(rawBody)
	for _, secret := range []string{"hunter2", "password=hunter2", "oncall@example.com"} {
		if strings.Contains(body, secret) {
			t.Errorf("posted body must redact %q, got %q", secret, body)
		}
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("posted body must carry redaction markers, got %q", body)
	}
}

// TestWithAuthResolvesPasswordAndFailsClosed proves SMTP AUTH is config-not-code and fail-closed: with
// WithAuth and a resolvable secret ref, Notify resolves the password at send time and hands PLAIN auth to
// the sender; a no-auth module hands nil; and if the password ref cannot resolve, Notify errors and sends
// NOTHING (never an unauthenticated fallback). Without configurable auth, an auth-requiring relay refuses
// every approval poll.
func TestWithAuthResolvesPasswordAndFailsClosed(t *testing.T) {
	// No auth configured ⇒ the sender receives nil auth (the reference path).
	m, r := newFixture()
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "TG-1#x", Body: "y"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if r.last().auth != nil {
		t.Error("a no-auth module must hand nil auth to the sender")
	}

	// Auth configured with a resolvable secret ⇒ the sender receives a non-nil PLAIN auth.
	t.Setenv("TG_TEST_SMTP_PW", "s3cr3t")
	r2 := &recordingSender{}
	auth := New("smtp.test:587", "grounder@example.com", []string{"ops@example.com"}, []string{"oncall@example.com"},
		WithSender(r2.send), WithAuth("grounder@example.com", config.SecretRef("env:TG_TEST_SMTP_PW")))
	if err := auth.Notify(context.Background(), notifier.Notice{DecisionID: "TG-2#x", Body: "y"}); err != nil {
		t.Fatalf("Notify with auth: %v", err)
	}
	if r2.last().auth == nil {
		t.Error("an auth-configured module must hand a non-nil PLAIN auth to the sender")
	}

	// Auth configured but the secret cannot resolve ⇒ fail closed: Notify errors, nothing is sent.
	r3 := &recordingSender{}
	broken := New("smtp.test:587", "grounder@example.com", []string{"ops@example.com"}, []string{"oncall@example.com"},
		WithSender(r3.send), WithAuth("grounder@example.com", config.SecretRef("env:TG_TEST_SMTP_PW_UNSET")))
	if err := broken.Notify(context.Background(), notifier.Notice{DecisionID: "TG-3#x", Body: "y"}); err == nil {
		t.Error("an unresolvable SMTP password must fail closed, not send unauthenticated")
	}
	if len(r3.sends) != 0 {
		t.Errorf("nothing may be sent when the credential cannot be resolved, got %d sends", len(r3.sends))
	}
}

// TestNotifyRejectsEmptyDecisionID: a notice with no decision id cannot bind a future vote, so it is
// rejected and nothing is put on the wire (INV-12 fail-closed).
func TestNotifyRejectsEmptyDecisionID(t *testing.T) {
	m, r := newFixture()
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "", Body: "restart web01"}); err == nil {
		t.Fatal("a notice with no decision id must be rejected")
	}
	if len(r.sends) != 0 {
		t.Errorf("nothing may be sent for an unbindable notice, got %d sends", len(r.sends))
	}
}

// TestResolveVoteBindsToDecisionFromApprover: an approver's reply binds to exactly the decision id its body
// cites, with the sender and approve/deny intent recovered (INV-12).
func TestResolveVoteBindsToDecisionFromApprover(t *testing.T) {
	m, _ := newFixture()
	raw := []byte(`{"from":"oncall@example.com","body":"approve TG-9#restart"}`)
	v, err := m.ResolveVote(context.Background(), raw)
	if err != nil {
		t.Fatalf("an approver vote must resolve: %v", err)
	}
	if v.DecisionID != "TG-9#restart" {
		t.Errorf("vote must bind to the cited decision id, got %q", v.DecisionID)
	}
	if v.Sender != "oncall@example.com" || !v.Approve {
		t.Errorf("vote sender/intent wrong: %+v", v)
	}

	// A deny verb must flip the intent while still binding to the same decision.
	deny, err := m.ResolveVote(context.Background(), []byte(`{"from":"oncall@example.com","body":"deny TG-9#restart"}`))
	if err != nil {
		t.Fatalf("a deny vote must resolve: %v", err)
	}
	if deny.DecisionID != "TG-9#restart" || deny.Approve {
		t.Errorf("deny must bind to the decision with Approve=false, got %+v", deny)
	}
}

// TestResolveVoteRejectsNonApproverUnboundAndMalformed enforces the two INV-12 gates and input hygiene: the
// sender must authenticate against the approver set, the reply must cite a decision id, and malformed JSON
// is rejected rather than silently dropped.
func TestResolveVoteRejectsNonApproverUnboundAndMalformed(t *testing.T) {
	m, _ := newFixture()

	// From not in the approver set → rejected (sender authentication).
	if _, err := m.ResolveVote(context.Background(), []byte(`{"from":"intruder@evil.com","body":"approve TG-9#restart"}`)); err == nil {
		t.Error("a vote from a non-approver must be rejected")
	}
	// Empty sender → fail closed.
	if _, err := m.ResolveVote(context.Background(), []byte(`{"from":"","body":"approve TG-9#restart"}`)); err == nil {
		t.Error("an empty sender must be rejected (fail closed)")
	}
	// A reply that cites no decision id → rejected (no misattribution).
	if _, err := m.ResolveVote(context.Background(), []byte(`{"from":"oncall@example.com","body":"approve"}`)); err == nil {
		t.Error("a reply citing no decision id must be rejected")
	}
	// A body with no recognized verb → not a vote.
	if _, err := m.ResolveVote(context.Background(), []byte(`{"from":"oncall@example.com","body":"maybe TG-9#restart"}`)); err == nil {
		t.Error("a reply with no recognized vote verb must be rejected")
	}
	// Malformed JSON → error.
	if _, err := m.ResolveVote(context.Background(), []byte(`{not json`)); err == nil {
		t.Error("malformed inbound email must be rejected")
	}
}

// TestDecisionIDRoundTripsFromSubjectToReply proves the end-to-end INV-12 round-trip through the real SMTP
// carrier: the id the module stamps into the outbound Subject is exactly the id its inbound parser recovers
// from an approver's reply. No global cursor, no cross-thread misattribution.
func TestDecisionIDRoundTripsFromSubjectToReply(t *testing.T) {
	m, r := newFixture()
	const id = "TG-9#restart"

	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: id, Body: "restart web01"}); err != nil {
		t.Fatalf("Notify must succeed: %v", err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(r.last().msg))
	if err != nil {
		t.Fatalf("composed message must parse: %v", err)
	}
	subject := msg.Header.Get("Subject")
	if !strings.Contains(subject, id) {
		t.Fatalf("Subject must carry the decision id so a reply can bind back, got %q", subject)
	}

	// The approver replies quoting the decision id the subject surfaced.
	reply := fmt.Sprintf(`{"from":"oncall@example.com","body":"approve %s"}`, id)
	v, err := m.ResolveVote(context.Background(), []byte(reply))
	if err != nil {
		t.Fatalf("the approver reply must resolve: %v", err)
	}
	if v.DecisionID != id {
		t.Errorf("the vote must bind to the same id the subject carried (INV-12): got %q want %q", v.DecisionID, id)
	}
}
