package langfuse

// Oracles for the console's TEST button on the Langfuse module.
//
// These drive SelfTest over a REAL *http.Client against a REAL httptest server: the module is built by New
// WITHOUT WithHTTPClient, so the transport under test is the production one. The fakeDoer the ingestion
// oracles use (langfuse_test.go) is the right seam for asserting batch bodies; it is the wrong seam here,
// because half of what this probe claims to establish IS the transport — a closed port is an outcome a Doer
// fake can only imitate by returning an error somebody typed.
//
// The property asserted in EVERY case, passing and failing alike, is that nothing was ingested. A probe that
// writes a synthetic trace on its way to reporting an error has still put a trace in the operator's project.

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

const (
	selfTestPublicEnv = "TG_TEST_LF_SELFTEST_PUBLIC"
	selfTestSecretEnv = "TG_TEST_LF_SELFTEST_SECRET"
	// Realistic Langfuse keys AND canaries: the secret half in particular must never reach Summary, Detail
	// or an error, because those are the strings an operator pastes into a ticket.
	selfTestPublicKey = "pk-lf-1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d"
	selfTestSecretKey = "sk-lf-CANARY-9f3a1b-must-never-be-echoed"
)

// langfuseAPI is a stand-in Langfuse that serves the one GET the probe makes and RECORDS every request. The
// recording is not diagnostics: it is the only way to assert the safety property that matters most here —
// that pressing TEST ingested nothing.
type langfuseAPI struct {
	mu   sync.Mutex
	hits []string

	status     int    // non-zero: force this status on the projects route
	body       string // non-empty: serve this body verbatim instead of the project list
	projects   []map[string]string
	sawIngest  bool // set if anything ever reached the ingestion route
	sawNonRead bool // set if any request arrived with a method other than GET
}

func (s *langfuseAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits = append(s.hits, r.Method+" "+r.URL.Path)
	if r.Method != http.MethodGet {
		s.sawNonRead = true
	}
	if strings.Contains(r.URL.Path, ingestPath) {
		s.sawIngest = true
	}
	status, body, projects := s.status, s.body, s.projects
	s.mu.Unlock()

	// The credential is checked for real: a probe that never presented the pair would pass against a revoked
	// one, which is one of the three things TEST exists to rule out.
	user, pass, ok := r.BasicAuth()
	if !ok || user != selfTestPublicKey || pass != selfTestSecretKey {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Invalid credentials. Confirm that you've configured the correct host."}`))
		return
	}
	if status != 0 && (status < 200 || status >= 300) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"served by the oracle"}`))
		return
	}
	if body != "" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
		return
	}
	out, _ := json.Marshal(map[string]any{"data": projects})
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (s *langfuseAPI) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.hits...)
}

func (s *langfuseAPI) ingested() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sawIngest || s.sawNonRead
}

// newSelfTestFixture builds the module the way production does — a real HTTP client, both keys supplied ONLY
// as secret references — pointed at a live local server.
func newSelfTestFixture(t *testing.T, api *langfuseAPI) (*Module, *httptest.Server) {
	t.Helper()
	t.Setenv(selfTestPublicEnv, selfTestPublicKey)
	t.Setenv(selfTestSecretEnv, selfTestSecretKey)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	m := New(srv.URL, config.SecretRef("env:"+selfTestPublicEnv), config.SecretRef("env:"+selfTestSecretEnv))
	return m, srv
}

// The pass and the failure classes, driven end to end over a real socket.
func TestSelfTestClassifiesLangfuseAnswers(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		projects    []map[string]string
		wantErr     bool
		wantSummary []string
		wantDetail  []string
	}{
		{
			// The observation is the whole point: the project NAME is what tells an operator they are
			// authenticated against staging rather than production.
			name:        "a project the key pair owns is named back",
			projects:    []map[string]string{{"id": "cmtg0prod01", "name": "TG production"}},
			wantSummary: []string{"TG production", "cmtg0prod01", "no trace or sample was ingested"},
			wantDetail:  []string{"does not prove ingestion succeeds"},
		},
		{
			name:        "401 names the credential problem and the half-rotated pair",
			status:      http.StatusUnauthorized,
			wantErr:     true,
			wantSummary: []string{"401"},
			wantDetail:  []string{"rejected the key pair", "half-rotated"},
		},
		{
			name:        "403 separates an accepted credential from a permitted one",
			status:      http.StatusForbidden,
			wantErr:     true,
			wantSummary: []string{"403"},
			wantDetail:  []string{"accepted the credential but refused the read"},
		},
		{
			name:        "404 says the base URL is not the Langfuse API root",
			status:      http.StatusNotFound,
			wantErr:     true,
			wantSummary: []string{"404"},
			wantDetail:  []string{projectsPath, "extra path prefix"},
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
			name:        "a 200 that is not a project list is a failure",
			body:        "<html><body>Sign in to continue</body></html>",
			wantErr:     true,
			wantSummary: []string{"answered 200 but not with a Langfuse project list"},
			wantDetail:  []string{"not the Langfuse API root"},
		},
		{
			// The pair is proven (a bad pair 401s before this point), but no project can be named — so the
			// operator is told the observation is missing rather than handed a bare green.
			name:        "an empty project list passes but says the observation is missing",
			projects:    []map[string]string{},
			wantSummary: []string{"named NO project"},
			wantDetail:  []string{"cannot say WHICH project"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &langfuseAPI{status: tc.status, body: tc.body, projects: tc.projects}
			m, srv := newSelfTestFixture(t, api)

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
			// The endpoint is reported so a pass against the WRONG Langfuse is still legible.
			if !strings.Contains(res.Summary, srv.URL) {
				t.Errorf("Summary must name the endpoint it reached (%s), got %q", srv.URL, res.Summary)
			}

			// The safety property, asserted on every path.
			if api.ingested() {
				t.Fatalf("SelfTest wrote to Langfuse — requests: %v", api.requests())
			}
			if reqs := api.requests(); len(reqs) != 1 || reqs[0] != "GET "+projectsPath {
				t.Errorf("the probe must make exactly one authenticated GET of %s, got %v", projectsPath, reqs)
			}
			assertNoKeyLeak(t, res.Summary, res.Detail, errText(err))
		})
	}
}

// THE KILLING ORACLE.
//
// Every configured value is present and non-empty — a base URL, two references that resolve to non-empty
// keys — and Langfuse rejects the pair. A "the configuration is filled in" check would return a green here;
// that is precisely the mock this probe must not be, because a revoked key pair is one of the three things
// pressing TEST is supposed to rule out and it is invisible until something authenticates.
func TestSelfTestFailsAgainstRejectingLangfuseDespiteCompleteConfig(t *testing.T) {
	api := &langfuseAPI{}
	m, _ := newSelfTestFixture(t, api)
	// The stored secret is rotated out from under the module AFTER construction — exactly what a revoked key
	// looks like — while every configured value stays present and non-empty.
	t.Setenv(selfTestSecretEnv, "sk-lf-revoked-but-very-much-non-empty")

	if strings.TrimSpace(m.endpoint) == "" || strings.TrimSpace(string(m.publicRef)) == "" || strings.TrimSpace(string(m.secretRef)) == "" {
		t.Fatal("fixture must have every configured value populated")
	}
	for _, ref := range []config.SecretRef{m.publicRef, m.secretRef} {
		if v, err := ref.Resolve(); err != nil || strings.TrimSpace(v) == "" {
			t.Fatalf("fixture must have %q resolve to a non-empty value (err=%v)", string(ref), err)
		}
	}

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("a rejected key pair MUST fail the test — a configured-values check would pass here: %+v", res)
	}
	if !strings.Contains(res.Detail, "rejected the key pair") {
		t.Errorf("Detail must name the credential as the problem, got %q", res.Detail)
	}
	if api.ingested() {
		t.Fatal("even while failing, the probe must not have written to Langfuse")
	}
}

// THE KILLING ORACLE, transport half: a closed port with the configuration fully populated.
func TestSelfTestFailsAgainstClosedLangfuse(t *testing.T) {
	api := &langfuseAPI{projects: []map[string]string{{"id": "cmtg0prod01", "name": "TG production"}}}
	m, srv := newSelfTestFixture(t, api)
	srv.Close() // the port is now closed: a real connection refusal, not a simulated one

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an unreachable Langfuse MUST be an error, not a pass: %+v", res)
	}
	if !strings.Contains(res.Summary, "could not be reached") {
		t.Errorf("Summary must say Langfuse was unreachable, got %q", res.Summary)
	}
	if !strings.Contains(res.Detail, "nothing accepted a connection") && !strings.Contains(res.Detail, "could not be reached at") {
		t.Errorf("Detail must classify this as unreachable, got %q", res.Detail)
	}
	assertNoKeyLeak(t, res.Summary, res.Detail, errText(err))
}

// A reference that does not resolve is a TG-side secret fault and must be named as one — and no request may
// leave the process, because there is no credential to present.
func TestSelfTestUnresolvedKeyRefIsATGSideFaultAndSendsNothing(t *testing.T) {
	cases := []struct {
		name               string
		publicRef, secret  string
		wantSummarySubstr  string
		wantDetailContains string
	}{
		{"public key missing", "env:TG_TEST_LF_SELFTEST_ABSENT", "env:" + selfTestSecretEnv, "public key could not be read", "public key never resolved"},
		{"secret key missing", "env:" + selfTestPublicEnv, "env:TG_TEST_LF_SELFTEST_ABSENT", "secret key could not be read", "secret key never resolved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(selfTestPublicEnv, selfTestPublicKey)
			t.Setenv(selfTestSecretEnv, selfTestSecretKey)
			api := &langfuseAPI{}
			srv := httptest.NewServer(api)
			defer srv.Close()
			m := New(srv.URL, config.SecretRef(tc.publicRef), config.SecretRef(tc.secret))

			res, err := m.SelfTest(context.Background(), "alice@example")
			if err == nil {
				t.Fatalf("an unresolved key reference must be an error: %+v", res)
			}
			if !strings.Contains(res.Summary, tc.wantSummarySubstr) {
				t.Errorf("Summary %q must name the failing half", res.Summary)
			}
			if !strings.Contains(res.Detail, "TG-side secret problem") || !strings.Contains(res.Detail, tc.wantDetailContains) {
				t.Errorf("Detail must attribute this to TG's secret backend, got %q", res.Detail)
			}
			if got := api.requests(); len(got) != 0 {
				t.Errorf("no request may be issued when a key cannot be read, got %v", got)
			}
		})
	}
}

// An empty half of the pair resolves cleanly and 401s every export. It must fail closed, before any request.
func TestSelfTestEmptyKeyHalfFailsClosed(t *testing.T) {
	t.Setenv(selfTestPublicEnv, selfTestPublicKey)
	t.Setenv(selfTestSecretEnv, "")
	api := &langfuseAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	m := New(srv.URL, config.SecretRef("env:"+selfTestPublicEnv), config.SecretRef("env:"+selfTestSecretEnv))

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an empty secret key must be an error: %+v", res)
	}
	if !strings.Contains(res.Summary, "EMPTY") || !strings.Contains(res.Summary, selfTestSecretEnv) {
		t.Errorf("Summary must say which half is empty, got %q", res.Summary)
	}
	if got := api.requests(); len(got) != 0 {
		t.Errorf("no request may be issued with half a credential, got %v", got)
	}
}

// A module with no endpoint was never registered as an exporter; saying so is more useful than a nil-deref
// inside the activity.
func TestSelfTestUnconfiguredModuleIsAnHonestFailure(t *testing.T) {
	m := New("", config.SecretRef("env:"+selfTestPublicEnv), config.SecretRef("env:"+selfTestSecretEnv))
	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("a module with no base URL must fail the test: %+v", res)
	}
	if !strings.Contains(res.Summary, "no Langfuse base URL") {
		t.Errorf("Summary must say there is no base URL, got %q", res.Summary)
	}
}

// The probe must honour the console's budget rather than holding an operator on a spinner.
func TestSelfTestRespectsContext(t *testing.T) {
	api := &langfuseAPI{projects: []map[string]string{{"id": "cmtg0prod01", "name": "TG production"}}}
	m, _ := newSelfTestFixture(t, api)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.SelfTest(ctx, "alice@example"); err == nil {
		t.Fatal("a cancelled context must abort the probe")
	}
	if api.ingested() {
		t.Fatal("a cancelled probe must not have written to Langfuse")
	}
}

func assertNoKeyLeak(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		if strings.Contains(s, selfTestSecretKey) || strings.Contains(s, "CANARY") {
			t.Errorf("the secret key leaked into operator-facing text: %q", s)
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// A REFERENCE FIELD HOLDING THE KEY ITSELF must not be echoed back.
//
// A Langfuse key pasted into the reference field instead of a pointer to it is the one value a "reference"
// can hold that is NOT safe to display. core/config refuses to echo it (its error reports only the
// length), and the probe must not undo that on the most commonly copied text in an incident.
func TestSelfTestDoesNotEchoASecretPastedIntoTheReferenceField(t *testing.T) {
	for _, tc := range []struct{ name, public, secret string }{
		{"public half is a literal", selfTestPublicKey, "env:" + selfTestSecretEnv},
		{"secret half is a literal", "env:" + selfTestPublicEnv, selfTestSecretKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(selfTestPublicEnv, selfTestPublicKey)
			t.Setenv(selfTestSecretEnv, selfTestSecretKey)
			api := &langfuseAPI{}
			srv := httptest.NewServer(api)
			defer srv.Close()
			m := New(srv.URL, config.SecretRef(tc.public), config.SecretRef(tc.secret))

			res, err := m.SelfTest(context.Background(), "alice@example")
			if err == nil {
				t.Fatalf("a scheme-less reference cannot resolve and must fail the test: %+v", res)
			}
			assertNoKeyLeak(t, res.Summary, res.Detail, errText(err))
			if strings.Contains(res.Summary+res.Detail+errText(err), selfTestPublicKey) {
				t.Errorf("the pasted public key leaked into operator-facing text: %q / %q / %v", res.Summary, res.Detail, err)
			}
			if !strings.Contains(res.Detail, "no env:/file:/store: prefix") {
				t.Errorf("Detail must say the field holds a pasted key rather than a reference, got %q", res.Detail)
			}
			if got := api.requests(); len(got) != 0 {
				t.Errorf("no request may be issued when a key cannot be read, got %v", got)
			}
		})
	}
}
