package healthchecks

// Oracles for the console's TEST button on the dead-man switch.
//
// These drive SelfTest over a REAL *http.Client against a REAL httptest server: the module is built by New
// WITHOUT WithHTTPClient, so the transport under test is the production one. The fakeDoer the ping oracles
// use (healthchecks_test.go) is the right seam for asserting URL construction; it is the wrong seam here,
// because half of what this probe claims to establish IS the transport — a closed port is an outcome a Doer
// fake can only imitate by returning an error somebody typed.
//
// The property asserted in EVERY case, passing and failing alike, is that the check was never pinged. A
// probe that resets the dead-man timer on its way to reporting anything has still reset the dead-man timer,
// and the alert it silenced was real.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// selfTestCheckID is a realistic check uuid AND a canary: it is the credential, so it must never appear in
// Summary, Detail or an error — those are the strings an operator pastes into a ticket.
const selfTestCheckID = "8f14e45f-ceea-467a-9f34-1c2d3e4f5a6b"

const selfTestCheckEnv = "TG_TEST_HC_SELFTEST_CHECK"

// pingHost is a stand-in Healthchecks ping endpoint that RECORDS every path it is asked for. The recording
// is not diagnostics: it is the only way to assert that pressing TEST did not register a heartbeat.
type pingHost struct {
	mu     sync.Mutex
	paths  []string
	status int    // 0 ⇒ 404, which is what a real ping host answers for a segment that is not a check
	body   string // 0 ⇒ "not found"
}

func (h *pingHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.paths = append(h.paths, r.Method+" "+r.URL.Path)
	status, body := h.status, h.body
	h.mu.Unlock()

	if status == 0 {
		status = http.StatusNotFound
	}
	if body == "" {
		body = "not found"
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// pinged reports whether anything addressed the CHECK — the mutation this probe must never perform.
func (h *pingHost) pinged() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.paths {
		if strings.Contains(p, selfTestCheckID) {
			return true
		}
	}
	return false
}

func (h *pingHost) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.paths...)
}

// newSelfTestFixture builds the module the way production does — a real HTTP client, a real base URL, the
// check id supplied ONLY as a secret reference — pointed at a live local server.
func newSelfTestFixture(t *testing.T, host *pingHost) (*Module, *httptest.Server) {
	t.Helper()
	t.Setenv(selfTestCheckEnv, selfTestCheckID)
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	return New(srv.URL, config.SecretRef("env:"+selfTestCheckEnv)), srv
}

// The pass and the failure classes, driven end to end over a real socket.
func TestSelfTestClassifiesPingHostAnswers(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		wantErr     bool
		wantSummary []string // substrings Summary must carry
		wantDetail  []string // substrings Detail must carry
	}{
		{
			// The HEALTHY case. A ping host correctly says "no such check" about a segment that cannot be
			// one, and the probe must read that as the pass it is rather than as a fault.
			name:        "404 is the healthy answer",
			status:      http.StatusNotFound,
			wantSummary: []string{"404", "NOT pinged", "env:" + selfTestCheckEnv},
			wantDetail:  []string{"does NOT prove the check id is valid"},
		},
		{
			name:        "401 means something in front of the ping host refuses callers",
			status:      http.StatusUnauthorized,
			wantErr:     true,
			wantSummary: []string{"401", "heartbeats would be rejected"},
			wantDetail:  []string{"reverse proxy", "refusing callers"},
		},
		{
			name:        "403 is the same class and is named as such",
			status:      http.StatusForbidden,
			wantErr:     true,
			wantSummary: []string{"403"},
			wantDetail:  []string{"IP allowlist"},
		},
		{
			name:        "5xx is a vendor-side fault, not a TG configuration one",
			status:      http.StatusBadGateway,
			wantErr:     true,
			wantSummary: []string{"502"},
			wantDetail:  []string{"Healthchecks-side fault"},
		},
		{
			// A host that answers 200 to a path that cannot be a check is answering 200 to everything. Not a
			// failure — TG cannot know what sits in front of an operator's ping host — but it is said aloud,
			// because this is what a captive portal looks like from here.
			name:        "200 to a non-ping path is suspicious and is reported",
			status:      http.StatusOK,
			wantSummary: []string{"200"},
			wantDetail:  []string{"portal", "answers 404 there"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := &pingHost{status: tc.status}
			m, srv := newSelfTestFixture(t, host)

			res, err := m.SelfTest(context.Background(), "alice@example")
			if tc.wantErr && err == nil {
				t.Fatalf("status %d must be an error, got a pass: %+v", tc.status, res)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("status %d must pass, got error: %v", tc.status, err)
			}
			for _, want := range tc.wantSummary {
				if !strings.Contains(res.Summary, want) {
					t.Errorf("Summary %q must name %q", res.Summary, want)
				}
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(res.Detail, want) {
					t.Errorf("Detail %q must name %q", res.Detail, want)
				}
			}
			// The observation must be the SERVED one: a probe that reported a status it did not receive
			// could not tell a healthy ping host from a broken one.
			if !strings.Contains(res.Summary, strconv.Itoa(tc.status)) {
				t.Errorf("Summary must report the status the server actually served (%d), got %q", tc.status, res.Summary)
			}
			// The base URL is reported so a pass against the WRONG ping host is still legible.
			if !strings.Contains(res.Summary, srv.URL) {
				t.Errorf("Summary must name the ping host it reached (%s), got %q", srv.URL, res.Summary)
			}

			// The safety property, asserted on every path: the dead-man timer was not reset.
			if host.pinged() {
				t.Fatalf("SelfTest PINGED the check — it reset the dead-man timer: %v", host.requests())
			}
			reqs := host.requests()
			if len(reqs) != 1 || reqs[0] != "GET /"+probeSegment {
				t.Errorf("the probe must make exactly one GET at the non-ping segment, got %v", reqs)
			}
			// Rule 5: the credential is never in text an operator pastes into a ticket.
			assertNoCheckIDLeak(t, res.Summary, res.Detail, errText(err))
		})
	}
}

// THE KILLING ORACLE.
//
// Every configured value is present and non-empty — a base URL, a check reference that resolves, a
// non-empty check id — and the endpoint rejects the request. A "the configuration is filled in" check would
// return a green here; that is precisely the mock this probe must not be, because a ping host behind a gate
// that refuses TG is a dead-man switch receiving nothing while the dialog says everything is fine.
func TestSelfTestFailsAgainstRejectingHostDespiteCompleteConfig(t *testing.T) {
	host := &pingHost{status: http.StatusUnauthorized}
	m, _ := newSelfTestFixture(t, host)

	// Everything a non-empty-values check would look at is populated.
	if strings.TrimSpace(m.baseURL) == "" {
		t.Fatal("fixture must have a non-empty ping host")
	}
	if strings.TrimSpace(string(m.checkRef)) == "" {
		t.Fatal("fixture must have a non-empty check reference")
	}
	if v, err := m.checkRef.Resolve(); err != nil || strings.TrimSpace(v) == "" {
		t.Fatalf("fixture must have a non-empty resolvable check id (v=%q err=%v)", v, err)
	}

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("a ping host that refuses the request MUST fail the test — a configured-values check would pass here: %+v", res)
	}
	if host.pinged() {
		t.Fatal("even while failing, the probe must not have pinged the check")
	}
}

// THE KILLING ORACLE, transport half: a closed port with the configuration fully populated.
func TestSelfTestFailsAgainstClosedPingHost(t *testing.T) {
	host := &pingHost{}
	m, srv := newSelfTestFixture(t, host)
	srv.Close() // the port is now closed: a real connection refusal, not a simulated one

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an unreachable ping host MUST be an error, not a pass: %+v", res)
	}
	if !strings.Contains(res.Summary, "could not be reached") {
		t.Errorf("Summary must say the ping host was unreachable, got %q", res.Summary)
	}
	if !strings.Contains(res.Detail, "nothing accepted a connection") && !strings.Contains(res.Detail, "could not be reached at") {
		t.Errorf("Detail must classify this as unreachable, got %q", res.Detail)
	}
	assertNoCheckIDLeak(t, res.Summary, res.Detail, errText(err))
}

// A reference that does not resolve is a TG-side secret fault, and must be named as one: an operator told
// "Healthchecks rejected it" would go and mint a new check for a problem that never left this process. No
// request may be issued at all.
func TestSelfTestUnresolvedCheckRefIsATGSideFaultAndSendsNothing(t *testing.T) {
	host := &pingHost{}
	srv := httptest.NewServer(host)
	defer srv.Close()
	m := New(srv.URL, config.SecretRef("env:TG_TEST_HC_SELFTEST_ABSENT"))

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an unresolved check reference must be an error: %+v", res)
	}
	if !strings.Contains(res.Detail, "TG-side secret problem") {
		t.Errorf("Detail must attribute this to TG's secret backend, not to Healthchecks, got %q", res.Detail)
	}
	if got := host.requests(); len(got) != 0 {
		t.Errorf("no request may be issued when the check id cannot be read, got %v", got)
	}
}

// A reference that resolves to an EMPTY value is worse than one that fails: nothing errors, and every
// heartbeat is sent to a URL that registers nowhere. It must fail closed, before any request.
func TestSelfTestEmptyCheckIDFailsClosed(t *testing.T) {
	t.Setenv(selfTestCheckEnv, "")
	host := &pingHost{}
	srv := httptest.NewServer(host)
	defer srv.Close()
	m := New(srv.URL, config.SecretRef("env:"+selfTestCheckEnv))

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an empty check id must be an error: %+v", res)
	}
	if !strings.Contains(res.Summary, "EMPTY") {
		t.Errorf("Summary must say the stored check id is empty, got %q", res.Summary)
	}
	if got := host.requests(); len(got) != 0 {
		t.Errorf("no request may be issued when the check id is empty, got %v", got)
	}
}

// A non-uuid check id is legitimate (self-hosted ping-key/slug addressing) but is also what a placeholder
// looks like. It must be reported as a note and must NOT fail the test.
func TestSelfTestReportsNonUUIDCheckIDWithoutFailing(t *testing.T) {
	t.Setenv(selfTestCheckEnv, "changeme")
	host := &pingHost{}
	srv := httptest.NewServer(host)
	defer srv.Close()
	m := New(srv.URL, config.SecretRef("env:"+selfTestCheckEnv))

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err != nil {
		t.Fatalf("a non-uuid check id must not fail the test: %v", err)
	}
	if !strings.Contains(res.Detail, "NOT uuid-shaped") {
		t.Errorf("Detail must report the unusual check-id shape, got %q", res.Detail)
	}
}

// The module with no ping host at all was never registered as a dead-man switch; saying so is more useful
// than a nil-deref inside the activity.
func TestSelfTestUnconfiguredModuleIsAnHonestFailure(t *testing.T) {
	m := New("", config.SecretRef("env:"+selfTestCheckEnv))
	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("a module with no ping host must fail the test: %+v", res)
	}
	if !strings.Contains(res.Summary, "no ping host") {
		t.Errorf("Summary must say there is no ping host, got %q", res.Summary)
	}
}

// The probe must honour the console's budget: a cancelled context returns rather than hanging on a spinner.
func TestSelfTestRespectsContext(t *testing.T) {
	host := &pingHost{}
	m, _ := newSelfTestFixture(t, host)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.SelfTest(ctx, "alice@example"); err == nil {
		t.Fatal("a cancelled context must abort the probe")
	}
	if host.pinged() {
		t.Fatal("a cancelled probe must not have pinged the check")
	}
}

func assertNoCheckIDLeak(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		if strings.Contains(s, selfTestCheckID) {
			t.Errorf("the check id is a credential and leaked into operator-facing text: %q", s)
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// A REFERENCE FIELD HOLDING THE SECRET ITSELF must not be echoed back.
//
// The check id pasted into TG_HEALTHCHECKS_CHECK_REF instead of a pointer to it is the one value a
// "reference" can hold that is NOT safe to display. core/config goes out of its way never to echo it (its
// error reports only the length), and the probe must not undo that: Summary, Detail and the error are the
// strings an operator pastes into a ticket, and the check id is a credential — whoever holds it can ping
// the check, and a check pinged by anything but TG makes a dead control plane read as alive.
func TestSelfTestDoesNotEchoASecretPastedIntoTheReferenceField(t *testing.T) {
	host := &pingHost{}
	srv := httptest.NewServer(host)
	defer srv.Close()
	m := New(srv.URL, config.SecretRef(selfTestCheckID)) // a bare literal, not a reference

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("a scheme-less reference cannot resolve and must fail the test: %+v", res)
	}
	assertNoCheckIDLeak(t, res.Summary, res.Detail, errText(err))
	if !strings.Contains(res.Summary, "no env:/file:/store: prefix") {
		t.Errorf("Summary must say the field holds a pasted secret rather than a reference, got %q", res.Summary)
	}
	if got := host.requests(); len(got) != 0 {
		t.Errorf("no request may be issued when the check id cannot be read, got %v", got)
	}
}
