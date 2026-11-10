// Command faultinjector drives the estate fault-injection campaign that supplies BOTH benchmark tracks:
// Track A's heal outcomes (axes A3/A5) and Track B's aligned head-to-head pairs (R1).
//
// It replaces an untracked bash script that lived in a /tmp scratchpad, died with its parent shell four
// times, and on 2026-07-26 stranded two LIVE guests at 97% root disk ~80 minutes past their restore deadline.
// The difference is not tidiness: restore obligations are now DURABLE (migration 0041), so a crashed or
// killed injector cannot lose track of what it broke, and a fresh process reconciles its predecessor's debts
// before adding any new load.
//
// SAFETY POSTURE (all fail-closed):
//   - refuses to start when the injection pool disagrees with TG_PROXMOX_ALLOWED_GUESTS
//   - refuses to fault on a short/absent cluster snapshot (never faults blind)
//   - name-asserts every guest immediately before acting on it
//   - records the restore obligation BEFORE performing the effect
//   - never stacks a second fault on a guest that already owes a restore
//   - holds off while TG's own mutation breaker is open (unreadable breaker ⇒ treated as OPEN)
//   - stops on either kill switch (a file OR a DB row; an unreadable switch ⇒ treated as ENGAGED)
//   - reconciles outstanding obligations on boot, on every tick, and on the way out
//
// Usage:
//
//	TG_RUNTIME_DSN=postgres://…  faultinjector \
//	    -pool-file pool.txt -allowed-guests "$TG_PROXMOX_ALLOWED_GUESTS" \
//	    -key ~/.ssh/one_key -snap-node dc1pve01 \
//	    -cadence 90s -restore-after 30m -max-down 4 -max-busy 7 -target 150 \
//	    -kill-file /var/run/faultinjector.STOP
//
// The pool file is one guest per line: "<vmid> <name> <node> [container]". The optional 4th field is the
// docker container a container-down fault may stop on that guest (operator-declared; absent = that guest is
// not eligible for container-down). Blank lines and #comments are ignored.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/tools/faultinjector"
)

func main() {
	var (
		dsn          = flag.String("dsn", os.Getenv("TG_RUNTIME_DSN"), "grounder DSN (defaults to $TG_RUNTIME_DSN)")
		poolFile     = flag.String("pool-file", "", "pool file: one '<vmid> <name> <node>' per line")
		allowedCSV   = flag.String("allowed-guests", os.Getenv("TG_PROXMOX_ALLOWED_GUESTS"), "comma-separated guests TG may actuate")
		key          = flag.String("key", os.Getenv("HOME")+"/.ssh/one_key", "ssh identity")
		snapNode     = flag.String("snap-node", "", "Proxmox node to read the cluster snapshot from (required)")
		cadence      = flag.Duration("cadence", 90*time.Second, "delay between ticks")
		restoreAfter = flag.Duration("restore-after", 30*time.Minute, "how long a fault is held before its restore falls due")
		settleWindow = flag.Duration("settle-window", 10*time.Minute, "how long a guest must be observed RECOVERED (from the later of its restore and its restore deadline) before ANY fault class may hit it again; must exceed the monitoring poll interval, because detection is a state TRANSITION, and must be at least 10m, the episode attribution's ownership grace, so two faults never share a guest's evidence (0 disables the guard and is refused unless -allow-overlap)")
		allowOverlap = flag.Bool("allow-overlap", false, "accept a -settle-window below the 10m attribution grace (or 0): every overlapping fault pair then reads ambiguous and is never credited")
		maxDown      = flag.Int("max-down", 4, "max guests concurrently stopped")
		maxBusy      = flag.Int("max-busy", 7, "max guests under any fault at once")
		target       = flag.Int("target", 0, "stop after N injections (0 = unbounded)")
		killFile     = flag.String("kill-file", "", "path whose existence stops the engine")
		note         = flag.String("note", "faultinjector", "note recorded on every ledger row")
		classesCSV   = flag.String("classes", "device-down,disk-fill,device-down", "class rotation")
		dryRun       = flag.Bool("dry-run", false, "assert the pool and report the next decision, then exit without touching the estate")
		pauseFile    = flag.String("pause-file", "", "path whose existence pauses new injections while restores continue; removing it resumes (TG-710)")
		heartbeat    = flag.String("pause-heartbeat", "", "file the pause watcher touches every cycle; new injections pause unless it is fresher than -pause-heartbeat-max-age (TG-710)")
		heartbeatAge = flag.Duration("pause-heartbeat-max-age", 90*time.Second, "how old the watcher heartbeat may be before new injections pause")
		memGuardFile = flag.String("memory-guard", "", "memory guard file (TG-710): guarded nodes with their floor, telemetry max-age and pressure limit, and each guarded target's declared restore cost per class; empty guards no node")
	)
	flag.Parse()

	logf := func(format string, args ...any) { log.Printf(format, args...) }

	if *settleWindow < minSettleWindow && !*allowOverlap {
		fatal("-settle-window %s is below the %s ownership grace of the episode attribution (TG-679): two faults could share a guest's evidence and both would read ambiguous, never credited; pass -allow-overlap to accept that", *settleWindow, minSettleWindow)
	}
	if *dsn == "" {
		fatal("no DSN — set $TG_RUNTIME_DSN or pass -dsn")
	}
	if *snapNode == "" {
		fatal("-snap-node is required: without a cluster snapshot the engine would fault blind")
	}
	pool, err := faultinjector.LoadPool(*poolFile)
	if err != nil {
		fatal("pool: %v", err)
	}
	allow := faultinjector.ParseAllowlist(*allowedCSV)
	if len(allow) == 0 {
		fatal("empty allowlist — refusing to run: every fault would be an automatic A1/A3 miss")
	}
	rotation, err := faultinjector.ParseClasses(*classesCSV)
	if err != nil {
		fatal("classes: %v", err)
	}
	if *heartbeat != "" && (*heartbeatAge <= 0 || *heartbeatAge > 10*time.Minute) {
		fatal("-pause-heartbeat-max-age %s must be in (0, 10m]", *heartbeatAge)
	}
	var guard *faultinjector.MemGuard
	if *memGuardFile != "" {
		guard, err = faultinjector.LoadMemGuard(*memGuardFile)
		if err != nil {
			fatal("%v", err)
		}
		if err := guard.Validate(pool, rotation); err != nil {
			fatal("%v", err)
		}
	}

	// Assert the config BEFORE opening any connection or touching the estate. A pool/allowlist mismatch is a
	// configuration error, so it must be reported as one — not surfaced later as a database problem, and never
	// discovered halfway through a campaign that has already manufactured misses against the system under test.
	if notAllowlisted, notDrilled := faultinjector.PoolMismatch(pool, allow); len(notAllowlisted) > 0 {
		for _, n := range notDrilled {
			logf("pool: %s is actuatable by TG but never drilled — its classes accrue no evidence", n)
		}
		fatal("pool/allowlist mismatch: %v are in the injection pool but NOT in TG_PROXMOX_ALLOWED_GUESTS — "+
			"TG structurally cannot heal them, so every fault there is an automatic A1/A3 miss that looks like a "+
			"TG failure; reconcile the two before running", notAllowlisted)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pgpool, err := db.Connect(ctx, *dsn)
	if err != nil {
		fatal("connect: %v", err)
	}
	defer pgpool.Close()

	store := faultinjector.NewStore(pgpool.Pool)
	eng := &faultinjector.Engine{
		Store:     store, // db.Pool embeds *pgxpool.Pool
		Exec:      faultinjector.SSHRunner{KeyPath: *key, Timeout: 25 * time.Second},
		Pool:      pool,
		Allowlist: allow,
		Limits: faultinjector.Limits{
			MaxDown: *maxDown, MaxBusy: *maxBusy, Target: *target, RestoreAfter: *restoreAfter,
			SettleWindow: *settleWindow,
		},
		Rotation:       rotation,
		Cadence:        *cadence,
		KillFile:       *killFile,
		PauseFile:      *pauseFile,
		PauseHeartbeat: *heartbeat, PauseHeartbeatMaxAge: *heartbeatAge,
		Note:     *note,
		Log:      logf,
		SnapNode: *snapNode,
	}
	if guard != nil {
		eng.Guard, eng.Skips = guard, store
		logf("memory guard: ON (%s) — a fault on a guarded node needs fresh telemetry showing the floor plus this restore plus every outstanding restore on that node; refusals go to injection_skip; restores are never gated", *memGuardFile)
		for _, l := range guard.Describe() {
			logf("memory guard: %s", l)
		}
	} else {
		logf("memory guard: OFF — no -memory-guard file; no node is guarded")
	}
	switch {
	case *pauseFile != "" && *heartbeat != "":
		logf("injection pause: ON — file %s; watcher heartbeat %s must be fresher than %s, or new injections pause; restores never pause", *pauseFile, *heartbeat, *heartbeatAge)
	case *pauseFile != "":
		logf("injection pause: file %s only — no watcher heartbeat is required", *pauseFile)
	default:
		logf("injection pause: OFF")
	}

	if *dryRun {
		if err := eng.AssertPool(); err != nil {
			fatal("%v", err)
		}
		logf("pool assertion PASSED — %d guests, all actuatable by TG", len(pool))
		// REPORT ONLY. An earlier version called ReconcileOnce here, which actually performs repairs — a flag
		// that promises not to touch the estate must not touch it, or nobody can trust it to inspect a
		// campaign mid-flight.
		out, err := eng.Store.Outstanding(ctx)
		if err != nil {
			fatal("dry-run: cannot read outstanding obligations: %v", err)
		}
		if guard != nil {
			reportGuard(ctx, eng, out, rotation, logf)
		}
		due := faultinjector.Reconcile(time.Now().UTC(), out)
		logf("dry-run: %d obligation(s) outstanding, %d of them due or overdue; nothing was changed", len(out), len(due))
		for _, a := range due {
			logf("  would repair id=%d %s/%s (%s overdue)", a.Fault.ID, a.Fault.Host, a.Fault.Class, a.Overdue.Round(time.Second))
		}
		return
	}

	// SIGTERM/SIGINT drain: the deferred reconcile is what makes `kill` (and a supervisor restart) safe. A
	// hard SIGKILL skips this, which is exactly why the ledger — not this handler — is the real guarantee.
	defer func() {
		drain, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if n := eng.ReconcileOnce(drain); n > 0 {
			logf("shutdown drain: discharged %d outstanding obligation(s)", n)
		}
	}()

	if err := eng.Run(ctx); err != nil && ctx.Err() == nil {
		fatal("%v", err)
	}
}

// minSettleWindow mirrors core/episodes.OwnershipGrace (this binary does not import the episodes package): the
// attribution's raw window ends at the restore deadline plus this grace, so a settle at least this long — clocked
// from the later of the restore and the deadline — keeps consecutive faults on one guest from sharing evidence.
const minSettleWindow = 10 * time.Minute

// reportGuard reads each guarded node once and says, for every guarded pool guest and rotation class, whether the
// guard would admit a fault now. It records nothing.
func reportGuard(ctx context.Context, eng *faultinjector.Engine, out []faultinjector.Outstanding, rotation []faultinjector.Class, logf func(string, ...any)) {
	now := func() time.Time { return time.Now().UTC() }
	readings := map[string]faultinjector.NodeTelemetry{}
	for _, n := range eng.Guard.Nodes() {
		t := faultinjector.ReadNodeTelemetry(ctx, eng.Exec, n, now)
		readings[n] = t
		if t.Err != nil {
			logf("dry-run: memory guard: %s telemetry unavailable: %v", n, t.Err)
			continue
		}
		logf("dry-run: memory guard: %s available %d bytes (%.2f GiB), memory pressure some-avg60 %.2f", n, t.AvailableBytes, float64(t.AvailableBytes)/(1<<30), t.PSISomeAvg60)
	}
	seen := map[faultinjector.Class]bool{}
	for _, c := range rotation {
		if seen[c] {
			continue
		}
		seen[c] = true
		for _, g := range eng.Pool {
			if !eng.Guard.Guards(g.Node) || !faultinjector.DeclaredFor(g, c) {
				continue
			}
			ok, skip := eng.Guard.Evaluate(now(), g, c, out, readings[g.Node])
			if ok {
				logf("dry-run: memory guard: would ADMIT %s on %s: needs %d bytes, has %d", c, g.Name, skip.RequiredBytes, skip.AvailableBytes)
			} else {
				logf("dry-run: memory guard: would SKIP %s on %s: needs %d bytes, has %d: %s (%s)", c, g.Name, skip.RequiredBytes, skip.AvailableBytes, skip.Detail, skip.Reason)
			}
		}
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "faultinjector: "+format+"\n", args...)
	os.Exit(1)
}

// The pool file, allowlist, and class-rotation parsers now live in the faultinjector package (LoadPool,
// ParseAllowlist, ParseClasses) so the worker's observation-probe loop reuses the SAME parsing (TG-180).
