package main

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/metrics"
)

// The REAL in-memory twin CI already uses (core/breaker.MemStore), not a stub. It implements the same
// Store contract the pgx store does, so this exercises the actual Save/List round-trip rather than a
// hand-written List that could silently drop a field — which is the defect class this whole change is about.
func storeWith(t *testing.T, recs ...breaker.Record) breaker.Store {
	t.Helper()
	m := breaker.NewMemStore()
	for _, r := range recs {
		if err := m.Save(context.Background(), r); err != nil {
			t.Fatalf("seed %s: %v", r.Name, err)
		}
	}
	return m
}

func brkSample(ss []metrics.Sample, name, label string) (metrics.Sample, bool) {
	for _, s := range ss {
		if s.Name == name && s.Labels["name"] == label {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

// ★ A LATCHED TRIP MUST SAY SINCE WHEN (TG-347).
//
// circuit_breaker_state reported OPEN and nothing reported for how long, so a trip that fired days ago on a
// recovered dependency was indistinguishable from one that fired a minute ago on a real fault. Measured
// 2026-08-06: judge-death read OPEN on both planes for days while the judge was demonstrably alive, and
// every skill-trial graduation was refused for the whole period. rec.OpenedAt was already on the record.
func TestAnOpenBreakerPublishesWhenItOpened(t *testing.T) {
	opened := time.Now().Add(-72 * time.Hour).UTC()
	adm := (&workerAdmin{}).withBreakerStore(storeWith(t,
		breaker.Record{Name: "judge-death", State: breaker.StateOpen, OpenedAt: opened, FailureCount: 3}))

	ss := adm.samples()
	got, ok := brkSample(ss, "circuit_breaker_opened_at_seconds", "judge-death")
	if !ok {
		t.Fatal("an OPEN breaker published no opened-at series. Without it a three-day latch and a " +
			"one-minute trip are the same reading, which is exactly how judge-death blocked every skill " +
			"graduation for days without anyone being able to see it was stale.")
	}
	if int64(got.Value) != opened.Unix() {
		t.Errorf("opened_at = %v, want %v", int64(got.Value), opened.Unix())
	}
}

// CLOSED MUST MEAN ABSENT, not a 1970 timestamp. OpenedAt is documented as "zero unless State == StateOpen",
// so publishing it unconditionally exports epoch-zero for every healthy breaker — which every dashboard
// renders as an ancient trip, and which would make the latched-open alert fire on closed breakers forever.
func TestAClosedBreakerPublishesNoOpenedAt(t *testing.T) {
	adm := (&workerAdmin{}).withBreakerStore(storeWith(t,
		breaker.Record{Name: "model-fast", State: breaker.StateClosed}))
	if s, ok := brkSample(adm.samples(), "circuit_breaker_opened_at_seconds", "model-fast"); ok {
		t.Errorf("a CLOSED breaker published opened_at = %v. Absent means closed; a 1970 value would make "+
			"CircuitBreakerLatchedOpen fire on every healthy breaker and be silenced within a day.", s.Value)
	}
}

// The state gauge must keep working exactly as before — this change adds a series, it does not alter one.
func TestTheStateGaugeIsUnchanged(t *testing.T) {
	adm := (&workerAdmin{}).withBreakerStore(storeWith(t,
		breaker.Record{Name: "judge-death", State: breaker.StateOpen, OpenedAt: time.Now()}))
	s, ok := brkSample(adm.samples(), "circuit_breaker_state", "judge-death")
	if !ok || s.Value != 2 {
		t.Errorf("circuit_breaker_state{judge-death} = %v (present=%v), want 2 (open)", s.Value, ok)
	}
}
