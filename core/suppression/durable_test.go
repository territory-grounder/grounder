package suppression

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/persist"
)

// TG-225: a learned reboot schedule lived only in the worker's memory, so a restart forgot the lesson. The
// registry now mirrors every mutation to a durable store and rehydrates from it at boot. This oracle proves a
// PROMOTED lesson survives a restart WITH its timezone — the safety-critical field: a reloaded schedule whose
// timezone was lost would evaluate its DST-correct window in the wrong zone and suppress at the wrong times.
// Killing mutation: drop mirrorLocked from Promote → the reloaded row is only observing (or absent) → RED.
func TestLearnedScheduleSurvivesRestartWithTimezone(t *testing.T) {
	store := persist.NewMemScheduledReboots()
	const tz = "Europe/Amsterdam"
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load %s: %v", tz, err)
	}
	key := ScheduleKey{Host: "host-X", Kind: "reboot", Cron: "0 3 * * *"} // daily 03:00 local
	validFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	validUntil := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	reg1 := NewScheduleRegistry().WithDurableStore(store, nil)
	reg1.RegisterObserving(Schedule{
		Host: "host-X", Kind: "reboot", Cron: "0 3 * * *", Timezone: tz,
		Source: SourceLearned, ValidFrom: validFrom, ValidUntil: validUntil,
	})
	// Promote to live with the two distinct in-window boots the threshold needs (03:00 Amsterdam, two days).
	w := WindowEvaluator{PreBuffer: 30 * time.Minute, PostWindow: 30 * time.Minute}
	boots := []Boot{
		{At: time.Date(2026, 8, 2, 3, 0, 0, 0, loc)},
		{At: time.Date(2026, 8, 3, 3, 0, 0, 0, loc)},
	}
	if st := reg1.Promote(key, w, boots, true, now); st != SchLive {
		t.Fatalf("fixture did not promote to live (threshold %d), got %v", PromotionThreshold, st)
	}

	// RESTART: a fresh registry over the SAME durable store, its in-memory buffer gone.
	reg2 := NewScheduleRegistry().WithDurableStore(store, nil)
	if err := reg2.LoadFromStore(context.Background()); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	got, ok := reg2.Get(key)
	if !ok {
		t.Fatal("the learned schedule did NOT survive the restart — still in-process only (TG-225)")
	}
	if got.Status != SchLive {
		t.Fatalf("the schedule survived but not as LIVE (got %v) — a promoted lesson was lost on restart", got.Status)
	}
	if got.Timezone != tz {
		t.Fatalf("timezone LOST on reload: %q — the DST-correct window would evaluate in the wrong zone and suppress at the wrong times", got.Timezone)
	}
	// Functional proof the timezone round-tripped: the reloaded live schedule still suppresses inside its own
	// 03:00-Amsterdam window. (Had the timezone been lost, Contains would fail-closed and this would be false.)
	if !got.Suppresses(w, time.Date(2026, 8, 20, 3, 0, 0, 0, loc), now) {
		t.Fatal("the reloaded schedule does not suppress inside its own window — the timezone did not round-trip functionally")
	}

	// The unlearning half survives too: a demote persists, so a reloaded registry sees observing, not live.
	if !reg1.Demote(key) {
		t.Fatal("demote of the live schedule failed")
	}
	reg3 := NewScheduleRegistry().WithDurableStore(store, nil)
	if err := reg3.LoadFromStore(context.Background()); err != nil {
		t.Fatalf("rehydrate 2: %v", err)
	}
	if d, ok := reg3.Get(key); !ok || d.Status != SchObserving {
		t.Fatalf("a DEMOTED schedule must survive the restart as observing (the correction survives with the lesson), got ok=%v status=%v", ok, d.Status)
	}
}

// TG-225 (review finding #2): only the promotion COUNT is persisted, not the ObservedBoots slice, so a LIVE
// schedule reloaded from the store comes back with ObservedCount = its earned N but ObservedBoots = nil. The
// first post-restart Promote must NOT recompute the count down from the now-empty slice and silently reset the
// durable evidence trail (e.g. 2 → 1) — a live suppression decision must keep the evidence that earned it.
// Killing mutation: drop the "an already-live count can only grow" guard in Promote → the reloaded live row's
// count regresses below threshold on the next routine boot → RED.
func TestReloadedLiveScheduleKeepsEarnedObservedCount(t *testing.T) {
	store := persist.NewMemScheduledReboots()
	const tz = "Europe/Amsterdam"
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load %s: %v", tz, err)
	}
	key := ScheduleKey{Host: "host-Y", Kind: "reboot", Cron: "0 3 * * *"}
	validFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	validUntil := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	w := WindowEvaluator{PreBuffer: 30 * time.Minute, PostWindow: 30 * time.Minute}

	reg1 := NewScheduleRegistry().WithDurableStore(store, nil)
	reg1.RegisterObserving(Schedule{
		Host: "host-Y", Kind: "reboot", Cron: "0 3 * * *", Timezone: tz,
		Source: SourceLearned, ValidFrom: validFrom, ValidUntil: validUntil,
	})
	if st := reg1.Promote(key, w, []Boot{
		{At: time.Date(2026, 8, 2, 3, 0, 0, 0, loc)},
		{At: time.Date(2026, 8, 3, 3, 0, 0, 0, loc)},
	}, true, now); st != SchLive {
		t.Fatalf("fixture did not promote to live, got %v", st)
	}

	// RESTART: the live row reloads with ObservedCount=2 but ObservedBoots=nil (only the count is persisted).
	reg2 := NewScheduleRegistry().WithDurableStore(store, nil)
	if err := reg2.LoadFromStore(context.Background()); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	// A routine weekly Promote sees ONE fresh in-window boot. The status stays live either way (Promote never
	// demotes), so the regression only shows in the COUNT — assert it did not drop below what earned live.
	if st := reg2.Promote(key, w, []Boot{{At: time.Date(2026, 8, 10, 3, 0, 0, 0, loc)}}, true, now); st != SchLive {
		t.Fatalf("a reloaded live schedule must stay live after a routine boot, got %v", st)
	}
	got, ok := reg2.Get(key)
	if !ok {
		t.Fatal("the schedule vanished after reload+promote")
	}
	if got.ObservedCount < PromotionThreshold {
		t.Fatalf("a reloaded LIVE schedule's evidence count regressed to %d (< threshold %d) on the first post-restart Promote — the durable evidence trail backing a live suppression was silently reset", got.ObservedCount, PromotionThreshold)
	}
}
