package healthchecks

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	observability "github.com/territory-grounder/grounder/adapters/observability"
	"github.com/territory-grounder/grounder/core/config"
)

// compile-time proof the concrete module satisfies the stable Exporter contract from the test side too.
var _ observability.Exporter = (*Module)(nil)

type recordedReq struct {
	method, path, rawURL, auth string
	hasBody                    bool
}

// fakeDoer records every request and returns a canned (status, body) response, so the oracle drives the
// module's REAL ping-URL-building path against a fake Healthchecks.io ping host. A network failure is
// simulated with netErr.
type fakeDoer struct {
	reqs    []recordedReq
	status  int
	respRet string
	netErr  error
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	hasBody := req.Body != nil && req.Body != http.NoBody
	if hasBody {
		b, _ := io.ReadAll(req.Body)
		hasBody = len(b) > 0
	}
	f.reqs = append(f.reqs, recordedReq{
		method:  req.Method,
		path:    req.URL.Path,
		rawURL:  req.URL.String(),
		auth:    req.Header.Get("Authorization"),
		hasBody: hasBody,
	})
	if f.netErr != nil {
		return nil, f.netErr
	}
	st := f.status
	if st == 0 {
		st = 200
	}
	resp := f.respRet
	if resp == "" {
		resp = "OK" // Healthchecks.io answers a healthy dead-man ping with the plain text body "OK".
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(resp)), Header: make(http.Header)}, nil
}

const checkUUID = "c0ffee-dead-man-853f-uuid"

func newFixture(t *testing.T) (*Module, *fakeDoer) {
	t.Helper()
	t.Setenv("TG_TEST_HC_CHECK", checkUUID)
	f := &fakeDoer{}
	// The check identifier is the URL-embedded secret and is supplied ONLY as a reference (INV-13).
	m := New("https://hc-ping.test", config.SecretRef("env:TG_TEST_HC_CHECK"), WithHTTPClient(f))
	return m, f
}

func lastReq(t *testing.T, f *fakeDoer) recordedReq {
	t.Helper()
	if len(f.reqs) == 0 {
		t.Fatal("expected at least one ping request, got none")
	}
	return f.reqs[len(f.reqs)-1]
}

// A healthy heartbeat must hit the plain success endpoint GET https://hc-ping.com/{uuid} — NOT /{uuid}/fail
// and NOT /{uuid}/start. Signalling success on the fail endpoint would page on every heartbeat; signalling on
// the wrong path would let the check silently expire. The dead-man secret rides in the URL path (never an
// Authorization header) and the request carries no body.
func TestPingHitsSuccessEndpointNotFailOrStart(t *testing.T) {
	m, f := newFixture(t)
	if err := m.Ping(context.Background()); err != nil {
		t.Fatalf("Ping must succeed against a 2xx dead-man host: %v", err)
	}
	r := lastReq(t, f)
	if r.method != http.MethodGet {
		t.Errorf("dead-man success ping must be GET, got %s", r.method)
	}
	if r.path != "/"+checkUUID {
		t.Errorf("success ping path = %q, want /%s (the bare check URL)", r.path, checkUUID)
	}
	if strings.HasSuffix(r.path, "/fail") || strings.HasSuffix(r.path, "/start") {
		t.Errorf("a HEALTHY heartbeat must not hit the /fail or /start signal, got %q", r.path)
	}
	if r.auth != "" {
		t.Errorf("the dead-man check id rides in the URL, not an Authorization header, got %q", r.auth)
	}
	if r.hasBody {
		t.Errorf("a dead-man ping carries no body")
	}
	if strings.Contains(r.rawURL, "?") {
		t.Errorf("the success ping URL must have no query string, got %q", r.rawURL)
	}
}

// Export is the liveness signal: a successfully produced batch is proof the writer is alive, so Export stamps
// freshness on any unstamped sample (INV-15) and then pings the SUCCESS endpoint. The stamp lets a downstream
// absent()-guarded staleness check page on a dead writer rather than read healthy.
func TestExportStampsFreshnessAndPingsSuccess(t *testing.T) {
	m, f := newFixture(t)
	samples := []observability.Sample{{Name: "cp.heartbeat", Value: 1}} // Stamped left zero on purpose.
	before := time.Now().UTC()
	if err := m.Export(context.Background(), samples); err != nil {
		t.Fatalf("Export must succeed: %v", err)
	}
	after := time.Now().UTC()
	// Freshness (INV-15): the previously-unstamped sample is stamped at export time, in UTC.
	got := samples[0].Stamped
	if got.IsZero() {
		t.Fatal("Export must stamp an unstamped sample (INV-15), got zero time")
	}
	if got.Before(before) || got.After(after) {
		t.Errorf("freshness stamp %v is outside [%v,%v]", got, before, after)
	}
	if got.Location() != time.UTC {
		t.Errorf("freshness stamp must be UTC, got %v", got.Location())
	}
	// ...and the liveness ping fired at the success endpoint.
	r := lastReq(t, f)
	if r.method != http.MethodGet || r.path != "/"+checkUUID {
		t.Errorf("Export must ping the success endpoint, got %s %s", r.method, r.path)
	}
	if strings.HasSuffix(r.path, "/fail") {
		t.Errorf("a healthy Export must not signal /fail, got %q", r.path)
	}
}

// A producer-supplied freshness stamp must NOT be clobbered — Export only stamps samples that are unstamped,
// so a real (possibly older) production timestamp survives to the downstream staleness check.
func TestExportPreservesProducerTimestamp(t *testing.T) {
	m, _ := newFixture(t)
	produced := time.Date(2026, 7, 15, 9, 30, 0, 0, time.UTC)
	samples := []observability.Sample{{Name: "cp.heartbeat", Value: 1, Stamped: produced}}
	if err := m.Export(context.Background(), samples); err != nil {
		t.Fatalf("Export must succeed: %v", err)
	}
	if !samples[0].Stamped.Equal(produced) {
		t.Errorf("Export clobbered a producer timestamp: got %v, want %v", samples[0].Stamped, produced)
	}
}

// Dead-man semantics: the batch itself is the liveness proof, so even an EMPTY batch still pings — an alive
// writer that momentarily has nothing to ship is still alive. (This is deliberately unlike an ingestion
// exporter, which no-ops an empty batch.)
func TestExportEmptyBatchStillPings(t *testing.T) {
	m, f := newFixture(t)
	if err := m.Export(context.Background(), nil); err != nil {
		t.Fatalf("empty batch Export must succeed: %v", err)
	}
	r := lastReq(t, f)
	if r.path != "/"+checkUUID {
		t.Errorf("an empty batch must still fire the liveness ping, got %q", r.path)
	}
}

// INV-15, the core contract: a dead/failed ping must SURFACE, never silently read healthy. When the ping host
// answers non-2xx (e.g. an unknown check), the module returns an error that names the status and echoes the
// response body — so the writer knows its liveness signal did not land, and Export fails up the stack.
func TestFailedPingSurfacesNotSilentlyHealthy(t *testing.T) {
	m, f := newFixture(t)
	f.status = http.StatusNotFound // 404 — Healthchecks.io response for an unknown check UUID.
	f.respRet = "not found"
	err := m.Ping(context.Background())
	if err == nil {
		t.Fatal("a non-2xx dead-man ping must be an error (INV-15: never silently read healthy)")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error must name the status, got %v", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error must surface the response body, got %v", err)
	}
	// The same failure must propagate through Export (a dead sink cannot report a successful export).
	f.status = http.StatusInternalServerError
	if err := m.Export(context.Background(), []observability.Sample{{Name: "x", Value: 1}}); err == nil {
		t.Fatal("Export must fail when the underlying liveness ping fails")
	}
}

// A transport-level failure (host unreachable) must propagate, not be swallowed into a healthy read.
func TestTransportErrorSurfaces(t *testing.T) {
	m, f := newFixture(t)
	f.netErr = errors.New("dial tcp: connection refused")
	if err := m.Ping(context.Background()); err == nil {
		t.Fatal("a transport error must surface as an error")
	}
}

// INV-13: the check id is a secret reference resolved PER PING, never captured as a literal at construction.
// Rotating the referenced secret between pings changes the pinged URL — proving the module dereferences the
// SecretRef on every call rather than snapshotting the value.
func TestCheckIDResolvedPerPingNeverLiteral(t *testing.T) {
	f := &fakeDoer{}
	m := New("https://hc-ping.test", config.SecretRef("env:TG_TEST_HC_ROTATE"), WithHTTPClient(f))

	t.Setenv("TG_TEST_HC_ROTATE", "uuid-one")
	if err := m.Ping(context.Background()); err != nil {
		t.Fatalf("first ping must succeed: %v", err)
	}
	if got := f.reqs[0].path; got != "/uuid-one" {
		t.Errorf("first ping path = %q, want /uuid-one", got)
	}

	t.Setenv("TG_TEST_HC_ROTATE", "uuid-two")
	if err := m.Ping(context.Background()); err != nil {
		t.Fatalf("second ping must succeed: %v", err)
	}
	if got := f.reqs[1].path; got != "/uuid-two" {
		t.Errorf("second ping path = %q, want /uuid-two (secret resolved per ping, not cached)", got)
	}
}

// A missing secret reference must fail closed: the module must NOT fall back to pinging the bare base URL
// (which could read healthy against an unrelated endpoint). No request may be issued at all.
func TestUnresolvedSecretIsErrorAndPingsNothing(t *testing.T) {
	f := &fakeDoer{}
	m := New("https://hc-ping.test", config.SecretRef("env:TG_TEST_HC_UNSET"), WithHTTPClient(f))
	if err := m.Ping(context.Background()); err == nil {
		t.Fatal("an unresolved check secret must be an error")
	}
	if len(f.reqs) != 0 {
		t.Errorf("no ping may be issued when the check secret is unresolved, got %d request(s)", len(f.reqs))
	}
}

// A base URL supplied with a trailing slash must not produce a double slash in the ping URL.
func TestBaseURLTrailingSlashTrimmed(t *testing.T) {
	t.Setenv("TG_TEST_HC_CHECK", checkUUID)
	f := &fakeDoer{}
	m := New("https://hc-ping.test/", config.SecretRef("env:TG_TEST_HC_CHECK"), WithHTTPClient(f))
	if err := m.Ping(context.Background()); err != nil {
		t.Fatalf("Ping must succeed: %v", err)
	}
	if got := f.reqs[0].path; got != "/"+checkUUID {
		t.Errorf("ping path = %q, want /%s (no double slash)", got, checkUUID)
	}
	if strings.Contains(f.reqs[0].rawURL, checkUUID) && strings.Contains(f.reqs[0].rawURL, "//"+checkUUID) {
		t.Errorf("ping URL must not contain a double slash before the check id, got %q", f.reqs[0].rawURL)
	}
}

func TestSourceType(t *testing.T) {
	m, _ := newFixture(t)
	if m.SourceType() != SourceType || SourceType != "healthchecks" {
		t.Errorf("SourceType() = %q / const %q, want healthchecks", m.SourceType(), SourceType)
	}
}
