package cronicle

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// TestLiveCronicle is an env-gated integration test against a REAL Cronicle (the demo instance on
// dc1tg01). It is SKIPPED unless TG_CRONICLE_LIVE_URL + TG_CRONICLE_LIVE_KEY are set, so CI (which has
// neither) never runs it. It proves the connector derives windows from live events and fails closed-safe
// against a dead endpoint. Run it via an SSH tunnel to the loopback-only demo:
//
//	ssh -N -L 3012:127.0.0.1:3012 root@dc1tg01 &
//	TG_CRONICLE_LIVE_URL=http://127.0.0.1:3012 TG_CRONICLE_LIVE_KEY=<key> \
//	  go test ./modules/schedule/cronicle/ -run TestLiveCronicle -v
func TestLiveCronicle(t *testing.T) {
	base := os.Getenv("TG_CRONICLE_LIVE_URL")
	key := os.Getenv("TG_CRONICLE_LIVE_KEY")
	if base == "" || key == "" {
		t.Skip("set TG_CRONICLE_LIVE_URL and TG_CRONICLE_LIVE_KEY to run the live Cronicle integration test")
	}
	t.Setenv("TG_CRONICLE_LIVE_KEY_ENV", key)

	c, err := New(Config{BaseURL: base, KeyRef: config.SecretRef("env:TG_CRONICLE_LIVE_KEY_ENV")})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	p, err := NewProvider(ProviderConfig{Client: c, Source: "demo01"})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	cal, skips, err := p.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("live snapshot: %v", err)
	}
	if !cal.Readable {
		t.Fatal("live schedule should be readable")
	}
	t.Logf("live derived: %d maintenance window(s), %d freeze window(s), %d scheduled job(s), %d skip(s)",
		len(cal.MaintenanceWindows()), len(cal.FreezeWindows()), len(cal.Jobs), len(skips))
	for _, w := range cal.MaintenanceWindows() {
		t.Logf("  maintenance window: %q target=%s event=%s dur=%s", w.Title, w.Target, w.EventID, w.Duration)
	}
	for _, w := range cal.FreezeWindows() {
		t.Logf("  freeze window: %q target=%s event=%s dur=%s", w.Title, w.Target, w.EventID, w.Duration)
	}
	if len(cal.MaintenanceWindows()) == 0 {
		t.Fatal("expected at least one maintenance window derived from the live demo events")
	}

	// evaluate the seam at chosen timestamps to exercise every state (the demo events are Amsterdam-tz).
	loc, _ := time.LoadLocation("Europe/Amsterdam")
	states := []struct {
		label  string
		target string
		when   time.Time
		want   bool
	}{
		{"inside maintenance (02:30)", "librespeed01", time.Date(2026, 7, 21, 2, 30, 0, 0, loc), true},
		{"inside freeze carve-out (03:30)", "librespeed01", time.Date(2026, 7, 21, 3, 30, 0, 0, loc), false},
		{"outside all windows (20:00)", "librespeed01", time.Date(2026, 7, 21, 20, 0, 0, 0, loc), false},
	}
	for _, s := range states {
		in, reason := cal.MaintenanceWindow(s.target, s.when)
		t.Logf("  %s -> inWindow=%v (%s)", s.label, in, reason)
		if in != s.want {
			t.Errorf("%s: inWindow=%v want %v", s.label, in, s.want)
		}
	}

	// fail-closed-safe: a client pointed at a dead port reports OUTSIDE the window conservatively.
	dead, _ := New(Config{BaseURL: "http://127.0.0.1:1", KeyRef: config.SecretRef("env:TG_CRONICLE_LIVE_KEY_ENV")})
	deadProv, _ := NewProvider(ProviderConfig{Client: dead, Source: "dead"})
	if in, reason := deadProv.MaintenanceWindow(context.Background(), "librespeed01", time.Now()); in {
		t.Fatalf("an unreadable scheduler must report OUTSIDE the window; got inWindow=true (%s)", reason)
	} else {
		t.Logf("  fail-safe (dead scheduler) -> inWindow=false (%s)", reason)
	}
}
