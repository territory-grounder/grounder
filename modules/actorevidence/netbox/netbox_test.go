package netbox

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fakeDoer struct {
	routes map[string]string // req.URL.Path -> JSON body
	seen   []string
	auth   string
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	full := req.URL.Path
	if req.URL.RawQuery != "" {
		full += "?" + req.URL.RawQuery
	}
	f.seen = append(f.seen, full)
	f.auth = req.Header.Get("Authorization")
	body, ok := f.routes[full] // exact (path+query) first, then a path-only fallback
	if !ok {
		body, ok = f.routes[req.URL.Path]
	}
	code := 200
	if !ok {
		code, body = 404, `{"detail":"Not found."}`
	}
	if f.auth != "Token test-token" {
		code, body = 403, `{"detail":"auth"}`
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func newReader(t *testing.T, f *fakeDoer) *Module {
	t.Helper()
	t.Setenv("NB_TEST_TOKEN", "test-token")
	return New("https://netbox.example", "env:NB_TEST_TOKEN", WithHTTPClient(f))
}

func TestReadEmitsActorEvidenceForInventoryChange(t *testing.T) {
	f := &fakeDoer{routes: map[string]string{
		"/api/dcim/devices/": `{"results":[{"id":18,"name":"ankh"}]}`,
		"/api/core/object-changes/": `{"results":[
			{"id":43409,"time":"2026-07-24T02:00:42.05Z","user_name":"Admin","user":{"username":"Admin"},"action":{"value":"update"}},
			{"id":40000,"time":"2026-07-01T00:00:00Z","user_name":"Bob","user":{"username":"Bob"},"action":{"value":"create"}}]}`,
	}}
	r := newReader(t, f)
	since := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC)
	ev, err := r.Read(context.Background(), "ankh", since, until)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(ev) != 1 {
		t.Fatalf("expected 1 in-window change (older filtered), got %d: %+v", len(ev), ev)
	}
	e := ev[0]
	if e.Domain != "netbox" || e.Actor != "Admin" || e.ActionKind != "update" || e.Target != "ankh" || e.Ref != "43409" || !e.Covered {
		t.Fatalf("evidence fields wrong: %+v", e)
	}
	// the v4.5.5 workaround: the changelog request MUST use the sparse fieldset (never the full/broken rep)
	var q string
	for _, s := range f.seen {
		if strings.HasPrefix(s, "/api/core/object-changes/") {
			q = s
		}
	}
	if !strings.Contains(q, "fields=id%2Ctime%2Caction%2Cuser%2Cuser_name") {
		t.Fatalf("changelog request must use the sparse fieldset (v4.5.5 500 workaround), got %q", q)
	}
	if !strings.Contains(q, "changed_object_id=18") {
		t.Fatalf("changelog must be scoped to the target device id, got %q", q)
	}
	if f.auth != "Token test-token" {
		t.Fatalf("NetBox uses the Token auth scheme, got %q", f.auth)
	}
}

func TestReadFallsBackToUserNameWhenUserObjectAbsent(t *testing.T) {
	f := &fakeDoer{routes: map[string]string{
		"/api/dcim/devices/":         `{"results":[{"id":1,"name":"ankh"}]}`,
		"/api/core/object-changes/":  `{"results":[{"id":9,"time":"2026-07-24T10:00:00Z","user_name":"svc-import","action":{"value":"delete"}}]}`,
	}}
	r := newReader(t, f)
	ev, err := r.Read(context.Background(), "ankh",
		time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(ev) != 1 || ev[0].Actor != "svc-import" || ev[0].ActionKind != "delete" {
		t.Fatalf("must fall back to user_name when the user object is absent, got %+v", ev)
	}
}

// A change NetBox records with no user at all (no user object, no user_name) yields NO evidence — a
// target-naming record with a blank actor is not admissible.
func TestReadSkipsBlankActor(t *testing.T) {
	f := &fakeDoer{routes: map[string]string{
		"/api/dcim/devices/":        `{"results":[{"id":1,"name":"ankh"}]}`,
		"/api/core/object-changes/": `{"results":[{"id":9,"time":"2026-07-24T10:00:00Z","action":{"value":"update"}}]}`,
	}}
	r := newReader(t, f)
	ev, err := r.Read(context.Background(), "ankh",
		time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(ev) != 0 {
		t.Fatalf("a change with no actor must yield no evidence, got %+v", ev)
	}
}

// The reader follows DRF pagination — an in-window change on page 2 is not silently dropped.
func TestReadFollowsPagination(t *testing.T) {
	f := &fakeDoer{routes: map[string]string{
		"/api/dcim/devices/": `{"results":[{"id":1,"name":"ankh"}]}`,
		// page 1 matched via the path-only fallback (the real first request also carries time_after/before)
		"/api/core/object-changes/":        `{"next":"https://netbox.example/api/core/object-changes/?page=2","results":[{"id":1,"time":"2026-07-24T09:00:00Z","user_name":"a","action":{"value":"update"}}]}`,
		"/api/core/object-changes/?page=2": `{"next":"","results":[{"id":2,"time":"2026-07-24T11:00:00Z","user_name":"b","action":{"value":"delete"}}]}`,
	}}
	r := newReader(t, f)
	ev, err := r.Read(context.Background(), "ankh",
		time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(ev) != 2 {
		t.Fatalf("both pages' changes must be returned (pagination followed), got %d: %+v", len(ev), ev)
	}
}

func TestReadFailsClosedOnUnknownDevice(t *testing.T) {
	f := &fakeDoer{routes: map[string]string{"/api/dcim/devices/": `{"results":[]}`}}
	r := newReader(t, f)
	if _, err := r.Read(context.Background(), "ghost", time.Time{}, time.Now()); err == nil {
		t.Fatal("an unknown device must fail closed")
	}
}

func TestReadFailsClosedOnAmbiguousDevice(t *testing.T) {
	f := &fakeDoer{routes: map[string]string{
		"/api/dcim/devices/": `{"results":[{"id":1,"name":"ankh"},{"id":2,"name":"ankh"}]}`,
	}}
	r := newReader(t, f)
	if _, err := r.Read(context.Background(), "ankh", time.Time{}, time.Now()); err == nil {
		t.Fatal("an ambiguous device must fail closed")
	}
}
