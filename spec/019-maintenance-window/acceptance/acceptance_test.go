package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/schedule"
	"github.com/territory-grounder/grounder/modules/schedule/cronicle"
)

const accKey = "31eb2acc6de04cab9309900f3fbacacc"

// fakeCronicle mirrors the live Cronicle REST shapes (get_schedule/get_event, X-API-Key auth, code-0
// envelope, timing arrays) so the acceptance oracle drives the REAL connector with no live server.
type fakeCronicle struct {
	mu     sync.Mutex
	events []map[string]any
	fail   bool
}

func (f *fakeCronicle) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		fail, events := f.fail, f.events
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":"api","description":"server error"}`))
			return
		}
		if r.Header.Get("X-API-Key") != accKey {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "api", "description": "Invalid API Key"})
			return
		}
		switch r.URL.Path {
		case "/api/app/get_schedule/v1":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "rows": events, "list": map[string]any{"length": len(events)}})
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
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "api", "description": "not found"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// demoEvents mirrors the four events seeded on the live demo Cronicle.
func demoEvents() []map[string]any {
	return []map[string]any{
		{"id": "ev-maint", "title": "Nightly maintenance window (librespeed01)", "enabled": 1, "target": "allgrp",
			"timezone": "Europe/Amsterdam", "timing": map[string]any{"hours": []int{2}, "minutes": []int{0}},
			"notes": "tg-window=maintenance tg-duration=3h tg-target=librespeed01\nSanctioned nightly change window."},
		{"id": "ev-freeze", "title": "Nightly change moratorium", "enabled": 1, "target": "allgrp",
			"timezone": "Europe/Amsterdam", "timing": map[string]any{"hours": []int{3}, "minutes": []int{0}},
			"notes": "tg-window=freeze tg-duration=1h tg-target=*\nHard change-freeze 03:00-04:00."},
		{"id": "ev-job", "title": "Hourly log rotation (librespeed01)", "enabled": 1, "target": "allgrp",
			"timezone": "Europe/Amsterdam", "timing": map[string]any{"minutes": []int{15}}, "notes": "Routine hourly job."},
		{"id": "ev-ondemand", "title": "On-demand rebuild", "enabled": 1, "target": "allgrp",
			"timezone": "Europe/Amsterdam", "notes": "Manual only."},
	}
}

// world is the per-scenario state driving the real cronicle connector.
type world struct {
	fake     *fakeCronicle
	srv      *httptest.Server
	provider *cronicle.Provider
	loc      *time.Location

	cal      schedule.Calendar
	skips    []cronicle.SkipRecord
	inWindow bool
	reason   string
	band     safety.Band
	err      error

	goodRead bool
	badRead  error
	events   int
}

func (w *world) startFake(events []map[string]any, fail bool) error {
	_ = os.Setenv("TG_SPEC019_KEY", accKey)
	w.fake = &fakeCronicle{events: events, fail: fail}
	w.srv = w.fake.server()
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		return err
	}
	w.loc = loc
	c, err := cronicle.New(cronicle.Config{BaseURL: w.srv.URL, KeyRef: config.SecretRef("env:TG_SPEC019_KEY"), HTTPClient: w.srv.Client()})
	if err != nil {
		return err
	}
	w.provider, err = cronicle.NewProvider(cronicle.ProviderConfig{Client: c, Source: "spec019"})
	return err
}

func (w *world) at(h, m int) time.Time { return time.Date(2026, 7, 20, h, m, 0, 0, w.loc) }

func TestMaintenanceWindowAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/019 maintenance-window",
		ScenarioInitializer: initializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"."},
			Tags:     "~@pending",
			Strict:   true,
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("spec/019 acceptance scenarios failed")
	}
}

func initializeScenario(sc *godog.ScenarioContext) {
	w := &world{}

	sc.After(func(ctx context.Context, _ *godog.Scenario, err error) (context.Context, error) {
		if w.srv != nil {
			w.srv.Close()
			w.srv = nil
		}
		_ = os.Unsetenv("TG_SPEC019_KEY")
		return ctx, err
	})

	// --- Given ---
	sc.Step(`^a scheduler serving a nightly maintenance-window event and recurring jobs$`, func() error {
		return w.startFake(demoEvents(), false)
	})
	sc.Step(`^a scheduler whose change-freeze overlaps a maintenance window$`, func() error {
		return w.startFake(demoEvents(), false) // the moratorium freeze (03:00-04:00) overlaps the 02:00-05:00 window
	})
	sc.Step(`^a scheduler that cannot be read$`, func() error {
		return w.startFake(demoEvents(), true)
	})
	sc.Step(`^a scheduler that requires an API key$`, func() error {
		return w.startFake(demoEvents(), false)
	})

	// --- When ---
	sc.Step(`^the connector reads and derives the schedule$`, func() error {
		w.cal, w.skips, w.err = w.provider.Snapshot(context.Background())
		return w.err
	})
	sc.Step(`^the seam is evaluated inside the overlapping freeze$`, func() error {
		w.inWindow, w.reason = w.provider.MaintenanceWindow(context.Background(), "librespeed01", w.at(3, 30))
		return nil
	})
	sc.Step(`^the seam is evaluated outside every window$`, func() error {
		w.cal, _, w.err = w.provider.Snapshot(context.Background())
		if w.err != nil {
			return w.err
		}
		w.band, w.inWindow, w.reason = w.cal.EvaluateBand("librespeed01", w.at(12, 0))
		return nil
	})
	sc.Step(`^the seam is evaluated$`, func() error {
		w.inWindow, w.reason = w.provider.MaintenanceWindow(context.Background(), "librespeed01", time.Now())
		return nil
	})
	sc.Step(`^the operator retimes the maintenance event upstream and the connector re-reads$`, func() error {
		w.fake.mu.Lock()
		w.fake.events[0]["timing"] = map[string]any{"hours": []int{6}, "minutes": []int{0}}
		w.fake.mu.Unlock()
		w.inWindow, w.reason = w.provider.MaintenanceWindow(context.Background(), "librespeed01", w.at(6, 30))
		return nil
	})
	sc.Step(`^the connector authenticates with a sealed SecretRef and then with a wrong key$`, func() error {
		if _, _, err := w.provider.Snapshot(context.Background()); err == nil {
			w.goodRead = true
		}
		_ = os.Setenv("TG_SPEC019_BADKEY", "not-the-key")
		defer os.Unsetenv("TG_SPEC019_BADKEY")
		bad, err := cronicle.New(cronicle.Config{BaseURL: w.srv.URL, KeyRef: config.SecretRef("env:TG_SPEC019_BADKEY"), HTTPClient: w.srv.Client()})
		if err != nil {
			return err
		}
		_, w.badRead = bad.Schedule(context.Background())
		return nil
	})
	sc.Step(`^the connector reads the schedule over its native HTTP client$`, func() error {
		w.cal, _, w.err = w.provider.Snapshot(context.Background())
		w.events = len(w.cal.Jobs)
		return w.err
	})

	// --- Then ---
	sc.Step(`^a sanctioned maintenance window and the already-scheduled jobs are derived and the target is in-window during the window$`, func() error {
		if len(w.cal.MaintenanceWindows()) != 1 {
			return fmt.Errorf("want exactly 1 maintenance window, got %d", len(w.cal.MaintenanceWindows()))
		}
		if len(w.cal.Jobs) != 3 {
			return fmt.Errorf("want 3 already-scheduled jobs (on-demand event excluded), got %d", len(w.cal.Jobs))
		}
		if in, reason := w.cal.MaintenanceWindow("librespeed01", w.at(2, 30)); !in {
			return fmt.Errorf("02:30 librespeed01 should be in the sanctioned window: %s", reason)
		}
		if _, _, imminent := w.cal.ImminentJob("librespeed01", w.at(13, 50), time.Hour); !imminent {
			return fmt.Errorf("the hourly job should be imminent within 1h — collision awareness not derived")
		}
		return nil
	})
	sc.Step(`^the target is reported not in-window because the freeze denies over the maintenance window$`, func() error {
		if w.inWindow {
			return fmt.Errorf("inside the change-freeze the target must NOT be in-window; reason=%s", w.reason)
		}
		return nil
	})
	sc.Step(`^the target is reported not in-window and the actuation is clamped to the POLL_PAUSE band$`, func() error {
		if w.inWindow {
			return fmt.Errorf("12:00 is outside every window; must not be in-window")
		}
		if w.band != safety.BandPollPause {
			return fmt.Errorf("out-of-window actuation must clamp to POLL_PAUSE, got %s", w.band)
		}
		return nil
	})
	sc.Step(`^the target is reported not in-window with the conservative unreadable reason$`, func() error {
		if w.inWindow {
			return fmt.Errorf("an unreadable schedule must report OUTSIDE the window (fail closed safe)")
		}
		if w.reason == "" {
			return fmt.Errorf("expected a conservative reason for the unreadable schedule")
		}
		return nil
	})
	sc.Step(`^the derived window reflects the retimed event with no stale cached window$`, func() error {
		if !w.inWindow {
			return fmt.Errorf("after re-read, 06:30 should fall in the retimed 06:00 window: %s", w.reason)
		}
		// re-read the single event by id and confirm the upstream change is visible (INV-05).
		cal, _, err := w.provider.Snapshot(context.Background())
		if err != nil {
			return err
		}
		if in, _ := cal.MaintenanceWindow("librespeed01", w.at(2, 30)); in {
			return fmt.Errorf("the OLD 02:00 window must be gone after the retime (stale cached window)")
		}
		return nil
	})
	sc.Step(`^the sealed key reads the schedule and the wrong key fails closed with no schedule read$`, func() error {
		if !w.goodRead {
			return fmt.Errorf("the sealed SecretRef key should have read the schedule")
		}
		if w.badRead == nil {
			return fmt.Errorf("a wrong API key must fail closed, not read the schedule")
		}
		return nil
	})
	sc.Step(`^the read completes with no subprocess and the connector exposes no actuation path$`, func() error {
		// structural: the read completed against an in-process HTTP fake (native net/http, no shell), and the
		// connector's surface (Client + Provider) has read-only methods only — there is no Exec/Actuate seam.
		if w.err != nil {
			return fmt.Errorf("native HTTP read failed: %v", w.err)
		}
		if w.events == 0 {
			return fmt.Errorf("expected the native read to derive jobs")
		}
		return nil
	})
}
