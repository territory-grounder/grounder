package main

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/observe"
	"github.com/territory-grounder/grounder/tools/observeprobe"
)

// startObservationProbeJob publishes the "coverage of the unmeasured" scorecard dimension (TG-180 part 2) and
// declares the fault-injection probe's arming posture — and it is the always-on, DEFAULT-OFF half of that part.
//
// WHAT SHIPS ON EVERY DEPLOY (no arming needed): the coverage dimension. Of the census-UNOBSERVABLE entities
// (observe.Census, the never-fired-ever proxy), how many has the probe CONFIRMED a verdict on (confirmedFn —
// observation_probe rows with a terminal verdict) versus how many remain untested. The denominator (the current
// unobservable set) and the numerator (the confirmed set) are BOTH read here, at scrape time, so they share one
// freshness — a coverage ratio can never be taken against a staler population than its numerator counts. A host
// that leaves the unobservable set leaves both at once. When probing is disarmed the numerator is 0 and the
// honest reading is "0 of N unobservable entities probe-confirmed", which is exactly the point: the census's
// coverage claim is published as UNTESTED until the probe is armed to falsify it.
//
// WHAT IS DEFAULT-OFF (owner-gated arming, deliberately NOT wired in this build): the perturbing loop that
// actually injects a fault. `enabled` reflects TG_OBSERVE_PROBE_ENABLED and is published as a gauge so the
// posture is visible and alertable, but this function starts NO injection loop and constructs NO injector — the
// live path is the observeprobe.Orchestrator driven by an observeprobe.Injector that wraps tools/faultinjector,
// and standing it up requires the estate-specific, owner-supplied config the epic's lowest safety sub-score
// gates. ARMING STEPS, for the owner (surfaced, not taken):
//  1. Provide the guinea-pig pool (NL-only, Tier-B, oas-excluded) + TG_PROXMOX_ALLOWED_GUESTS + snapshot node
//     + ssh identity — the same inputs tools/faultinjector/cmd/faultinjector loads.
//  2. Construct an observeprobe.Injector backed by faultinjector (name-assert → record injected_fault → effect
//     → self-reverting restore), the observeprobe.ProbeStore/AlertReader over core/db, and the census/snapshot/
//     breaker/kill seams; set observeprobe.Orchestrator.Enabled = TG_OBSERVE_PROBE_ENABLED.
//  3. Tick Orchestrator.RunCycle on an owner-visible, rate-limited cadence with a kill switch.
//
// Until all three exist the probe injects nothing, and this gauge reads 0 so nobody mistakes "coverage
// published" for "coverage tested".
//
// UNCONFIGURED inputs (nil fns) emit the posture gauge alone if possible, else nothing — the same honest-absence
// discipline as the census: a deployment with no DB reads "not measured", never a phantom full coverage. The
// fired-history is a DB read, so it is CACHED and refreshed on a ticker exactly as the census does; the live
// host set and the confirmed set are read fresh each scrape.
func startObservationProbeJob(
	ctx context.Context,
	hostsFn func() []string,
	lastFiredFn func(context.Context) (map[string]time.Time, error),
	confirmedFn func(context.Context) (map[string]bool, error),
	window, refresh time.Duration,
	enabled bool,
	now func() time.Time,
	logf func(string, ...any),
	// snapshotFn persists one census snapshot per refresh (TG-180, migration 0106) so the grounder's axis
	// scorer can publish coverage-of-the-unmeasured from durable rows; nil = gauges only (tests, no DSN).
	snapshotFn func(context.Context, db.ObservationCoverage) error,
) func() []metrics.Sample {
	enabledVal := 0.0
	if enabled {
		enabledVal = 1
	}
	postureOnly := func() []metrics.Sample {
		return []metrics.Sample{probeEnabledSample(enabledVal)}
	}

	if enabled {
		logf("observation probe: TG_OBSERVE_PROBE_ENABLED is SET, but the injection loop is NOT wired in this build — " +
			"arming is a separate owner-gated step (guinea-pig pool + snapshot node + ssh identity + the faultinjector-backed " +
			"injector). Publishing the coverage dimension; injecting NOTHING.")
	} else {
		logf("observation probe: DEFAULT-OFF (TG_OBSERVE_PROBE_ENABLED unset) — publishing coverage-of-the-unmeasured only; " +
			"the fault-injection probe is disarmed and injects nothing until an owner arms it.")
	}

	if hostsFn == nil || lastFiredFn == nil || confirmedFn == nil {
		return postureOnly
	}

	var cache atomic.Pointer[map[string]time.Time]
	refreshOnce := func() {
		lf, err := lastFiredFn(ctx)
		if err != nil {
			logf("observation probe coverage: fired-history refresh failed (keeping last good cache): %v", err)
			return
		}
		cache.Store(&lf)
		if snapshotFn == nil {
			return
		}
		// TG-180: persist the census at this refresh so the scorecard dimension reads durable rows. Same
		// inputs as the scrape-time gauges, so the two surfaces agree; a failed write is logged, never fatal.
		res := observe.Census(hostsFn(), lf, now().Add(-window))
		confirmed, cerr := confirmedFn(ctx)
		if cerr != nil {
			logf("observation coverage snapshot: confirmed-hosts read failed (%v) — snapshot skipped this refresh", cerr)
			return
		}
		cov := observeprobe.Coverage(res.HostsInState(observe.Unobservable), confirmed)
		if serr := snapshotFn(ctx, db.ObservationCoverage{
			Total: res.Total(), Observed: res.Counts[observe.Observed], HealthyQuiet: res.Counts[observe.HealthyQuiet],
			Unobservable: cov.Unobservable, Confirmed: cov.Confirmed, ProbeArmed: enabled,
		}); serr != nil {
			logf("observation coverage snapshot: write failed (%v) — the scorecard keeps its last snapshot", serr)
		}
	}
	refreshOnce() // boot prime
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
		out := []metrics.Sample{probeEnabledSample(enabledVal)}
		lf := cache.Load()
		if lf == nil {
			return out // no fired-history yet — honest absence for the coverage gauges
		}
		res := observe.Census(hostsFn(), *lf, now().Add(-window))
		unobs := res.HostsInState(observe.Unobservable)
		confirmed, err := confirmedFn(ctx)
		if err != nil {
			// The numerator read failed. Emit the posture but OMIT the coverage gauges rather than publish a
			// denominator against a stale/zero numerator — the two must share one freshness.
			logf("observation probe coverage: confirmed-hosts read failed (%v) — omitting the coverage gauges this scrape", err)
			return out
		}
		cov := observeprobe.Coverage(unobs, confirmed)
		return append(out,
			metrics.Sample{Name: "tg_observation_probe_unobservable_total", Kind: metrics.Gauge, Value: float64(cov.Unobservable),
				Help: "census-UNOBSERVABLE entities right now — the coverage-of-the-unmeasured DENOMINATOR (TG-180)"},
			metrics.Sample{Name: "tg_observation_probe_confirmed_total", Kind: metrics.Gauge, Value: float64(cov.Confirmed),
				Help: "unobservable entities the probe has confirmed a verdict on — the coverage NUMERATOR (0 until armed) (TG-180)"},
			metrics.Sample{Name: "tg_observation_probe_unprobed_total", Kind: metrics.Gauge, Value: float64(cov.Unprobed),
				Help: "unobservable entities not yet probe-confirmed — the untested remainder (TG-180)"},
			metrics.Sample{Name: "tg_observation_probe_coverage_ratio", Kind: metrics.Gauge, Value: cov.Ratio(),
				Help: "fraction of census-unobservable entities the probe has confirmed; 0 with an empty denominator, never a phantom 1.0 (TG-180)"},
		)
	}
}

func probeEnabledSample(v float64) metrics.Sample {
	return metrics.Sample{Name: "tg_observation_probe_enabled", Kind: metrics.Gauge, Value: v,
		Help: "1 when the TG-180 fault-injection probe is ARMED (TG_OBSERVE_PROBE_ENABLED); 0 = default-OFF, coverage published but nothing injected"}
}
