package openobserve

// Oracles for the console's TEST button on the OpenObserve module.
//
// These drive SelfTest over a REAL *http.Client against a REAL httptest server: the module is built by New
// WITHOUT WithHTTPClient, so the transport under test is the production one. The fakeDoer the export oracles
// use (openobserve_test.go) is the right seam for asserting OTLP body shapes; it is the wrong seam here,
// because half of what this probe claims to establish IS the transport — a closed port is an outcome a Doer
// fake can only imitate by returning an error somebody typed.
//
// The property asserted in EVERY case, passing and failing alike, is that nothing was ingested. A probe that
// writes a junk record on its way to reporting an error has still written a junk record into the operator's
// metrics store.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

const selfTestTokenEnv = "TG_TEST_OO_SELFTEST_TOKEN"

// The base64(user:password) credential OpenObserve issues — and a canary: it must never appear in Summary,
// Detail or an error, because those are the strings an operator pastes into a ticket.
const selfTestToken = "Y2FuYXJ5QGV4YW1wbGUuY29tOkNBTkFSWS1wYXNzd29yZA=="

// observeAPI is a stand-in OpenObserve that serves the one GET the probe makes and RECORDS every request.
// The recording is not diagnostics: it is the only way to assert that pressing TEST ingested nothing.
type observeAPI struct {
	mu   sync.Mutex
	hits []string

	status     int      // non-zero: force this status on the stream list
	body       string   // non-empty: serve this body verbatim instead of the stream list
	streams    []string // stream names the org holds
	sawIngest  bool     // set if anything ever reached an ingest route
	sawNonRead bool     // set if any request arrived with a method other than GET
}

func (s *observeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits = append(s.hits, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	if r.Method != http.MethodGet {
		s.sawNonRead = true
	}
	if strings.Contains(r.URL.Path, "/v1/logs") || strings.Contains(r.URL.Path, "/v1/traces") {
		s.sawIngest = true
	}
	status, body, streams := s.status, s.body, s.streams
	s.mu.Unlock()

	// The credential is checked for real: a probe that never presented the token would pass against a
	// revoked one, which is one of the three things TEST exists to rule out. The expected header is HTTP
	// Basic with the token verbatim — a Bearer would 401 here exactly as it does on a real OpenObserve.
	if r.Header.Get("Authorization") != "Basic "+selfTestToken {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"message":"Unauthorized Access"}`))
		return
	}
	if status != 0 && (status < 200 || status >= 300) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"code":` + strconv.Itoa(status) + `,"message":"served by the oracle"}`))
		return
	}
	if body != "" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
		return
	}
	list := make([]map[string]any, 0, len(streams))
	for _, name := range streams {
		list = append(list, map[string]any{"name": name, "storage_type": "disk", "stream_type": "logs"})
	}
	out, _ := json.Marshal(map[string]any{"list": list})
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (s *observeAPI) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.hits...)
}

func (s *observeAPI) ingested() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sawIngest || s.sawNonRead
}

// newSelfTestFixture builds the module the way production does — a real HTTP client, the token supplied ONLY
// as a secret reference, and an endpoint carrying the /api/<org> prefix the OTLP ingest path requires.
func newSelfTestFixture(t *testing.T, api *observeAPI) (*Module, string) {
	t.Helper()
	t.Setenv(selfTestTokenEnv, selfTestToken)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	endpoint := srv.URL + "/api/default"
	return New(endpoint, config.SecretRef("env:"+selfTestTokenEnv)), endpoint
}

// The pass and the failure classes, driven end to end over a real socket.
func TestSelfTestClassifiesOpenObserveAnswers(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		streams     []string
		wantErr     bool
		wantSummary []string
		wantDetail  []string
	}{
		{
			// The observation is the whole point: the stream names are what tell an operator whether they
			// are looking at the org their dashboards query.
			name:        "the streams the credential can see are named back",
			streams:     []string{"tg_worker", "k8s", "journald"},
			wantSummary: []string{"3 log stream(s) visible", "tg_worker", "k8s", "journald", "nothing was ingested"},
			wantDetail:  []string{"does not prove ingest is PERMITTED"},
		},
		{
			name:        "401 names the credential problem and the raw-API-key mistake",
			status:      http.StatusUnauthorized,
			wantErr:     true,
			wantSummary: []string{"401"},
			wantDetail:  []string{"rejected the credential", "RAW API key"},
		},
		{
			name:        "403 separates an accepted credential from a permitted one",
			status:      http.StatusForbidden,
			wantErr:     true,
			wantSummary: []string{"403"},
			wantDetail:  []string{"authenticated but OpenObserve refused the read", "ingest-only service account"},
		},
		{
			// The diagnosis that pays for this whole probe: the same wrong prefix silently 404s every export.
			name:        "404 points at the missing /api/<org> prefix and says ingest fails the same way",
			status:      http.StatusNotFound,
			wantErr:     true,
			wantSummary: []string{"404"},
			wantDetail:  []string{"/api/<org> prefix", "404s every export"},
		},
		{
			name:        "5xx is a vendor-side fault, not a TG configuration one",
			status:      http.StatusBadGateway,
			wantErr:     true,
			wantSummary: []string{"502"},
			wantDetail:  []string{"vendor-side fault"},
		},
		{
			// The wrong-instance case that looks like success to anything checking only the status code.
			name:        "a 200 that is not a stream list is a failure",
			body:        "<html><body>Sign in to continue</body></html>",
			wantErr:     true,
			wantSummary: []string{"answered 200 but not with an OpenObserve stream list"},
			wantDetail:  []string{"not the OpenObserve API"},
		},
		{
			// A real pass with a real caveat: this is what a correctly configured TG looks like before its
			// first export, and the caveat says the org could not be confirmed from its contents.
			name:        "an empty org passes but says the observation is missing",
			streams:     []string{},
			wantSummary: []string{"holds NO log streams yet"},
			wantDetail:  []string{"cannot confirm WHICH org"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &observeAPI{status: tc.status, body: tc.body, streams: tc.streams}
			m, endpoint := newSelfTestFixture(t, api)

			res, err := m.SelfTest(context.Background(), "alice@example")
			if tc.wantErr && err == nil {
				t.Fatalf("this case must be an error, got a pass: %+v", res)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("this case must pass, got error: %v", err)
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
			if tc.status != 0 && !strings.Contains(res.Summary, strconv.Itoa(tc.status)) {
				t.Errorf("Summary must report the status the server actually served (%d), got %q", tc.status, res.Summary)
			}
			// The endpoint is reported so a pass against the WRONG instance or org is still legible.
			if !strings.Contains(res.Summary, endpoint) {
				t.Errorf("Summary must name the endpoint it reached (%s), got %q", endpoint, res.Summary)
			}

			// The safety property, asserted on every path.
			if api.ingested() {
				t.Fatalf("SelfTest wrote to OpenObserve — requests: %v", api.requests())
			}
			reqs := api.requests()
			if len(reqs) != 1 || reqs[0] != "GET /api/default/streams?fetchSchema=false&type=logs" {
				t.Errorf("the probe must make exactly one bounded authenticated GET of the org's stream list, got %v", reqs)
			}
			assertNoTokenLeak(t, res.Summary, res.Detail, errText(err))
		})
	}
}

// The response must be READ, not assumed: the reported stream count and names come from the served payload,
// so a probe that printed a fixed sentence could not tell one OpenObserve org from another.
func TestSelfTestReportsTheServedStreamsNotAFixedSentence(t *testing.T) {
	api := &observeAPI{streams: []string{"site_aa_logs"}}
	m, _ := newSelfTestFixture(t, api)
	res, err := m.SelfTest(context.Background(), "alice@example")
	if err != nil {
		t.Fatalf("self-test must pass: %v", err)
	}
	if !strings.Contains(res.Summary, "site_aa_logs") || !strings.Contains(res.Summary, "1 log stream(s)") {
		t.Fatalf("Summary must reflect the served payload, got %q", res.Summary)
	}
	// A different estate must produce a different Summary.
	api2 := &observeAPI{streams: []string{"site_bb_logs", "site_cc_logs"}}
	m2, _ := newSelfTestFixture(t, api2)
	res2, err := m2.SelfTest(context.Background(), "alice@example")
	if err != nil {
		t.Fatalf("self-test must pass: %v", err)
	}
	if res2.Summary == res.Summary {
		t.Fatal("two different OpenObserve orgs must not produce the same Summary — the observation is not being read")
	}
}

// The Summary must stay one legible line even against an estate with hundreds of streams.
func TestSelfTestBoundsTheNamedStreams(t *testing.T) {
	many := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		many = append(many, "stream_"+strconv.Itoa(i))
	}
	api := &observeAPI{streams: many}
	m, _ := newSelfTestFixture(t, api)
	res, err := m.SelfTest(context.Background(), "alice@example")
	if err != nil {
		t.Fatalf("self-test must pass: %v", err)
	}
	if !strings.Contains(res.Summary, "40 log stream(s) visible") {
		t.Errorf("Summary must report the true total, got %q", res.Summary)
	}
	if strings.Contains(res.Summary, "stream_"+strconv.Itoa(maxNamedStreams)) {
		t.Errorf("Summary must name at most %d streams, got %q", maxNamedStreams, res.Summary)
	}
	if !strings.Contains(res.Detail, "further stream(s) exist") {
		t.Errorf("Detail must say the list was truncated, got %q", res.Detail)
	}
}

// THE KILLING ORACLE.
//
// Every configured value is present and non-empty — an endpoint, a token reference that resolves to a
// non-empty credential — and OpenObserve rejects it. A "the configuration is filled in" check would return a
// green here; that is precisely the mock this probe must not be, because a revoked or wrong-format token is
// one of the three things pressing TEST is supposed to rule out and it is invisible until something
// authenticates.
func TestSelfTestFailsAgainstRejectingOpenObserveDespiteCompleteConfig(t *testing.T) {
	api := &observeAPI{streams: []string{"tg_worker"}}
	m, _ := newSelfTestFixture(t, api)
	// The stored credential is rotated out from under the module AFTER construction — exactly what a revoked
	// token looks like — while every configured value stays present and non-empty.
	t.Setenv(selfTestTokenEnv, "cmV2b2tlZC1idXQtdmVyeS1tdWNoLW5vbi1lbXB0eQ==")

	if strings.TrimSpace(m.endpoint) == "" || strings.TrimSpace(string(m.tokenRef)) == "" {
		t.Fatal("fixture must have every configured value populated")
	}
	if v, err := m.tokenRef.Resolve(); err != nil || strings.TrimSpace(v) == "" {
		t.Fatalf("fixture must have a non-empty resolvable token (v=%q err=%v)", v, err)
	}

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("a rejected credential MUST fail the test — a configured-values check would pass here: %+v", res)
	}
	if !strings.Contains(res.Detail, "rejected the credential") {
		t.Errorf("Detail must name the credential as the problem, got %q", res.Detail)
	}
	if api.ingested() {
		t.Fatal("even while failing, the probe must not have written to OpenObserve")
	}
}

// THE KILLING ORACLE, transport half: a closed port with the configuration fully populated.
func TestSelfTestFailsAgainstClosedOpenObserve(t *testing.T) {
	t.Setenv(selfTestTokenEnv, selfTestToken)
	api := &observeAPI{streams: []string{"tg_worker"}}
	srv := httptest.NewServer(api)
	endpoint := srv.URL + "/api/default"
	srv.Close() // the port is now closed: a real connection refusal, not a simulated one
	m := New(endpoint, config.SecretRef("env:"+selfTestTokenEnv))

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an unreachable OpenObserve MUST be an error, not a pass: %+v", res)
	}
	if !strings.Contains(res.Summary, "could not be reached") {
		t.Errorf("Summary must say OpenObserve was unreachable, got %q", res.Summary)
	}
	if !strings.Contains(res.Detail, "nothing accepted a connection") && !strings.Contains(res.Detail, "could not be reached at") {
		t.Errorf("Detail must classify this as unreachable, got %q", res.Detail)
	}
	assertNoTokenLeak(t, res.Summary, res.Detail, errText(err))
}

// A reference that does not resolve is a TG-side secret fault and must be named as one — and no request may
// leave the process, because there is no credential to present.
func TestSelfTestUnresolvedTokenRefIsATGSideFaultAndSendsNothing(t *testing.T) {
	api := &observeAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	m := New(srv.URL+"/api/default", config.SecretRef("env:TG_TEST_OO_SELFTEST_ABSENT"))

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an unresolved token reference must be an error: %+v", res)
	}
	if !strings.Contains(res.Detail, "TG-side secret problem") {
		t.Errorf("Detail must attribute this to TG's secret backend, not to OpenObserve, got %q", res.Detail)
	}
	if got := api.requests(); len(got) != 0 {
		t.Errorf("no request may be issued when the token cannot be read, got %v", got)
	}
}

// A token reference that resolves EMPTY 401s every export while looking configured. It must fail closed,
// before any request.
func TestSelfTestEmptyTokenFailsClosed(t *testing.T) {
	t.Setenv(selfTestTokenEnv, "")
	api := &observeAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	m := New(srv.URL+"/api/default", config.SecretRef("env:"+selfTestTokenEnv))

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an empty token must be an error: %+v", res)
	}
	if !strings.Contains(res.Summary, "EMPTY") {
		t.Errorf("Summary must say the stored token is empty, got %q", res.Summary)
	}
	if got := api.requests(); len(got) != 0 {
		t.Errorf("no request may be issued with an empty credential, got %v", got)
	}
}

// A module with no endpoint was never registered as an exporter; saying so is more useful than a nil-deref
// inside the activity.
func TestSelfTestUnconfiguredModuleIsAnHonestFailure(t *testing.T) {
	m := New("", config.SecretRef("env:"+selfTestTokenEnv))
	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("a module with no base URL must fail the test: %+v", res)
	}
	if !strings.Contains(res.Summary, "no OpenObserve base URL") {
		t.Errorf("Summary must say there is no base URL, got %q", res.Summary)
	}
}

// The probe must honour the console's budget rather than holding an operator on a spinner.
func TestSelfTestRespectsContext(t *testing.T) {
	api := &observeAPI{streams: []string{"tg_worker"}}
	m, _ := newSelfTestFixture(t, api)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.SelfTest(ctx, "alice@example"); err == nil {
		t.Fatal("a cancelled context must abort the probe")
	}
	if api.ingested() {
		t.Fatal("a cancelled probe must not have written to OpenObserve")
	}
}

func assertNoTokenLeak(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		if strings.Contains(s, selfTestToken) || strings.Contains(s, "CANARY") {
			t.Errorf("the ingest token leaked into operator-facing text: %q", s)
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// A REFERENCE FIELD HOLDING THE TOKEN ITSELF must not be echoed back.
//
// The ingest token pasted into TG_OPENOBSERVE_TOKEN_REF instead of a pointer to it is the one value a
// "reference" can hold that is NOT safe to display. core/config refuses to echo it (its error reports only
// the length), and the probe must not undo that on the most commonly copied text in an incident.
func TestSelfTestDoesNotEchoASecretPastedIntoTheReferenceField(t *testing.T) {
	api := &observeAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	m := New(srv.URL+"/api/default", config.SecretRef(selfTestToken)) // a bare literal, not a reference

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("a scheme-less reference cannot resolve and must fail the test: %+v", res)
	}
	assertNoTokenLeak(t, res.Summary, res.Detail, errText(err))
	if !strings.Contains(res.Summary, "no env:/file:/store: prefix") {
		t.Errorf("Summary must say the field holds a pasted token rather than a reference, got %q", res.Summary)
	}
	if got := api.requests(); len(got) != 0 {
		t.Errorf("no request may be issued when the token cannot be read, got %v", got)
	}
}
