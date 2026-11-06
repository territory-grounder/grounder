package main

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/observe"
)

// startObservationCensusJob publishes TG's estate-observation census (TG-180): it joins the live estate host
// set (hostsFn — FreshHostNames, over the SAME freshness the gate enforces, TG-449) against fired-alert history
// (lastFiredFn) and classifies each host observed / healthy_quiet / unobservable. It answers what fraction of
// the estate TG can actually SEE — silence today is indistinguishable from health.
//
// The host set is read LIVE on every scrape (cheap, in-memory), so it tracks the graph immediately; the
// fired-history is a DB read, so it is CACHED and refreshed on a ticker (refresh; <=0 = boot-load only) — a
// host that begins alerting is reflected within one refresh interval, not only on the next deploy (slice 1c,
// closing the boot-load staleness the slice-1 review flagged). A refresh that ERRORS keeps the last good cache,
// so a DB blip never blanks a working census. UNAVAILABLE inputs (nil fns, or the boot read failing with no
// cache yet) emit NOTHING — honest absence, never a phantom all-unobservable census. "unobservable" is a LOWER
// BOUND (never-fired-ever) because TG stores fired alerts, not rule definitions; the fault-injection PROBE
// (part 2) is the safety-gated slice that would make it falsifiable. now is injected for deterministic tests.
func startObservationCensusJob(
	ctx context.Context,
	hostsFn func() []string,
	lastFiredFn func(context.Context) (map[string]time.Time, error),
	window, refresh time.Duration,
	now func() time.Time,
	logf func(string, ...any),
) func() []metrics.Sample {
	if hostsFn == nil || lastFiredFn == nil {
		return func() []metrics.Sample { return nil }
	}
	var cache atomic.Pointer[map[string]time.Time]
	refreshOnce := func() {
		lf, err := lastFiredFn(ctx)
		if err != nil {
			logf("observation census: fired-history refresh failed (keeping last good cache): %v", err)
			return
		}
		cache.Store(&lf)
	}
	refreshOnce() // boot prime
	if lf := cache.Load(); lf != nil {
		res := observe.Census(hostsFn(), *lf, now().Add(-window))
		logf("observation census: %d live hosts — observed=%d healthy_quiet=%d unobservable=%d (window %s, refresh %s; unobservable=never-fired, a lower bound pending the fault-injection probe)",
			res.Total(), res.Counts[observe.Observed], res.Counts[observe.HealthyQuiet], res.Counts[observe.Unobservable], window, refresh)
	}
	if refresh > 0 {
		go func() {
			t := time.NewTicker(refresh)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					refreshOnce()
				}
			}
		}()
	}
	return func() []metrics.Sample {
		lf := cache.Load()
		if lf == nil {
			return nil // boot read failed and no refresh has landed yet — honest absence
		}
		res := observe.Census(hostsFn(), *lf, now().Add(-window))
		out := []metrics.Sample{{
			Name: "tg_observation_census_hosts_total", Kind: metrics.Gauge, Value: float64(res.Total()),
			Help: "distinct live estate hosts censused for observation coverage (TG-180)",
		}}
		for _, st := range observe.CensusStates {
			out = append(out, metrics.Sample{
				Name: "tg_observation_census", Kind: metrics.Gauge, Value: float64(res.Counts[st]),
				Help:   "live estate hosts by observation state — observed (fired in window), healthy_quiet (fired before, quiet now), unobservable (never fired) (TG-180)",
				Labels: map[string]string{"state": string(st)},
			})
		}
		return out
	}
}
