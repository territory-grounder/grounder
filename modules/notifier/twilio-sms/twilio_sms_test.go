package twiliosms

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/core/config"
)

type recordedReq struct {
	method, path, auth, contentType, body string
}

// fakeDoer records every request and returns a canned response (status + body). It drives the module's
// REAL request-building path against a fake Twilio REST endpoint, so the oracle can assert the exact
// vendor-correct request (verb, path, HTTP Basic auth, form encoding, body) without a live account.
type fakeDoer struct {
	reqs   []recordedReq
	status int    // 0 -> 201 Created, Twilio's success status for POST Messages.json
	resp   string // canned response body
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	var body string
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}
	f.reqs = append(f.reqs, recordedReq{
		method:      req.Method,
		path:        req.URL.Path,
		auth:        req.Header.Get("Authorization"),
		contentType: req.Header.Get("Content-Type"),
		body:        body,
	})
	st := f.status
	if st == 0 {
		st = 201 // Twilio returns 201 Created on message create, not 200.
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(f.resp)), Header: make(http.Header)}, nil
}

const (
	testSID   = "AC123"
	testToken = "s3cr3t"
	fromNum   = "+100"
	toNum     = "+200"
)

func newFixture(t *testing.T) (*Module, *fakeDoer) {
	t.Setenv("TG_TEST_TWILIO_TOKEN", testToken) // auth token supplied only via a secret reference (INV-13).
	f := &fakeDoer{}
	m := New("https://api.twilio.test/", testSID, fromNum, toNum, config.SecretRef("env:TG_TEST_TWILIO_TOKEN"), WithHTTPClient(f))
	return m, f
}

// wantBasicAuth is the vendor-correct Authorization header: HTTP Basic base64(AccountSID:auth_token). A
// bare "Bearer <token>" 401s on every Twilio REST request.
func wantBasicAuth(t *testing.T) string {
	t.Helper()
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(testSID+":"+testToken))
}

func TestNotifyPostsFormEncodedSMSWithBasicAuth(t *testing.T) {
	m, f := newFixture(t)
	err := m.Notify(context.Background(), notifier.Notice{
		DecisionID: "TG-9#restart",
		Body:       "POLL_PAUSE restart web01; password=hunter2 token=abcd1234 — contact ops@example.com",
	})
	if err != nil {
		t.Fatalf("Notify must succeed: %v", err)
	}
	if len(f.reqs) != 1 {
		t.Fatalf("Notify must send exactly one request, got %d", len(f.reqs))
	}
	r := f.reqs[0]

	// Vendor protocol: POST /2010-04-01/Accounts/{SID}/Messages.json.
	if r.method != http.MethodPost {
		t.Errorf("method = %q, want POST", r.method)
	}
	if want := "/2010-04-01/Accounts/" + testSID + "/Messages.json"; r.path != want {
		t.Errorf("path = %q, want %q", r.path, want)
	}

	// Vendor protocol: the message parameters are form-encoded (not JSON).
	if r.contentType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", r.contentType)
	}
	form, perr := url.ParseQuery(r.body)
	if perr != nil {
		t.Fatalf("request body must be form-encoded: %v (%q)", perr, r.body)
	}
	if form.Get("From") != fromNum {
		t.Errorf("From = %q, want %q", form.Get("From"), fromNum)
	}
	if form.Get("To") != toNum {
		t.Errorf("To = %q, want %q", form.Get("To"), toNum)
	}

	// Vendor protocol: HTTP Basic base64(SID:auth_token), never a bare Bearer.
	if r.auth != wantBasicAuth(t) {
		t.Errorf("Authorization = %q, want HTTP Basic base64(SID:token) %q", r.auth, wantBasicAuth(t))
	}
	if strings.HasPrefix(r.auth, "Bearer ") {
		t.Errorf("Authorization must be HTTP Basic, not a bare Bearer: %q", r.auth)
	}
	// The resolved token must never travel in cleartext outside the Basic credential.
	if strings.Contains(r.body, testToken) {
		t.Errorf("the auth token must not appear in the request body: %q", r.body)
	}

	// INV-19: the SMS body is credential/PII-redacted before it leaves the process.
	sms := form.Get("Body")
	for _, secret := range []string{"hunter2", "abcd1234", "ops@example.com"} {
		if strings.Contains(sms, secret) {
			t.Errorf("SMS body must redact %q, got %q", secret, sms)
		}
	}
	if !strings.Contains(sms, "[REDACTED]") {
		t.Errorf("SMS body must carry redaction markers, got %q", sms)
	}
}

func TestNotifyDedupsByDecisionID(t *testing.T) {
	m, f := newFixture(t)
	n := notifier.Notice{DecisionID: "TG-9#restart", Body: "page"}
	// page the SAME decision three times — only the first delivers (dedup by decision id).
	for i := 0; i < 3; i++ {
		if err := m.Notify(context.Background(), n); err != nil {
			t.Fatalf("Notify #%d must succeed: %v", i, err)
		}
	}
	if len(f.reqs) != 1 {
		t.Fatalf("the same decision must be paged exactly once (dedup), got %d sends", len(f.reqs))
	}
	// a DIFFERENT decision is a distinct page.
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "TG-10", Body: "page"}); err != nil {
		t.Fatalf("a new decision must page: %v", err)
	}
	if len(f.reqs) != 2 {
		t.Fatalf("a distinct decision must produce a second send, got %d", len(f.reqs))
	}
}

func TestNotifyEmptyDecisionIDRejected(t *testing.T) {
	m, f := newFixture(t)
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "", Body: "page"}); err == nil {
		t.Fatal("a notice with no decision id must be rejected (INV-12: a page must bind to a decision)")
	}
	if len(f.reqs) != 0 {
		t.Fatalf("a rejected notice must send nothing, got %d", len(f.reqs))
	}
}

func TestNotifyNon2xxIsErrorAndReleasesClaim(t *testing.T) {
	m, f := newFixture(t)
	f.status = 400
	f.resp = `{"code":21211,"message":"invalid 'To' number","status":400}`
	n := notifier.Notice{DecisionID: "TG-9#restart", Body: "page"}
	if err := m.Notify(context.Background(), n); err == nil {
		t.Fatal("a non-2xx Twilio response must be an error")
	}
	// The failed send released its dedup claim, so a retry of the SAME decision sends again (it was never
	// actually delivered) — a page is deduped only once it has really gone out.
	f.status = 201
	if err := m.Notify(context.Background(), n); err != nil {
		t.Fatalf("a retry after a failed page must send: %v", err)
	}
	if len(f.reqs) != 2 {
		t.Fatalf("expected the failed send + the retry = 2 sends, got %d", len(f.reqs))
	}
}

func TestNotifyRequiresResolvableSecret(t *testing.T) {
	f := &fakeDoer{}
	// A secret reference to an UNSET env var: the token cannot be resolved, so nothing may be sent —
	// the module must never fall back to an unauthenticated request (INV-13).
	m := New("https://api.twilio.test", testSID, fromNum, toNum, config.SecretRef("env:TG_TEST_TWILIO_MISSING"), WithHTTPClient(f))
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "TG-9", Body: "page"}); err == nil {
		t.Fatal("an unresolvable secret reference must fail the page")
	}
	if len(f.reqs) != 0 {
		t.Fatalf("no request may be sent when the secret cannot be resolved, got %d", len(f.reqs))
	}
}

func TestResolveVoteRejectsAllInboundSendOnly(t *testing.T) {
	m, _ := newFixture(t)
	// A well-formed inbound Twilio webhook (form-encoded From in E.164 + Body) still yields NO vote: Twilio
	// SMS is a one-way pager (REQ-807), so it exposes no approval channel and binds nothing back.
	inbound := url.Values{"From": {fromNum}, "Body": {"approve TG-9#restart"}}
	if _, err := m.ResolveVote(context.Background(), []byte(inbound.Encode())); err == nil {
		t.Fatal("a send-only pager must reject every inbound payload as a vote")
	}
	// an empty payload is likewise rejected, never silently accepted.
	if _, err := m.ResolveVote(context.Background(), nil); err == nil {
		t.Fatal("ResolveVote must reject an empty payload")
	}
}

func TestSourceType(t *testing.T) {
	m, _ := newFixture(t)
	if m.SourceType() != SourceType || SourceType != "twilio-sms" {
		t.Errorf("SourceType = %q, want twilio-sms", m.SourceType())
	}
}
