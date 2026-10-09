package openobserve

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

const selfTestReadTokenEnv = "TG_TEST_OO_READ_SELFTEST_TOKEN"

// searchAPI is a stand-in OpenObserve that serves the one bounded _search the probe makes and RECORDS every
// request — the only way to assert that pressing TEST read (POST /_search) and ingested nothing.
type searchAPI struct {
	mu     sync.Mutex
	reqs   []string
	status int
	body   string
	total  int

	sawIngest bool
}

func (s *searchAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.reqs = append(s.reqs, r.Method+" "+r.URL.Path)
	if strings.Contains(r.URL.Path, "/v1/logs") || strings.Contains(r.URL.Path, "/v1/traces") {
		s.sawIngest = true
	}
	status, body, total := s.status, s.body, s.total
	s.mu.Unlock()

	// The credential is checked for real: a probe that never presented the token would pass against a revoked
	// one. HTTP Basic with the token verbatim — a Bearer 401s here exactly as on a real OpenObserve.
	if r.Header.Get("Authorization") != "Basic "+ingestToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if status != 0 && (status < 200 || status >= 300) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"code":` + strconv.Itoa(status) + `}`))
		return
	}
	if body != "" {
		_, _ = w.Write([]byte(body))
		return
	}
	_, _ = w.Write([]byte(`{"took":2,"total":` + strconv.Itoa(total) + `,"hits":[]}`))
}

func (s *searchAPI) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

func newProbeFixture(t *testing.T, api *searchAPI, opts ...ReaderOption) *Reader {
	t.Helper()
	t.Setenv(selfTestReadTokenEnv, ingestToken)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	// The reader is built the production way — a REAL *http.Client (no WithReaderHTTPClient), the token as a
	// reference — so the probe exercises the transport, not a fake.
	return NewReader(srv.URL+"/api/default", config.SecretRef("env:"+selfTestReadTokenEnv), opts...)
}

// TestReaderSelfTestPassesAndReportsTheStream: a reachable, authenticated OpenObserve with a queryable stream
// passes, and the Summary names what it observed (the endpoint and the stream) and that nothing was ingested.
func TestReaderSelfTestPassesAndReportsTheStream(t *testing.T) {
	api := &searchAPI{total: 7}
	r := newProbeFixture(t, api, WithStream("tg_syslog"))
	res, err := r.SelfTest(context.Background(), "alice@example")
	if err != nil {
		t.Fatalf("a reachable, queryable stream must pass: %v (%+v)", err, res)
	}
	for _, want := range []string{"reached OpenObserve", "tg_syslog", "nothing was ingested"} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("Summary must name %q, got %q", want, res.Summary)
		}
	}
	// The safety property: the probe read (POST /_search) and never touched an ingest route.
	if api.sawIngest {
		t.Fatal("SelfTest must not write to OpenObserve")
	}
	reqs := api.requests()
	if len(reqs) != 1 || reqs[0] != "POST /api/default/_search" {
		t.Errorf("the probe must make exactly one bounded search, got %v", reqs)
	}
}

// TestReaderSelfTest404NamesTheStreamAndPrefix: a 404 is the correlate-specific failure the stream-list probe
// cannot catch — a stream the pipeline never created, or a missing /api/<org> prefix.
func TestReaderSelfTest404NamesTheStreamAndPrefix(t *testing.T) {
	api := &searchAPI{status: http.StatusNotFound}
	r := newProbeFixture(t, api, WithStream("absent_stream"))
	res, err := r.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("a 404 must fail the test: %+v", res)
	}
	if !strings.Contains(res.Detail, "absent_stream") || !strings.Contains(res.Detail, "/api/<org>") {
		t.Errorf("the 404 detail must name the stream and the prefix, got %q", res.Detail)
	}
}

// TestReaderSelfTest403SeparatesListFromSearch: a credential that lists but cannot search fails HERE, which is
// the scope gap the exporter's stream-list probe would have passed.
func TestReaderSelfTest403SeparatesListFromSearch(t *testing.T) {
	api := &searchAPI{status: http.StatusForbidden}
	r := newProbeFixture(t, api)
	res, err := r.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("a 403 must fail: %+v", res)
	}
	if !strings.Contains(res.Detail, "refused the SEARCH") {
		t.Errorf("the 403 detail must name search as the refused scope, got %q", res.Detail)
	}
}

// TestReaderSelfTestUnconfiguredIsHonest: SelfTest on a nil reader (the state an empty TG_OPENOBSERVE_URL
// produces) is an honest failure, not a nil-deref.
func TestReaderSelfTestUnconfiguredIsHonest(t *testing.T) {
	var r *Reader // exactly what NewReader("") returns
	res, err := r.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("a nil reader must fail the test: %+v", res)
	}
	if !strings.Contains(res.Summary, "no OpenObserve base URL") {
		t.Errorf("Summary must say there is no base URL, got %q", res.Summary)
	}
}

// TestReaderSelfTestClosedEndpointFailsClosed: a closed port with full config is an error, not a pass.
func TestReaderSelfTestClosedEndpointFailsClosed(t *testing.T) {
	t.Setenv(selfTestReadTokenEnv, ingestToken)
	srv := httptest.NewServer(&searchAPI{})
	endpoint := srv.URL + "/api/default"
	srv.Close() // the port is now closed
	r := NewReader(endpoint, config.SecretRef("env:"+selfTestReadTokenEnv))
	res, err := r.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an unreachable OpenObserve must be an error: %+v", res)
	}
	if !strings.Contains(res.Summary, "could not be reached") {
		t.Errorf("Summary must say OpenObserve was unreachable, got %q", res.Summary)
	}
}
