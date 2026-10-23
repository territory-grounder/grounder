package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/persist"
)

// TG-225: the pgx ScheduledReboots store gains Save (OVERWRITE — the registry-mirror write) + List (boot
// rehydrate) + a timezone column (migration 0073). This oracle (gated on TG_TEST_POSTGRES_DSN; it Migrates
// the empty db itself) proves against REAL Postgres that a learned schedule persists WITH its timezone and
// that Save overwrites promotion state (unlike Register, which preserves it) — so a demote reaches durability.
func TestScheduledRebootsSaveListRoundTripTimezone(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the durable scheduled-reboots oracle")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	s := NewScheduledReboots(p)

	host := "sr-" + t.Name()
	sr := persist.ScheduledReboot{
		Host: host, Kind: "reboot", Cron: "0 3 * * *", Timezone: "Europe/Amsterdam",
		State: persist.SRLive, Observations: 2, KillSwitch: false,
		ValidFrom:      time.Now().UTC().Add(-time.Hour),
		ValidUntil:     time.Now().UTC().Add(30 * 24 * time.Hour),
		LastVerifiedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := s.Save(ctx, sr); err != nil {
		t.Fatalf("save (live): %v", err)
	}
	// Save must OVERWRITE promotion state (unlike Register, which preserves it): a demote must reach durability.
	sr.State = persist.SRObserving
	if err := s.Save(ctx, sr); err != nil {
		t.Fatalf("save (demote): %v", err)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *persist.ScheduledReboot
	for i := range list {
		if list[i].Host == host {
			found = &list[i]
			break
		}
	}
	if found == nil {
		t.Fatal("Save did not persist the row, or List did not return it")
	}
	if found.Timezone != "Europe/Amsterdam" {
		t.Fatalf("timezone LOST through the pgx round-trip: %q — a reloaded window would be wrong-zone", found.Timezone)
	}
	if found.State != persist.SRObserving {
		t.Fatalf("Save must OVERWRITE state (got %v, want observing) — a demote that only lived in memory does not survive", found.State)
	}
	if found.Observations != 2 {
		t.Errorf("observations = %d, want 2 (round-trip)", found.Observations)
	}
}
