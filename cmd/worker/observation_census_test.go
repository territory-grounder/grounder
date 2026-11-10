package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestObservationCensusJob_NilInputsEmitNothing(t *testing.T) {
	okFired := func(context.Context) (map[string]time.Time, error) { return nil, nil }
	if got := startObservationCensusJob(context.Background(), nil, okFired, time.Hour, 0, time.Now, edcQuiet)(); got != nil {
		t.Errorf("nil hostsFn emitted %v, want nil", got)
	}
	if got := startObservationCensusJob(context.Background(), func() []string { return []string{"h"} }, nil, time.Hour, 0, time.Now, edcQuiet)(); got != nil {
		t.Errorf("nil lastFiredFn emitted %v, want nil", got)
	}
}

// A fired-history read error at boot (no cache yet) must emit NOTHING (honest absence), never a phantom
// all-unobservable census. KILLING MUTATION: emit off a nil cache and the job reports the whole estate blind.
func TestObservationCensusJob_DBErrorAtBootEmitsNothing(t *testing.T) {
	errFn := func(context.Context) (map[string]time.Time, error) { return nil, errors.New("db down") }
	got := startObservationCensusJob(context.Background(), func() []string { return []string{"h1", "h2"} }, errFn, time.Hour, 0, time.Now, edcQuiet)()
	if got != nil {
		t.Errorf("a boot fired-history error emitted %v, want nil (honest absence, not a phantom all-unobservable census)", got)
	}
}

func TestObservationCensusJob_EmitsCensusFromFakes(t *testing.T) {
	now := time.Unix(100000, 0)
	lastFired := map[string]time.Time{
		"obs-host":   now.Add(-30 * time.Minute), // within the window → observed
		"quiet-host": now.Add(-48 * time.Hour),   // fired long ago → healthy_quiet
	}
	samples := startObservationCensusJob(context.Background(),
		func() []string { return []string{"obs-host", "quiet-host", "blind-host"} },
		func(context.Context) (map[string]time.Time, error) { return lastFired, nil },
		time.Hour, 0, func() time.Time { return now }, edcQuiet)()

	byState := map[string]float64{}
	var total float64
	for _, s := range samples {
		switch s.Name {
		case "tg_observation_census_hosts_total":
			total = s.Value
		case "tg_observation_census":
			byState[s.Labels["state"]] = s.Value
		}
	}
	if total != 3 {
		t.Errorf("tg_observation_census_hosts_total = %v, want 3", total)
	}
	if byState["observed"] != 1 || byState["healthy_quiet"] != 1 || byState["unobservable"] != 1 {
		t.Errorf("census by state = %v, want observed=1 healthy_quiet=1 unobservable=1", byState)
	}
}

// SLICE 1C: the host set is read LIVE on every scrape, not frozen at boot — a host appearing in the graph is
// censused on the next scrape without waiting for a redeploy. KILLING MUTATION: compute the samples once at
// boot and return them fixed (the slice-1 shape) and the second scrape still reports the boot host count.
func TestObservationCensusJob_ReadsHostsLiveAtScrape(t *testing.T) {
	now := time.Unix(100000, 0)
	hosts := []string{"h1"}
	reader := startObservationCensusJob(context.Background(),
		func() []string { return hosts },
		func(context.Context) (map[string]time.Time, error) { return map[string]time.Time{}, nil },
		time.Hour, 0, func() time.Time { return now }, edcQuiet)

	hostsTotal := func() float64 {
		for _, s := range reader() {
			if s.Name == "tg_observation_census_hosts_total" {
				return s.Value
			}
		}
		return -1
	}
	if got := hostsTotal(); got != 1 {
		t.Fatalf("first scrape host total = %v, want 1", got)
	}
	hosts = []string{"h1", "h2", "h3"} // the graph grew after boot
	if got := hostsTotal(); got != 3 {
		t.Errorf("second scrape host total = %v, want 3 — hosts must be read live at scrape, not frozen at boot", got)
	}
}
