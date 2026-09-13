package cronicle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

const testAPIKey = "31eb2acc6de04cab9309900f3fbacacc"

// fakeCronicle is an in-process Cronicle mirroring the real REST shapes (verified live): POST
// /api/app/{get_schedule,get_event}/v1, X-API-Key auth, `code` 0-number success / string-code error, a
// `rows`+`list.length` schedule envelope, and a `event` single-event envelope. Its event set is mutable so a
// test can prove re-read-by-id (no cached mutation).
type fakeCronicle struct {
	mu     sync.Mutex
	events []map[string]any
	fail   bool // when true every request returns HTTP 500 (unreachable/broken scheduler)
	hits   int
}

func (f *fakeCronicle) setFail(v bool) { f.mu.Lock(); f.fail = v; f.mu.Unlock() }

func (f *fakeCronicle) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits++
		fail := f.fail
		events := f.events
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":"api","description":"server error"}`))
			return
		}
		if r.Header.Get("X-API-Key") != testAPIKey {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "api", "description": "Invalid API Key"})
			return
		}
		switch r.URL.Path {
		case "/api/app/get_schedule/v1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "rows": events, "list": map[string]any{"length": len(events)},
			})
		case "/api/app/get_event/v1":
			var body struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, ev := range events {
				if ev["id"] == body.ID {
					_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "event": ev})
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "api", "description": "Event not found"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// demoEvents mirrors the four events created on the live demo Cronicle.
func demoEvents() []map[string]any {
	return []map[string]any{
		{
			"id": "ev-maint", "title": "Nightly maintenance window (librespeed01)", "enabled": 1,
			"target": "allgrp", "timezone": "Europe/Amsterdam",
			"timing": map[string]any{"hours": []int{2}, "minutes": []int{0}},
			"notes":  "tg-window=maintenance tg-duration=3h tg-target=librespeed01\nSanctioned nightly change window.",
		},
		{
			"id": "ev-freeze", "title": "Nightly change moratorium", "enabled": 1,
			"target": "allgrp", "timezone": "Europe/Amsterdam",
			"timing": map[string]any{"hours": []int{3}, "minutes": []int{0}},
			"notes":  "tg-window=freeze tg-duration=1h tg-target=*\nHard change-freeze 03:00-04:00.",
		},
		{
			"id": "ev-job", "title": "Hourly log rotation (librespeed01)", "enabled": 1,
			"target": "allgrp", "timezone": "Europe/Amsterdam",
			"timing": map[string]any{"minutes": []int{15}},
			"notes":  "Routine hourly job (no TG directive).",
		},
		{
			"id": "ev-ondemand", "title": "On-demand rebuild", "enabled": 1,
			"target": "allgrp", "timezone": "Europe/Amsterdam",
			// no "timing" key => on-demand event
			"notes": "Manual only.",
		},
	}
}

func newProvider(t *testing.T, srv *httptest.Server) *Provider {
	t.Helper()
	t.Setenv("TG_CRONICLE_TEST_KEY", testAPIKey)
	c, err := New(Config{BaseURL: srv.URL, KeyRef: config.SecretRef("env:TG_CRONICLE_TEST_KEY"), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	p, err := NewProvider(ProviderConfig{Client: c, Source: "demo01"})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	return p
}

func amsterdam(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestProviderDerivesWindowsAndJobsLive(t *testing.T) {
	f := &fakeCronicle{events: demoEvents()}
	srv := f.server()
	defer srv.Close()
	p := newProvider(t, srv)

	cal, skips, err := p.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !cal.Readable {
		t.Fatal("snapshot should be readable")
	}
	if len(cal.MaintenanceWindows()) != 1 || len(cal.FreezeWindows()) != 1 {
		t.Fatalf("want 1 maintenance + 1 freeze window, got %d/%d (skips=%v)", len(cal.MaintenanceWindows()), len(cal.FreezeWindows()), skips)
	}
	// three enabled recurring events -> three jobs (on-demand has no timing, so it is not a job).
	if len(cal.Jobs) != 3 {
		t.Fatalf("want 3 scheduled jobs, got %d", len(cal.Jobs))
	}

	loc := amsterdam(t)
	at := func(h, m int) time.Time { return time.Date(2026, 7, 20, h, m, 0, 0, loc) }

	if in, reason := cal.MaintenanceWindow("librespeed01", at(2, 30)); !in {
		t.Errorf("02:30 should be in the sanctioned window: %s", reason)
	}
	if in, reason := cal.MaintenanceWindow("librespeed01", at(3, 30)); in {
		t.Errorf("03:30 is under the change-freeze; want NOT sanctioned: %s", reason)
	}
	if in, _ := cal.MaintenanceWindow("librespeed01", at(12, 0)); in {
		t.Error("12:00 should be outside every window")
	}
}

func TestProviderFailsClosedWhenUnreadable(t *testing.T) {
	f := &fakeCronicle{events: demoEvents()}
	srv := f.server()
	defer srv.Close()
	p := newProvider(t, srv)
	f.setFail(true) // the scheduler is now broken/unreachable

	in, reason := p.MaintenanceWindow(context.Background(), "librespeed01", time.Now())
	if in {
		t.Fatal("an unreadable schedule MUST report OUTSIDE the maintenance window (conservative fail-safe)")
	}
	if reason == "" {
		t.Fatal("expected a conservative reason")
	}
	// and a direct Snapshot surfaces the read error rather than a silent empty calendar.
	if _, _, err := p.Snapshot(context.Background()); err == nil {
		t.Fatal("Snapshot should error on an unreadable scheduler")
	}
}

func TestProviderReReadsByIdNoCachedMutation(t *testing.T) {
	f := &fakeCronicle{events: demoEvents()}
	srv := f.server()
	defer srv.Close()
	p := newProvider(t, srv)
	loc := amsterdam(t)
	at := func(h, m int) time.Time { return time.Date(2026, 7, 20, h, m, 0, 0, loc) }

	// initially the maintenance window is 02:00-05:00; 06:00 is outside.
	if in, _ := p.MaintenanceWindow(context.Background(), "librespeed01", at(6, 0)); in {
		t.Fatal("06:00 should be outside the initial window")
	}
	// the operator retimes the SAME event upstream (06:00 start). A re-read must reflect it (no cached copy).
	f.mu.Lock()
	f.events[0]["timing"] = map[string]any{"hours": []int{6}, "minutes": []int{0}}
	f.mu.Unlock()
	if in, reason := p.MaintenanceWindow(context.Background(), "librespeed01", at(6, 30)); !in {
		t.Fatalf("after re-read, 06:30 should be in the retimed window: %s", reason)
	}

	// re-read a single event by id and confirm the upstream change is visible.
	ev, err := p.client.Event(context.Background(), "ev-maint")
	if err != nil {
		t.Fatalf("get_event: %v", err)
	}
	if ev.Timing == nil || len(ev.Timing.Hours) != 1 || ev.Timing.Hours[0] != 6 {
		t.Fatalf("re-read event did not reflect the upstream retime: %+v", ev.Timing)
	}
}

func TestClientSendsAPIKeyHeaderAndFailsClosedOnBadKey(t *testing.T) {
	f := &fakeCronicle{events: demoEvents()}
	srv := f.server()
	defer srv.Close()
	// a client with the WRONG key must fail closed (the fake rejects a bad X-API-Key).
	_ = os.Setenv("TG_CRONICLE_BAD_KEY", "not-the-key")
	defer os.Unsetenv("TG_CRONICLE_BAD_KEY")
	c, err := New(Config{BaseURL: srv.URL, KeyRef: config.SecretRef("env:TG_CRONICLE_BAD_KEY"), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Schedule(context.Background()); err == nil {
		t.Fatal("a wrong API key must fail closed, not read the schedule")
	}
}

func TestParseDeployments(t *testing.T) {
	rows := ParseDeployments("demo01|http://127.0.0.1:3012|env:TG_CRONICLE_TOKEN|2h; ;bad|only-two-fields")
	if len(rows) != 1 {
		t.Fatalf("want 1 valid row (partial rows skipped), got %d: %+v", len(rows), rows)
	}
	d := rows[0]
	if d.ID != "demo01" || d.BaseURL != "http://127.0.0.1:3012" || d.KeyRef != "env:TG_CRONICLE_TOKEN" || d.DefaultDur != 2*time.Hour {
		t.Fatalf("unexpected parse: %+v", d)
	}
}
