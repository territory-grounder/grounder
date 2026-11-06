package main

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/breaker"
)

// TG-452 — tg_breaker_state is the tg_-prefixed FORWARD NAME for the 3-state breaker gauge, dual-emitted beside
// the legacy circuit_breaker_state so dashboards/alerts can migrate without a breaking rename. It must appear for
// every NAMED store breaker (the named-breaker emit) carrying the SAME value + name-label as the legacy series;
// the mutation breaker's emit is covered by TestMetricsExposition's /metrics rendering. The legacy series is kept
// deliberately — alert rules still key on it (deploy/breaker_alert_covers_every_name_test.go).
func TestTgBreakerStateMirrorsNamedBreakerGauge(t *testing.T) {
	adm := (&workerAdmin{}).withBreakerStore(storeWith(t,
		breaker.Record{Name: "model-fast", State: breaker.StateOpen, OpenedAt: time.Now()},
		breaker.Record{Name: "judge", State: breaker.StateClosed},
	))
	ss := adm.samples()
	for _, tc := range []struct {
		name string
		want float64
	}{
		{"model-fast", 2}, // open
		{"judge", 0},      // closed
	} {
		legacy, okL := brkSample(ss, "circuit_breaker_state", tc.name)
		fwd, okF := brkSample(ss, "tg_breaker_state", tc.name)
		if !okL || !okF {
			t.Errorf("breaker %q: circuit_breaker_state present=%v, tg_breaker_state present=%v — the forward name "+
				"must be emitted beside the legacy one", tc.name, okL, okF)
			continue
		}
		if fwd.Value != legacy.Value || fwd.Value != tc.want {
			t.Errorf("breaker %q: tg_breaker_state=%v, legacy=%v, want both %v", tc.name, fwd.Value, legacy.Value, tc.want)
		}
	}
}
