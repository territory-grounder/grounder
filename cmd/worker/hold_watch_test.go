package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/holds"
	"github.com/territory-grounder/grounder/core/metrics"
)

type fakeHoldWatchStore struct {
	open   []holds.HoldDecision
	re     map[int64]time.Time
	failed []holds.FailedInvestigation
	err    error
}

func (f *fakeHoldWatchStore) OpenHolds(context.Context, time.Time) ([]holds.HoldDecision, error) {
	return f.open, f.err
}
func (f *fakeHoldWatchStore) HoldRedetections(context.Context, time.Time) (map[int64]time.Time, error) {
	return f.re, f.err
}
func (f *fakeHoldWatchStore) FailedInvestigationsUnretried(context.Context, time.Time) ([]holds.FailedInvestigation, error) {
	return f.failed, f.err
}

func holdSample(t *testing.T, ss []metrics.Sample, name string) (metrics.Sample, bool) {
	t.Helper()
	for _, s := range ss {
		if s.Name == name {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

// Three open holds (two sticky): one decided 3h ago and re-fired 5 minutes ago (STILL RAISING), one re-fired
// once 3h ago and quiet since (NOT raising — the incident resolved), one never re-fired; two unretried failures.
// KILLING MUTATIONS: drop the recency bound (redetected reads 2 and the oldest becomes the quiet one), count
// every open hold as re-detected (3), drop the sticky filter (sticky reads 3), measure the age from ExpiresAt
// (oldest reads ~21h).
func TestHoldWatchSamplesCountOpenStickyRedetectedAndFailed(t *testing.T) {
	now := time.Date(2026, 11, 7, 22, 0, 0, 0, time.UTC)
	open := []holds.HoldDecision{
		{Seq: 1, ReasonKey: "actor-attributed-authorized", DecidedAt: now.Add(-3 * time.Hour), ExpiresAt: now.Add(21 * time.Hour)},
		{Seq: 2, ReasonKey: "rationale-names-a-different-host", DecidedAt: now.Add(-10 * time.Minute), ExpiresAt: now.Add(23 * time.Hour)},
		{Seq: 3, ReasonKey: "human-deny", DecidedAt: now.Add(-5 * time.Hour), ExpiresAt: now.Add(19 * time.Hour)},
	}
	re := map[int64]time.Time{1: now.Add(-5 * time.Minute), 3: now.Add(-3 * time.Hour)}
	failed := []holds.FailedInvestigation{{ExternalRef: "a"}, {ExternalRef: "b"}}
	ss := holdWatchSamples(open, re, failed, now)
	want := map[string]float64{
		"tg_hold_open": 3, "tg_hold_sticky_open": 2, "tg_hold_redetected_open": 1,
		"tg_failed_investigation_unretried": 2, "tg_hold_redetected_oldest_seconds": 3 * 3600,
		"tg_hold_watch_last_success_timestamp_seconds": float64(now.Unix()),
	}
	for name, v := range want {
		s, ok := holdSample(t, ss, name)
		if !ok {
			t.Fatalf("%s missing from %+v", name, ss)
		}
		if s.Value != v {
			t.Errorf("%s = %v, want %v", name, s.Value, v)
		}
	}
}

// A quiet estate is a READING, not an absence: the depth gauges are published at zero, and the age gauge is
// not published at all (a zero there would read as "held this instant").
func TestHoldWatchQuietEstateEmitsZeroDepthAndNoAge(t *testing.T) {
	ss := holdWatchSamples(nil, nil, nil, time.Now())
	for _, name := range []string{"tg_hold_open", "tg_hold_sticky_open", "tg_hold_redetected_open", "tg_failed_investigation_unretried"} {
		s, ok := holdSample(t, ss, name)
		if !ok || s.Value != 0 {
			t.Errorf("%s must be published at 0 on a quiet estate, got ok=%v %+v", name, ok, s)
		}
	}
	if _, ok := holdSample(t, ss, "tg_hold_redetected_oldest_seconds"); ok {
		t.Error("with no re-detected hold there is no age to publish — a 0 would read as 'held this instant'")
	}
}

// A read error keeps the previous reading. Driven through the refresh seam, because reading the held pointer
// after setting the store to fail asserts nothing on its own.
func TestHoldWatchReadErrorKeepsThePreviousReading(t *testing.T) {
	store := &fakeHoldWatchStore{open: []holds.HoldDecision{{Seq: 1, ReasonKey: "human-deny", DecidedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}}
	reader, refresh := newHoldWatchJob(context.Background(), store)
	if refresh == nil {
		t.Fatal("a store must yield a drivable refresh")
	}
	if s, ok := holdSample(t, reader(), "tg_hold_open"); !ok || s.Value != 1 {
		t.Fatalf("first reading must be published immediately, got %+v", reader())
	}
	store.err = errors.New("db: connection reset")
	store.open = nil
	refresh()
	if s, ok := holdSample(t, reader(), "tg_hold_open"); !ok || s.Value != 1 {
		t.Fatalf("a read error must keep the previous reading (1), got %+v — zeroing it reads as 'every hold released'", reader())
	}
	store.err = nil
	store.open = nil
	refresh()
	if s, ok := holdSample(t, reader(), "tg_hold_open"); !ok || s.Value != 0 {
		t.Fatalf("a successful empty read then publishes 0, got %+v", reader())
	}
}

// No store: nothing to tick and nothing emitted — never a fabricated row of zeros, which would make "no
// watcher" and "no holds" read alike.
func TestHoldWatchWithoutAStoreEmitsNothing(t *testing.T) {
	reader, refresh := newHoldWatchJob(context.Background(), nil)
	if refresh != nil {
		t.Fatal("no store ⇒ no refresh to tick")
	}
	if got := reader(); len(got) != 0 {
		t.Fatalf("no store must emit NO series, got %+v", got)
	}
	if holdWatchStoreOrNil(nil) != nil {
		t.Fatal("a nil pool must be a nil reader, not a typed nil that the job would then call")
	}
}
