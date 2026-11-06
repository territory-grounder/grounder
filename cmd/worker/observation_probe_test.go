package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/metrics"
)

// gaugeVal pulls a single-value gauge out of a sample slice; -1 if absent.
func gaugeVal(samples []metrics.Sample, name string) float64 {
	for _, s := range samples {
		if s.Name == name {
			return s.Value
		}
	}
	return -1
}

func probeCensusFakes() (func() []string, func(context.Context) (map[string]time.Time, error)) {
	now := time.Unix(100000, 0)
	// obs fired recently (observed); gap1/gap2 never fired (unobservable).
	fired := map[string]time.Time{"obs": now.Add(-10 * time.Minute)}
	return func() []string { return []string{"obs", "gap1", "gap2"} },
		func(context.Context) (map[string]time.Time, error) { return fired, nil }
}

var probeNow = func() time.Time { return time.Unix(100000, 0) }

// DEFAULT-OFF at the worker: with TG_OBSERVE_PROBE_ENABLED unset (enabled=false), the posture gauge is 0, the
// coverage DIMENSION still ships (denominator from the live census), and the numerator is 0 — "0 of N
// unobservable entities probe-confirmed". No injection path is constructed here at all.
func TestObservationProbeJob_DefaultOff_PublishesCoverageWithZeroNumerator(t *testing.T) {
	hostsFn, firedFn := probeCensusFakes()
	confirmed := func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }

	collect := startObservationProbeJob(context.Background(), hostsFn, firedFn, confirmed, time.Hour, 0, false, probeNow, edcQuiet, nil)
	s := collect()

	if got := gaugeVal(s, "tg_observation_probe_enabled"); got != 0 {
		t.Fatalf("enabled gauge = %v, want 0 (default-OFF)", got)
	}
	if got := gaugeVal(s, "tg_observation_probe_unobservable_total"); got != 2 {
		t.Fatalf("denominator = %v, want 2 (gap1, gap2 never fired)", got)
	}
	if got := gaugeVal(s, "tg_observation_probe_confirmed_total"); got != 0 {
		t.Fatalf("numerator = %v, want 0 (nothing probed while disarmed)", got)
	}
	if got := gaugeVal(s, "tg_observation_probe_coverage_ratio"); got != 0 {
		t.Fatalf("coverage ratio = %v, want 0", got)
	}
}

// The posture gauge tracks the arming flag.
func TestObservationProbeJob_EnabledPostureIsOne(t *testing.T) {
	hostsFn, firedFn := probeCensusFakes()
	confirmed := func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }
	collect := startObservationProbeJob(context.Background(), hostsFn, firedFn, confirmed, time.Hour, 0, true, probeNow, edcQuiet, nil)
	if got := gaugeVal(collect(), "tg_observation_probe_enabled"); got != 1 {
		t.Fatalf("enabled gauge = %v, want 1 when armed", got)
	}
}

// A confirmed host that IS currently unobservable moves the numerator; the denominator is unchanged, so the
// published coverage is real (and shares the numerator's freshness — both read in the same collector call).
func TestObservationProbeJob_ConfirmedMovesNumerator(t *testing.T) {
	hostsFn, firedFn := probeCensusFakes()
	confirmed := func(context.Context) (map[string]bool, error) { return map[string]bool{"gap1": true}, nil }
	collect := startObservationProbeJob(context.Background(), hostsFn, firedFn, confirmed, time.Hour, 0, false, probeNow, edcQuiet, nil)
	s := collect()
	if got := gaugeVal(s, "tg_observation_probe_confirmed_total"); got != 1 {
		t.Fatalf("numerator = %v, want 1 (gap1 confirmed)", got)
	}
	if got := gaugeVal(s, "tg_observation_probe_coverage_ratio"); got != 0.5 {
		t.Fatalf("coverage ratio = %v, want 0.5 (1 of 2)", got)
	}
}

// A numerator read error OMITS the coverage gauges (they must share the denominator's freshness) but still
// publishes the posture — honest absence, never a denominator against a stale zero.
func TestObservationProbeJob_NumeratorError_OmitsCoverageKeepsPosture(t *testing.T) {
	hostsFn, firedFn := probeCensusFakes()
	confirmed := func(context.Context) (map[string]bool, error) { return nil, errors.New("db down") }
	collect := startObservationProbeJob(context.Background(), hostsFn, firedFn, confirmed, time.Hour, 0, false, probeNow, edcQuiet, nil)
	s := collect()
	if got := gaugeVal(s, "tg_observation_probe_enabled"); got != 0 {
		t.Fatalf("posture gauge = %v, want 0", got)
	}
	if got := gaugeVal(s, "tg_observation_probe_unobservable_total"); got != -1 {
		t.Fatalf("denominator gauge present (%v) despite a failed numerator read — the two must share freshness", got)
	}
}

// Nil inputs emit the posture alone (no census/DB configured) — never a phantom coverage.
func TestObservationProbeJob_NilInputs_PostureOnly(t *testing.T) {
	collect := startObservationProbeJob(context.Background(), nil, nil, nil, time.Hour, 0, false, probeNow, edcQuiet, nil)
	s := collect()
	if len(s) != 1 || s[0].Name != "tg_observation_probe_enabled" {
		t.Fatalf("nil-input samples = %v, want only the posture gauge", s)
	}
}
