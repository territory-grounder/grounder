package awxplaybooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// The probe is exercised over a real TCP listener (httptest) rather than through the package's
// recordingDoer, because two of the things it must get right — a genuinely unreachable controller and the
// real Bearer header on the wire — cannot be observed through a transport that returns canned structs.

// probeSensorToken is the token value the probe must actually present. It must never appear in a Result.
const probeSensorToken = "awx-read-only-sensor-token-VALUE-must-not-leak"

// probeTemplatePage is a realistic AWX list answer. `next` is set so a test can prove the probe reads ONE
// page and does not walk the chain on an operator's spinner.
const probeTemplatePage = `{"count":37,"next":"/api/v2/job_templates/?page=2&page_size=200","previous":null,
	"results":[{"id":7,"name":"nginx-restart"},{"id":9,"name":"gather-facts"}]}`

// probeSrv records what the probe really sent, so a test can prove the read is a GET to the template list
// carrying the resolved sensor credential — not a check that configuration is non-empty.
type probeSrv struct {
	mu      sync.Mutex
	methods []string
	paths   []string
	auths   []string
	hits    int
}

func (r *probeSrv) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hits++
	r.methods = append(r.methods, req.Method)
	r.paths = append(r.paths, req.URL.Path)
	r.auths = append(r.auths, req.Header.Get("Authorization"))
}

// newProbeServer starts an AWX stand-in that records every request and answers with h. It fails the test
// on any non-GET or any /launch/ path: a knowledge lane that ever mutates has stopped being discovery-only,
// and a probe reachable from a settings dialog is the last place that may start a job.
func newProbeServer(t *testing.T, rec *probeSrv, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			t.Errorf("the self-test issued a non-GET request: %s %s", req.Method, req.URL.Path)
		}
		if strings.Contains(req.URL.Path, "/launch/") {
			t.Errorf("the self-test hit a launch endpoint: %s", req.URL.Path)
		}
		if rec != nil {
			rec.record(req)
		}
		h(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func serveJSON(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// newProbeClient builds the client the console would probe, with every configured value present and
// non-empty: a base URL and a sensor-token reference that really resolves.
func newProbeClient(t *testing.T, base string) *Client {
	t.Helper()
	t.Setenv("TG_TEST_PROBE_AWXPB_TOKEN", probeSensorToken)
	c, err := NewClient(ClientConfig{
		BaseURL:    base,
		TokenRef:   "env:TG_TEST_PROBE_AWXPB_TOKEN",
		HTTPClient: http.DefaultClient,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestSelfTest(t *testing.T) {
	cases := []struct {
		name        string
		handler     http.HandlerFunc
		closeFirst  bool // take the URL, then close the listener: a real unreachable controller
		wantErr     bool
		wantSummary []string
		wantDetail  []string
	}{
		{
			// The observation must come from the SERVED payload: neither the count nor the template name
			// appears anywhere in the module's configuration, so a Summary carrying them can only have been
			// read off the wire.
			name:        "success reports what it observed",
			handler:     serveJSON(http.StatusOK, probeTemplatePage),
			wantSummary: []string{"37 visible", "nginx-restart"},
		},
		{
			// AWX answers an unprivileged list with 200 and an empty page rather than 403, so this is the
			// shape a permission problem actually arrives in — and it means the lane ingests nothing for ever.
			name:        "zero templates is a failure, not a quiet pass",
			handler:     serveJSON(http.StatusOK, `{"count":0,"next":null,"previous":null,"results":[]}`),
			wantErr:     true,
			wantSummary: []string{"0 visible"},
			wantDetail:  []string{"NO job templates", "RBAC", "read access"},
		},
		{
			name:       "401 names the credential",
			handler:    serveJSON(http.StatusUnauthorized, `{"detail":"Authentication credentials were not provided."}`),
			wantErr:    true,
			wantDetail: []string{"401", "revoked", "env:TG_TEST_PROBE_AWXPB_TOKEN"},
		},
		{
			name:       "403 names the permission, not the token",
			handler:    serveJSON(http.StatusForbidden, `{"detail":"You do not have permission to perform this action."}`),
			wantErr:    true,
			wantDetail: []string{"403", "not permitted", "read access"},
		},
		{
			name:       "404 points at the base URL",
			handler:    serveJSON(http.StatusNotFound, `{"detail":"Not found."}`),
			wantErr:    true,
			wantDetail: []string{"404", "base URL"},
		},
		{
			name:       "5xx says the controller is unhealthy, not the credential",
			handler:    serveJSON(http.StatusBadGateway, `<html>bad gateway</html>`),
			wantErr:    true,
			wantDetail: []string{"502", "unhealthy"},
		},
		{
			name:       "a non-AWX answer is a failure",
			handler:    serveJSON(http.StatusOK, `<html><body>Please log in</body></html>`),
			wantErr:    true,
			wantDetail: []string{"not with an AWX list", "AWX root"},
		},
		{
			name:       "a closed port is unreachable and an error, never a pass",
			handler:    serveJSON(http.StatusOK, probeTemplatePage),
			closeFirst: true,
			wantErr:    true,
			wantDetail: []string{"could not be reached"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newProbeServer(t, nil, c.handler)
			if c.closeFirst {
				srv.Close()
			}
			client := newProbeClient(t, srv.URL)

			res, err := client.SelfTest(context.Background(), "alice@example")
			if c.wantErr && err == nil {
				t.Fatalf("expected an error, got a pass: %+v", res)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("expected a pass, got error %v (%+v)", err, res)
			}
			if strings.TrimSpace(res.Summary) == "" {
				t.Error("Summary is empty — the operator is shown nothing")
			}
			for _, want := range c.wantSummary {
				if !strings.Contains(res.Summary, want) {
					t.Errorf("Summary %q does not name %q", res.Summary, want)
				}
			}
			if c.wantErr && strings.TrimSpace(res.Detail) == "" {
				t.Error("a failure must carry an actionable Detail")
			}
			for _, want := range c.wantDetail {
				if !strings.Contains(res.Detail, want) {
					t.Errorf("Detail %q does not name %q", res.Detail, want)
				}
			}
			// selftest contract clause 5 / INV-13: nothing an operator can paste into a ticket may carry the
			// sensor token.
			shown := res.Summary + " " + res.Detail
			if err != nil {
				shown += " " + err.Error()
			}
			if strings.Contains(shown, probeSensorToken) {
				t.Fatalf("the sensor token leaked into the operator-visible result: %q", shown)
			}
		})
	}
}

// The probe must be the REAL first step of the real lane: ONE GET to the job-template list, carrying the
// token resolved from the client's own SecretRef, and it must NOT walk the pagination chain — the server's
// own count already states the total, and following .next would issue up to maxPages requests while an
// operator waits.
func TestSelfTestIssuesOneRealReadOnlyRequest(t *testing.T) {
	rec := &probeSrv{}
	srv := newProbeServer(t, rec, serveJSON(http.StatusOK, probeTemplatePage))
	client := newProbeClient(t, srv.URL)

	if _, err := client.SelfTest(context.Background(), "alice@example"); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.hits != 1 {
		t.Fatalf("expected exactly 1 request (one page, one attempt, no retry), got %d: %v", rec.hits, rec.paths)
	}
	if rec.methods[0] != http.MethodGet {
		t.Errorf("probe issued %s — this lane has only GET methods by construction", rec.methods[0])
	}
	if rec.paths[0] != "/api/v2/job_templates/" {
		t.Errorf("probe read %q, want the list endpoint an ingest starts with", rec.paths[0])
	}
	if rec.auths[0] != "Bearer "+probeSensorToken {
		t.Errorf("probe presented %q, want the token resolved from the client's own SecretRef", rec.auths[0])
	}
}

// The lane object carries the capability too, because the composition root holds the *Ingest — and pressing
// TEST must never re-ingest: Run writes the corpus, and a settings dialog may not rewrite the retrieval
// plane as a side effect of a connectivity check.
func TestIngestSelfTestDelegatesAndWritesNothing(t *testing.T) {
	rec := &probeSrv{}
	srv := newProbeServer(t, rec, serveJSON(http.StatusOK, probeTemplatePage))
	store := &memStore{}
	ing, err := NewIngest(newProbeClient(t, srv.URL), store)
	if err != nil {
		t.Fatalf("NewIngest: %v", err)
	}

	res, err := ing.SelfTest(context.Background(), "alice@example")
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if !strings.Contains(res.Summary, "37 visible") {
		t.Errorf("Summary %q must report what the lane's own client observed", res.Summary)
	}
	if store.saves != 0 {
		t.Fatalf("the self-test wrote the knowledge corpus %d time(s) — a TEST button may not re-ingest", store.saves)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.hits != 1 {
		t.Fatalf("expected exactly 1 request (the list only, no re-read by id), got %d: %v", rec.hits, rec.paths)
	}
}

// A sensor-token reference that cannot be resolved is a TG-side fault, and the message must say so rather
// than blaming AWX — the operator would otherwise go and look at a healthy controller.
func TestSelfTestUnresolvableTokenRefIsATGSideFault(t *testing.T) {
	srv := newProbeServer(t, nil, serveJSON(http.StatusOK, probeTemplatePage))
	c, err := NewClient(ClientConfig{
		BaseURL:    srv.URL,
		TokenRef:   "env:TG_TEST_PROBE_AWXPB_TOKEN_THAT_IS_NOT_SET",
		HTTPClient: http.DefaultClient,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	res, err := c.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an unresolvable token reference must not pass: %+v", res)
	}
	if !strings.Contains(res.Detail, "TG-side") {
		t.Errorf("Detail %q must say the fault is TG's, not AWX's", res.Detail)
	}
}

// AN UNRECOGNISED STATUS MAY QUOTE AWX'S OWN `detail`, NEVER THE RAW BODY.
//
// The body of a failed request was written by WHATEVER answered at that address, which on the far side of a
// TEST button is frequently a proxy rather than AWX. A debug/echo page renders the request back — and this
// request carries the sensor Bearer token. Quoting an unstructured body would put that token in the console
// and in the ticket the operator pastes it into.
func TestSelfTestNeverQuotesAnUnstructuredErrorBody(t *testing.T) {
	echo := func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusTeapot) // an unrecognised status: the branch that quotes the body
		_, _ = w.Write([]byte("proxy debug page\nAuthorization: " + req.Header.Get("Authorization") + "\n"))
	}
	srv := newProbeServer(t, nil, echo)
	client := newProbeClient(t, srv.URL)

	res, err := client.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an unrecognised status must not pass: %+v", res)
	}
	shown := res.Summary + " " + res.Detail + " " + err.Error()
	if strings.Contains(shown, probeSensorToken) {
		t.Fatalf("the sensor token was echoed back out of the response body: %q", shown)
	}
	if !strings.Contains(res.Detail, "418") {
		t.Errorf("Detail %q must still name the status the operator has to explain", res.Detail)
	}
}

// A base URL that cannot be parsed must not republish it. net/http strips the password from a TRANSPORT
// error; net/url does not strip anything from a PARSE error, and a base URL may carry userinfo.
func TestSelfTestNeverEchoesAnUnparseableBaseURL(t *testing.T) {
	const pw = "hunter2-must-not-leak"
	t.Setenv("TG_TEST_PROBE_AWXPB_TOKEN", probeSensorToken)
	c, err := NewClient(ClientConfig{
		BaseURL:    "https://tg:" + pw + "@awx.example\x7f", // a stray control character: url.Parse refuses it
		TokenRef:   "env:TG_TEST_PROBE_AWXPB_TOKEN",
		HTTPClient: http.DefaultClient,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	res, serr := c.SelfTest(context.Background(), "alice@example")
	if serr == nil {
		t.Fatalf("an unusable base URL must not pass: %+v", res)
	}
	shown := res.Summary + " " + res.Detail + " " + serr.Error()
	if strings.Contains(shown, pw) {
		t.Fatalf("the base URL's password leaked into the operator-visible result: %q", shown)
	}
	if !strings.Contains(res.Detail, "base URL") {
		t.Errorf("Detail %q must still name what the operator has to fix", res.Detail)
	}
}

// THE KILLING ORACLE.
//
// Every configured value is present and non-empty — a base URL and a sensor-token reference that resolves
// to a real non-empty token — and the endpoint answers 401. A SelfTest replaced by a
// configured-values-are-non-empty check passes here; this test fails it. That is what makes this probe more
// than a mock: a revoked token, a permission never granted and a controller that has been down for a week
// are all invisible to a configuration check, and all three look exactly like this.
func TestSelfTestFailsWithCompleteConfigAgainstARejectingServer(t *testing.T) {
	srv := newProbeServer(t, nil, serveJSON(http.StatusUnauthorized, `{"detail":"Invalid token."}`))
	t.Setenv("TG_TEST_PROBE_AWXPB_TOKEN", probeSensorToken)

	const ref = "env:TG_TEST_PROBE_AWXPB_TOKEN"
	// Guard the premise: if either of these were empty the test would prove nothing (NewClient already
	// fails closed on an empty base URL or token reference, which is itself only a configuration check).
	if srv.URL == "" || ref == "" {
		t.Fatal("the oracle requires a COMPLETE configuration")
	}
	if tok, err := config.SecretRef(ref).Resolve(); err != nil || tok == "" {
		t.Fatalf("the oracle requires a token reference that really resolves: %v", err)
	}

	c, err := NewClient(ClientConfig{BaseURL: srv.URL, TokenRef: ref, HTTPClient: http.DefaultClient})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	res, err := c.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("a complete configuration pointed at a rejecting controller MUST fail: %+v", res)
	}
	if !strings.Contains(res.Detail, "401") {
		t.Errorf("Detail %q must name the rejection so the operator knows the token is the problem", res.Detail)
	}

	// The same completeness against a dead port must also fail — the "controller has been down for a week"
	// case, which is likewise invisible to any check of the configured values.
	dead := newProbeServer(t, nil, serveJSON(http.StatusOK, probeTemplatePage))
	dead.Close()
	dc, err := NewClient(ClientConfig{BaseURL: dead.URL, TokenRef: ref, HTTPClient: http.DefaultClient})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if res, err := dc.SelfTest(context.Background(), "alice@example"); err == nil {
		t.Fatalf("a complete configuration pointed at a dead port MUST fail: %+v", res)
	}
}
