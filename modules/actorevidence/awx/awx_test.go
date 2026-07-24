package awx

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeDoer serves canned JSON per request path so the tests drive the reader without a live AWX.
type fakeDoer struct {
	routes map[string]string // path (with query) -> JSON body; "" value in a matched prefix -> 404
	status map[string]int
	seen   []string
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	p := req.URL.Path
	if req.URL.RawQuery != "" {
		p += "?" + req.URL.RawQuery
	}
	f.seen = append(f.seen, p)
	// exact match first, then a path-only (query-insensitive) fallback
	body, ok := f.routes[p]
	if !ok {
		body, ok = f.routes[req.URL.Path]
	}
	code := 200
	if f.status != nil {
		if c, has := f.status[req.URL.Path]; has {
			code = c
		}
	}
	if !ok {
		code = 404
		body = `{"detail":"Not found."}`
	}
	if req.Header.Get("Authorization") != "Bearer test-token" {
		code, body = 401, `{"detail":"auth"}`
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func newReader(t *testing.T, f *fakeDoer) *Module {
	t.Helper()
	t.Setenv("AWX_TEST_TOKEN", "test-token")
	return New("https://awx.example", "env:AWX_TEST_TOKEN", WithHTTPClient(f))
}

func TestReadEmitsActorEvidenceForPlaybookRun(t *testing.T) {
	f := &fakeDoer{routes: map[string]string{
		"/api/v2/hosts/": `{"results":[{"id":42,"name":"web01"}]}`,
		"/api/v2/hosts/42/job_host_summaries/": `{"results":[
			{"summary_fields":{"job":{"id":31380}}},
			{"summary_fields":{"job":{"id":31000}}}]}`,
		// in-window user-launched job
		"/api/v2/jobs/31380/": `{"finished":"2026-07-24T10:30:15.79Z","status":"successful",
			"launched_by":{"name":"admin","type":"user"},
			"summary_fields":{"unified_job_template":{"name":"Deploy App"}}}`,
		// out-of-window job (older) — must be filtered out
		"/api/v2/jobs/31000/": `{"finished":"2026-07-01T00:00:00Z","status":"successful",
			"launched_by":{"name":"admin","type":"user"},
			"summary_fields":{"unified_job_template":{"name":"Old Job"}}}`,
	}}
	r := newReader(t, f)
	since := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC)
	ev, err := r.Read(context.Background(), "web01", since, until)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(ev) != 1 {
		t.Fatalf("expected 1 in-window evidence (the older job filtered out), got %d: %+v", len(ev), ev)
	}
	e := ev[0]
	if e.Domain != "awx" || e.Actor != "admin" || e.Target != "web01" || e.Ref != "31380" || !e.Covered {
		t.Fatalf("evidence fields wrong: %+v", e)
	}
	if e.ActionKind != "Deploy App/successful" {
		t.Fatalf("action_kind wrong: %q", e.ActionKind)
	}
}

// A non-user launcher (schedule/system) must be rendered as "<type>:<name>" so the automation nature
// survives into the deterministic attributor.
func TestReadRendersNonHumanLauncher(t *testing.T) {
	f := &fakeDoer{routes: map[string]string{
		"/api/v2/hosts/":                       `{"results":[{"id":1,"name":"web01"}]}`,
		"/api/v2/hosts/1/job_host_summaries/":  `{"results":[{"summary_fields":{"job":{"id":7}}}]}`,
		"/api/v2/jobs/7/": `{"finished":"2026-07-24T10:00:00Z","status":"successful",
			"launched_by":{"name":"Daily Cert Sync","type":"schedule"},
			"summary_fields":{"unified_job_template":{"name":"Cert Sync"}}}`,
	}}
	r := newReader(t, f)
	ev, err := r.Read(context.Background(), "web01",
		time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(ev) != 1 || ev[0].Actor != "schedule:Daily Cert Sync" {
		t.Fatalf("non-human launcher must render as schedule:<name>, got %+v", ev)
	}
}

// A job whose launcher AWX records as empty (null launched_by) yields NO evidence — an actor-evidence
// record with no principal is not admissible; it must not surface as a target-naming record with a blank actor.
func TestReadSkipsBlankActor(t *testing.T) {
	f := &fakeDoer{routes: map[string]string{
		"/api/v2/hosts/":                      `{"results":[{"id":1,"name":"web01"}]}`,
		"/api/v2/hosts/1/job_host_summaries/": `{"results":[{"summary_fields":{"job":{"id":9}}}]}`,
		"/api/v2/jobs/9/": `{"finished":"2026-07-24T10:00:00Z","status":"successful",
			"launched_by":{},"summary_fields":{"unified_job_template":{"name":"X"}}}`,
	}}
	r := newReader(t, f)
	ev, err := r.Read(context.Background(), "web01",
		time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(ev) != 0 {
		t.Fatalf("a blank-actor job must yield no evidence, got %+v", ev)
	}
}

// A still-running job (no `finished`) must NOT be attributed by its `created` time — a job that has not
// completed has not "run against" the host yet.
func TestReadSkipsRunningJob(t *testing.T) {
	f := &fakeDoer{routes: map[string]string{
		"/api/v2/hosts/":                      `{"results":[{"id":1,"name":"web01"}]}`,
		"/api/v2/hosts/1/job_host_summaries/": `{"results":[{"summary_fields":{"job":{"id":5}}}]}`,
		"/api/v2/jobs/5/": `{"finished":"","created":"2026-07-24T10:00:00Z","status":"running",
			"launched_by":{"name":"admin","type":"user"},"summary_fields":{"unified_job_template":{"name":"X"}}}`,
	}}
	r := newReader(t, f)
	ev, err := r.Read(context.Background(), "web01",
		time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(ev) != 0 {
		t.Fatalf("a still-running job must yield no evidence, got %+v", ev)
	}
}

// The walk follows DRF pagination — an in-window job on page 2 is not silently dropped.
func TestReadFollowsPagination(t *testing.T) {
	f := &fakeDoer{routes: map[string]string{
		"/api/v2/hosts/": `{"results":[{"id":1,"name":"web01"}]}`,
		"/api/v2/hosts/1/job_host_summaries/?order_by=-id&page_size=200": `{"next":"https://awx.example/api/v2/hosts/1/job_host_summaries/?page=2","results":[{"summary_fields":{"job":{"id":100}}}]}`,
		"/api/v2/hosts/1/job_host_summaries/?page=2":                     `{"next":"","results":[{"summary_fields":{"job":{"id":200}}}]}`,
		"/api/v2/jobs/100/": `{"finished":"2026-07-24T09:00:00Z","status":"successful","launched_by":{"name":"alice","type":"user"},"summary_fields":{"unified_job_template":{"name":"P1"}}}`,
		"/api/v2/jobs/200/": `{"finished":"2026-07-24T11:00:00Z","status":"successful","launched_by":{"name":"bob","type":"user"},"summary_fields":{"unified_job_template":{"name":"P2"}}}`,
	}}
	r := newReader(t, f)
	ev, err := r.Read(context.Background(), "web01",
		time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(ev) != 2 {
		t.Fatalf("both pages' in-window jobs must be returned (pagination followed), got %d: %+v", len(ev), ev)
	}
}

// A host AWX does not know is a fail-closed error (never a blank identity / empty-but-nil evidence).
func TestReadFailsClosedOnUnknownHost(t *testing.T) {
	f := &fakeDoer{routes: map[string]string{"/api/v2/hosts/": `{"results":[]}`}}
	r := newReader(t, f)
	if _, err := r.Read(context.Background(), "ghost", time.Time{}, time.Now()); err == nil {
		t.Fatal("an unknown host must fail closed")
	}
}

// An ambiguous host name (two AWX hosts share it) fails closed rather than guessing.
func TestReadFailsClosedOnAmbiguousHost(t *testing.T) {
	f := &fakeDoer{routes: map[string]string{
		"/api/v2/hosts/": `{"results":[{"id":1,"name":"web01"},{"id":2,"name":"web01"}]}`,
	}}
	r := newReader(t, f)
	if _, err := r.Read(context.Background(), "web01", time.Time{}, time.Now()); err == nil {
		t.Fatal("an ambiguous host must fail closed")
	}
}
