package openobserve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// readTokenEnv / newReaderFixture build a Reader the production way — the token supplied ONLY as a secret
// reference (INV-13) — over the recording fakeDoer (openobserve_test.go), so the oracle drives the real
// request-building path against a fake OpenObserve and pins the exact wire shape without a live API.
const readTokenEnv = "TG_TEST_OO_READ_TOKEN"

func newReaderFixture(t *testing.T, opts ...ReaderOption) (*Reader, *fakeDoer) {
	t.Helper()
	t.Setenv(readTokenEnv, ingestToken)
	f := &fakeDoer{}
	r := NewReader("https://openobserve.example/api/default", config.SecretRef("env:"+readTokenEnv),
		append([]ReaderOption{WithReaderHTTPClient(f)}, opts...)...)
	if r == nil {
		t.Fatal("fixture reader must not be nil")
	}
	return r, f
}

// errDoer returns a transport error for every request, so the fail-closed transport path is exercisable.
type errDoer struct{ err error }

func (e errDoer) Do(*http.Request) (*http.Response, error) { return nil, e.err }

// wireSearch mirrors the OpenObserve Search API request body so a test decodes the REAL bytes and pins the
// timestamp UNIT (microseconds) and the size, never a paraphrase.
type wireSearch struct {
	Query struct {
		SQL       string `json:"sql"`
		StartTime int64  `json:"start_time"`
		EndTime   int64  `json:"end_time"`
		From      int    `json:"from"`
		Size      int    `json:"size"`
	} `json:"query"`
	SearchType string `json:"search_type"`
}

func decodeSearchBody(t *testing.T, body string) wireSearch {
	t.Helper()
	var w wireSearch
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		t.Fatalf("search body must be valid JSON: %v\nbody=%s", err, body)
	}
	return w
}

// TestNewReaderNilOnEmptyEndpoint locks the registration gate at construction: no endpoint ⇒ no reader ⇒ the
// correlate tool is structurally absent, never a live tool that errors on every call.
func TestNewReaderNilOnEmptyEndpoint(t *testing.T) {
	if NewReader("", config.SecretRef("env:X")) != nil {
		t.Fatal("an empty endpoint must yield a nil Reader (config-not-code: absent ⇒ no capability)")
	}
	if NewReader("   ", config.SecretRef("env:X")) != nil {
		t.Fatal("a blank endpoint must yield a nil Reader")
	}
}

// TestCorrelatePostsSearchWithBasicAuthMicrosecondsAndCappedSize is the protocol lock: Correlate must POST to
// the _search route under HTTP Basic auth (never Bearer), carry the time window in MICROSECONDS, and never
// ask OpenObserve for more than the hard hit cap.
func TestCorrelatePostsSearchWithBasicAuthMicrosecondsAndCappedSize(t *testing.T) {
	r, f := newReaderFixture(t, WithStream("journald"), WithHostField("k8s_host"))

	_, err := r.Correlate(context.Background(), CorrelationQuery{
		Hosts:       []string{"sw01", "app01"},
		StartMicros: 1786000000000000,
		EndMicros:   1786000900000000,
		Size:        100000, // absurd: must be clamped to searchMaxHits
	})
	if err != nil {
		t.Fatalf("a 2xx search must succeed: %v", err)
	}
	if len(f.reqs) != 1 {
		t.Fatalf("Correlate must issue exactly one POST, got %d", len(f.reqs))
	}
	rq := f.reqs[0]
	if rq.method != http.MethodPost || rq.path != "/api/default/_search" {
		t.Errorf("search request = %s %s, want POST /api/default/_search", rq.method, rq.path)
	}
	if rq.auth != "Basic "+ingestToken {
		t.Errorf("Authorization = %q, want HTTP Basic %q (a bare Bearer 401s on OpenObserve)", rq.auth, "Basic "+ingestToken)
	}
	if strings.HasPrefix(rq.auth, "Bearer ") {
		t.Errorf("search must not use a bare Bearer token: %q", rq.auth)
	}
	if rq.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", rq.contentType)
	}
	w := decodeSearchBody(t, rq.body)
	if w.Query.StartTime != 1786000000000000 || w.Query.EndTime != 1786000900000000 {
		t.Errorf("window must ship in microseconds unchanged, got start=%d end=%d", w.Query.StartTime, w.Query.EndTime)
	}
	if w.Query.Size != searchMaxHits {
		t.Errorf("requested size must be clamped to the hard cap %d, got %d", searchMaxHits, w.Query.Size)
	}
	if !strings.Contains(w.Query.SQL, `FROM "journald"`) {
		t.Errorf("SQL must query the configured stream, got %q", w.Query.SQL)
	}
	if !strings.Contains(w.Query.SQL, `"k8s_host" IN ('sw01','app01')`) {
		t.Errorf("SQL must filter the configured host field over the host set, got %q", w.Query.SQL)
	}
}

// TestBuildCorrelationSQLEscapesAlertInjection is the query-injection lock (TG-39). An alert-derived Pattern
// and Severity must land inside a single-quoted literal with every quote DOUBLED, so nothing can break out
// into SQL structure. This is the constitutional "no string-built injection" guarantee for the search body.
func TestBuildCorrelationSQLEscapesAlertInjection(t *testing.T) {
	sql := buildCorrelationSQL("syslog", "host", []string{"h1", "h2"}, "x') OR ('1'='1", "err'; DROP")

	// Host set: a plain IN over quoted literals.
	if !strings.Contains(sql, `"host" IN ('h1','h2')`) {
		t.Errorf("host set must be a quoted IN list, got %q", sql)
	}
	// The malicious pattern's single quotes must be DOUBLED, becoming a harmless literal argument to match_all.
	if !strings.Contains(sql, `match_all('x'') OR (''1''=''1')`) {
		t.Errorf("pattern injection was not neutralised by quote-doubling, got %q", sql)
	}
	// The severity likewise.
	if !strings.Contains(sql, `match_all('err''; DROP')`) {
		t.Errorf("severity injection was not neutralised, got %q", sql)
	}
	// The dangerous UNescaped structure must NOT appear: an odd number of quotes that would end the literal.
	if strings.Contains(sql, `match_all('x') OR ('1'='1')`) {
		t.Fatal("the pattern broke out of its string literal — injection is possible")
	}
	if !strings.HasSuffix(sql, `ORDER BY "_timestamp" DESC`) {
		t.Errorf("results must be ordered newest-first by the timestamp column, got %q", sql)
	}
}

// TestCorrelateHitsAreHostAttributedAndCapped: the reader lifts host + timestamp from each record for
// attribution and never returns more than the requested size, flagging truncation when the server sends more.
func TestCorrelateHitsAreHostAttributedAndCapped(t *testing.T) {
	r, f := newReaderFixture(t)
	f.respRet = `{"took":3,"total":9,"hits":[
		{"_timestamp":1786000000000000,"host":"app01","message":"a"},
		{"_timestamp":1786000000000001,"host":"app02","message":"b"},
		{"_timestamp":1786000000000002,"host":"app03","message":"c"}
	]}`
	res, err := r.Correlate(context.Background(), CorrelationQuery{Hosts: []string{"app01", "app02", "app03"}, Size: 2})
	if err != nil {
		t.Fatalf("Correlate must succeed: %v", err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("size cap of 2 must bound the returned hits, got %d", len(res.Hits))
	}
	if !res.Truncated {
		t.Error("more hits than the cap must set Truncated")
	}
	if res.Hits[0].Host != "app01" || res.Hits[0].TimestampMicros != 1786000000000000 {
		t.Errorf("first hit must be attributed to app01 with its microsecond stamp, got %+v", res.Hits[0])
	}
}

// TestCorrelateNon2xxFailsClosed: a non-2xx answer is an ERROR carrying the status — never an empty result a
// caller could misread as "no logs".
func TestCorrelateNon2xxFailsClosed(t *testing.T) {
	r, f := newReaderFixture(t)
	f.status = http.StatusServiceUnavailable
	f.respRet = "upstream down"
	res, err := r.Correlate(context.Background(), CorrelationQuery{Hosts: []string{"app01"}})
	if err == nil {
		t.Fatal("a non-2xx search must be an error, not an empty SearchResult")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error must carry the status, got %v", err)
	}
	if len(res.Hits) != 0 {
		t.Error("a failed search must return no hits")
	}
}

// TestCorrelateTransportErrorFailsClosed: a transport failure is an error, not a fabricated empty result.
func TestCorrelateTransportErrorFailsClosed(t *testing.T) {
	t.Setenv(readTokenEnv, ingestToken)
	r := NewReader("https://openobserve.example/api/default", config.SecretRef("env:"+readTokenEnv),
		WithReaderHTTPClient(errDoer{err: errors.New("connection refused")}))
	_, err := r.Correlate(context.Background(), CorrelationQuery{Hosts: []string{"app01"}})
	if err == nil {
		t.Fatal("a transport error must fail closed")
	}
}

// TestCorrelateUndecodableReplyFailsClosed: a 200 that is not an OpenObserve search reply (a portal/proxy) is
// an error, not an empty result — the wrong-host case that looks like success to a status-only check.
func TestCorrelateUndecodableReplyFailsClosed(t *testing.T) {
	r, f := newReaderFixture(t)
	f.respRet = "<html>Sign in</html>"
	_, err := r.Correlate(context.Background(), CorrelationQuery{Hosts: []string{"app01"}})
	if err == nil {
		t.Fatal("a 200 that is not a search reply must fail closed")
	}
}

// TestCorrelateNoHostsIsAnError: an empty host set must not build `IN ()` or an unbounded scan.
func TestCorrelateNoHostsIsAnError(t *testing.T) {
	r, f := newReaderFixture(t)
	if _, err := r.Correlate(context.Background(), CorrelationQuery{Hosts: nil}); err == nil {
		t.Fatal("a query with no hosts must be an error")
	}
	if len(f.reqs) != 0 {
		t.Error("no request may be issued for an empty host set")
	}
}
