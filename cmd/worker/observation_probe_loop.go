package main

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/tools/faultinjector"
	"github.com/territory-grounder/grounder/tools/observeprobe"
)

// engineInjector adapts a *faultinjector.Engine to observeprobe.Injector: it drives ONE probe injection through
// the engine's extracted InjectGuest (name-assert → record obligation → effect → arm restore). The orchestrator
// does its own gating (kill/breaker/snapshot/outstanding/already-probed) through its seams, so this shim carries
// none of it — it is purely the effect.
type engineInjector struct{ e *faultinjector.Engine }

func (a engineInjector) Inject(ctx context.Context, g faultinjector.PoolGuest, c faultinjector.Class, window time.Duration) (bool, error) {
	return a.e.InjectGuest(ctx, g, c) // window = the observation window; the restore timer is e.Limits.RestoreAfter, configured >= window
}

// startObservationProbeLoop stands up the TG-180 fault-injection probe loop — the perturbing arm that turns the
// census's coverage claim from an assertion into a tested one. It is DARK by default: with none of the
// TG_OBSERVE_PROBE_* estate config supplied it returns before starting any goroutine, so an unconfigured worker
// is byte-identical to today (the coverage gauge in startObservationProbeJob, nothing injected).
//
// When configured but TG_OBSERVE_PROBE_ENABLED is unset the loop still runs, reconciling verdicts for probes
// already in flight and logging what it WOULD probe, but the orchestrator injects nothing (Enabled=false is its
// single arming gate). Arming is the owner decision the epic's lowest safety sub-score gates.
func startObservationProbeLoop(ctx context.Context, dbPool *db.Pool, unobservableFn func() []string, now func() time.Time, logf func(string, ...any)) {
	enabled := truthyEnv("TG_OBSERVE_PROBE_ENABLED")
	allow := faultinjector.ParseAllowlist(getenv("TG_PROXMOX_ALLOWED_GUESTS", ""))
	snapNode := strings.TrimSpace(getenv("TG_OBSERVE_PROBE_SNAP_NODE", ""))

	var pool []faultinjector.PoolGuest
	if poolFile := strings.TrimSpace(getenv("TG_OBSERVE_PROBE_POOL_FILE", "")); poolFile != "" {
		p, err := faultinjector.LoadPool(poolFile)
		if err != nil {
			logf("observation probe loop: pool file %q failed to load (%v) — not starting the loop", poolFile, err)
			return
		}
		pool = p
	}

	// GUARD — the byte-identical default. The pool file and snapshot node are NEW env keys, unset by default, so
	// with no new config this returns having built no store and started no goroutine: coverage gauge only.
	if len(pool) == 0 || len(allow) == 0 || snapNode == "" {
		logf("observation probe loop: not configured (pool/allowlist/snap-node) — coverage gauge only, no loop")
		return
	}

	// HOME goes through the resolver (getenv), not os.Getenv directly — cmd/worker forbids reads that bypass the
	// console-config layer (boot_config_test's resolver-discipline guard); for HOME the resolver falls through to
	// the environment anyway, so the value is identical.
	sshKey := getenv("TG_OBSERVE_PROBE_SSH_KEY", filepath.Join(getenv("HOME", ""), ".ssh/one_key"))
	sshTimeout := envDuration("TG_OBSERVE_PROBE_SSH_TIMEOUT", 30*time.Second)
	cadence := envDuration("TG_OBSERVE_PROBE_CADENCE", 30*time.Minute)
	window := envDuration("TG_OBSERVE_PROBE_WINDOW", 10*time.Minute)
	restoreAfter := envDuration("TG_OBSERVE_PROBE_RESTORE_AFTER", 15*time.Minute)

	// A non-positive cadence would panic time.NewTicker; a non-positive window collapses the observation to
	// nothing (a probe's verdict would be decided the instant it records). Both are config slips, not reasons to
	// leave the loop dark — floor them to the defaults, the same bump-don't-refuse posture as restore-after below.
	if cadence <= 0 {
		logf("observation probe loop: cadence %s is non-positive — flooring to 30m", cadence)
		cadence = 30 * time.Minute
	}
	if window <= 0 {
		logf("observation probe loop: window %s is non-positive — flooring to 10m", window)
		window = 10 * time.Minute
	}

	// Classes default to every restore-owing class (device/disk/container/service/log-fill) — the self-reverting
	// faults the ledger reconciler can undo. An explicit list is validated by the same parser the injector uses.
	var classes []faultinjector.Class
	if csv := strings.TrimSpace(getenv("TG_OBSERVE_PROBE_CLASSES", "")); csv != "" {
		c, err := faultinjector.ParseClasses(csv)
		if err != nil {
			logf("observation probe loop: invalid TG_OBSERVE_PROBE_CLASSES (%v) — not starting the loop", err)
			return
		}
		classes = c
	} else {
		for _, c := range faultinjector.AllClasses() {
			if c.OwesRestore() {
				classes = append(classes, c)
			}
		}
	}

	// The fault must OUTLIVE the observation window. If it reverts first, a genuinely-blind host could recover
	// before the window closes and read as observed — false blindness in the safe direction is still a wrong
	// verdict. Bump rather than refuse: a mis-ordered pair is a config slip, not a reason to leave the loop dark.
	if restoreAfter < window {
		bumped := window + 5*time.Minute
		logf("observation probe loop: restore-after %s < window %s — the fault would revert before the observation "+
			"window closes (false blindness); bumping restore-after to %s", restoreAfter, window, bumped)
		restoreAfter = bumped
	}

	fiStore := faultinjector.NewStore(dbPool.Pool) // db.Pool embeds *pgxpool.Pool
	eng := &faultinjector.Engine{
		Store:     fiStore,
		Exec:      faultinjector.SSHRunner{KeyPath: sshKey, Timeout: sshTimeout},
		Pool:      pool,
		Allowlist: allow,
		Limits:    faultinjector.Limits{RestoreAfter: restoreAfter},
		SnapNode:  snapNode,
		Note:      "TG-180 observe-probe",
		Log:       logf,
	}

	probeStore := probeStoreAdapter{db.NewObservationProbeStore(dbPool)} // one adapter serves both the ProbeStore and AlertReader seams
	o := &observeprobe.Orchestrator{
		Enabled:      enabled,
		Injector:     engineInjector{eng},
		Store:        probeStore,
		Alerts:       probeStore,
		Unobservable: unobservableFn,
		Snapshot:     eng.Snapshot,
		Outstanding:  fiStore.Outstanding,
		BreakerOpen:  fiStore.BreakerOpen,
		KillSwitch:   fiStore.KillSwitchEngaged,
		Pool:         pool,
		Allowlist:    allow,
		Classes:      classes,
		Window:       window,
		Now:          now,
		Log:          logf,
	}

	if enabled {
		logf("observation probe loop ARMED: pool=%d classes=%v cadence=%s window=%s restore-after=%s — injecting on guinea-pigs",
			len(pool), classes, cadence, window, restoreAfter)
	} else {
		logf("observation probe loop CONFIGURED BUT DISARMED (TG_OBSERVE_PROBE_ENABLED unset) — reconciling verdicts + logging would-probes, injecting nothing")
	}

	go func() {
		o.RunCycle(ctx) // boot reconcile: decide any verdict whose window closed while we were down
		t := time.NewTicker(cadence)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				o.RunCycle(ctx)
			}
		}
	}()
}
