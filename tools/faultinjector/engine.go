package faultinjector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Logf is the engine's output sink.
type Logf func(format string, args ...any)

// Engine composes the store, the runner and the pure planner into a supervised loop.
// LedgerStore is the slice of the durable ledger the engine uses. It is an INTERFACE so the reconcile loop is
// testable without a database — and the reconcile loop is where the safety property lives, since it is the
// only code that may close an obligation. `*Store` is the production implementation.
//
// Deliberately narrow. A broad fake that pretends to persist rows would let a dropped column pass unnoticed
// (this repo has been bitten by exactly that); these five methods carry no row marshalling, so a fake can only
// record which decision was taken, which is what the oracles assert.
type LedgerStore interface {
	Outstanding(ctx context.Context) ([]Outstanding, error)
	RecentRestores(ctx context.Context, within time.Duration) (map[string]time.Time, error)
	RecordInjection(ctx context.Context, host string, class Class, node, faultRef string, restoreAfter time.Duration, note string) (int64, error)
	MarkRestored(ctx context.Context, id int64) error
	MarkRestoreFailed(ctx context.Context, id int64, reason string) error
	BreakerOpen(ctx context.Context) (bool, error)
	KillSwitchEngaged(ctx context.Context) bool
}

type Engine struct {
	Store     LedgerStore
	Exec      Runner
	Pool      []PoolGuest
	Allowlist map[string]bool
	Limits    Limits
	Rotation  []Class
	Cadence   time.Duration
	KillFile  string // file-side kill switch; the DB-side one is checked independently
	Note      string
	Log       Logf
	SnapNode  string // the Proxmox node to read the cluster snapshot from
}

// snapshotAttempts bounds how many times snapshotRead re-reads the cluster before giving up;
// snapshotRetryBackoff is the base inter-attempt delay (grows linearly with the attempt). The PVE API
// returns a transient non-zero often enough that ~40% of single-shot ticks were skipped during
// campaign #3 (2026-08-26 soak.log), halving the inject rate for no safety benefit — a bounded retry
// recovers the transient case. They are package vars, not consts, ONLY so a test can drive the retry
// without real sleeps. Fail-closed is unchanged: after the final attempt the error is returned and the
// caller skips the tick ("refusing to fault blind"), exactly as before.
var (
	snapshotAttempts     = 3
	snapshotRetryBackoff = 2 * time.Second
)

// Snapshot reads vmid -> status for the whole cluster from one node.
func (e *Engine) Snapshot(ctx context.Context) (map[string]string, error) {
	out, err := e.snapshotRead(ctx)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		VMID   json.Number `json:"vmid"`
		Status string      `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("snapshot: parse: %w", err)
	}
	st := make(map[string]string, len(rows))
	for _, r := range rows {
		st[r.VMID.String()] = r.Status
	}
	return st, nil
}

// snapshotRead runs the pvesh cluster-resources read, retrying a TRANSIENT transport failure (an SSH or
// pvesh error, or a non-zero exit) up to snapshotAttempts times with a linear backoff. A parse failure
// is deliberately NOT retried here — malformed JSON is a deterministic condition the caller should see,
// not one a retry should mask. When every attempt fails the last transport error is returned, and the
// tick skips (fail-closed), exactly as the single-shot read did before.
func (e *Engine) snapshotRead(ctx context.Context) (string, error) {
	argv := []string{"pvesh", "get", "/cluster/resources", "--type", "vm", "--output-format", "json"}
	var lastErr error
	for attempt := 1; attempt <= snapshotAttempts; attempt++ {
		out, code, err := e.Exec.Run(ctx, e.SnapNode, argv)
		switch {
		case err != nil:
			lastErr = fmt.Errorf("snapshot: %w", err)
		case code != 0:
			lastErr = fmt.Errorf("snapshot: pvesh exited %d", code)
		default:
			return out, nil
		}
		if attempt < snapshotAttempts {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(snapshotRetryBackoff * time.Duration(attempt)):
			}
		}
	}
	return "", lastErr
}

// killed reports the file-side kill switch.
func (e *Engine) killed() bool {
	if e.KillFile == "" {
		return false
	}
	_, err := os.Stat(e.KillFile)
	return err == nil
}

// AssertPool refuses to start when the injection pool and TG's actuation allowlist disagree.
//
// This is deliberately FATAL rather than a warning. A pool guest TG cannot actuate produces a guaranteed
// detection/heal MISS on every fault, which lands in the A1/A3 denominators looking like a TG failure when it
// is an instrumentation artifact. That happened live: searxng01 was drilled three times while absent from
// TG_PROXMOX_ALLOWED_GUESTS. A benchmark engine that silently manufactures misses against the system under
// test is worse than no engine.
func (e *Engine) AssertPool() error {
	notAllowlisted, notDrilled := PoolMismatch(e.Pool, e.Allowlist)
	for _, n := range notDrilled {
		e.Log("pool: %s is actuatable by TG but never drilled — its classes accrue no evidence", n)
	}
	if len(notAllowlisted) > 0 {
		return fmt.Errorf("pool/allowlist mismatch: %v are in the injection pool but NOT in TG_PROXMOX_ALLOWED_GUESTS — "+
			"every fault there is an automatic A1/A3 miss; reconcile the two before running", notAllowlisted)
	}
	return nil
}

// ReconcileOnce repairs every outstanding restore that is due or overdue, and returns how many it closed.
//
// This is what makes a crash survivable: it runs on boot (inheriting a dead predecessor's obligations) and on
// every tick (retrying anything a transient failure missed). Each repair is VERIFIED before the ledger row is
// closed — the ledger must describe the estate, not our intentions toward it.
func (e *Engine) ReconcileOnce(ctx context.Context) int {
	out, err := e.Store.Outstanding(ctx)
	if err != nil {
		e.Log("reconcile: cannot read outstanding obligations: %v", err)
		return 0
	}
	closed := 0
	for _, act := range Reconcile(time.Now().UTC(), out) {
		o := act.Fault
		argv, target, err := UndoArgv(o)
		if err != nil {
			e.Log("reconcile: id=%d %s/%s: %v", o.ID, o.Host, o.Class, err)
			continue
		}
		if act.Overdue > 5*time.Minute {
			e.Log("reconcile: id=%d %s/%s is %s OVERDUE — repairing now", o.ID, o.Host, o.Class, act.Overdue.Round(time.Second))
		}
		// THE REPAIR'S EXIT CODE IS NOT THE ANSWER. REAL STATE IS.
		//
		// This used to short-circuit on `code == 255 || code == -1` as "transport failed, retry". The intent
		// was right — ssh reports an unreachable host as 255, and a repair that never reached the host must not
		// pass as a successful one — but 255 is OVERLOADED: it is also whatever the remote command exited with.
		// `pct start` on a guest that is ALREADY RUNNING exits 255 with "CT <id> already running", which is the
		// desired end state, not a failure. Every such obligation retried forever, never discharged, and held
		// its host permanently "busy" — measured live: one stranded row plus four legitimate faults hit the 5/5
		// throttle and stalled the whole campaign, blocking a newly-armed fault class from ever running.
		//
		// The irony is that UndoArgv already says the right thing: "`pct start` on an already-running guest
		// exits non-zero but is harmless; the caller verifies by reading status rather than trusting the exit
		// code." The caller never got to verify, because the exit code decided first.
		//
		// So: run the repair, log what it returned, and let the VERIFIER decide — it reads actual estate state
		// and is the only thing that can tell "already fine" from "never ran". This is not weaker. A repair
		// that truly never reached the host leaves the fault present, the verifier says so, and the host is
		// quarantined with that reason recorded. An unreachable host also fails the VERIFIER's own read, which
		// quarantines it too. Nothing closes an obligation except a positive reading of real state.
		if out, code, rerr := e.Exec.Run(ctx, target, argv); rerr != nil || code != 0 {
			e.Log("reconcile: id=%d %s: repair returned code=%d err=%v (%s) — deferring to the verifier, which "+
				"reads real state", o.ID, o.Host, code, rerr, strings.TrimSpace(firstLine(out)))
		}
		ok, verr := e.verifyRepaired(ctx, o)
		if verr != nil || !ok {
			reason := "verification says the fault is still present"
			if verr != nil {
				reason = verr.Error()
			}
			e.Log("reconcile: id=%d %s: repair NOT verified (%s) — host stays quarantined", o.ID, o.Host, reason)
			_ = e.Store.MarkRestoreFailed(ctx, o.ID, reason)
			continue
		}
		if err := e.Store.MarkRestored(ctx, o.ID); err != nil {
			e.Log("reconcile: id=%d repaired but ledger update failed: %v", o.ID, err)
			continue
		}
		e.Log("reconcile: id=%d %s/%s RESTORED (was %s late)", o.ID, o.Host, o.Class, act.Overdue.Round(time.Second))
		closed++
	}
	return closed
}

// Drain reconciles repeatedly until every obligation is discharged, or the deadline expires.
//
// WHY A LOOP AND NOT A SINGLE PASS: a single reconcile only repairs what is ALREADY due. An engine that
// stops (target reached, kill-switch, SIGTERM) while faults are still inside their hold window would exit
// leaving those rows 'pending' forever. Found in the first live validation run: two obligations stayed
// pending after the process exited even though the belt-and-braces timers had restored the estate correctly.
//
// The consequences are not cosmetic. Busy-ness derives from the ledger (INVARIANT 1), so a permanently
// 'pending' row QUARANTINES that host from every future campaign; and the ledger would be describing an
// estate state that is not true, which is the one thing it exists not to do.
//
// Repairs are idempotent, so discharging an obligation whose in-guest timer already fired is safe: the verify
// step simply observes the guest running / the fill gone and closes the row.
func (e *Engine) Drain(ctx context.Context, deadline time.Duration) {
	stop := time.Now().Add(deadline)
	for {
		e.ReconcileOnce(ctx)
		out, err := e.Store.Outstanding(ctx)
		if err != nil {
			e.Log("drain: cannot read outstanding obligations: %v — stopping drain", err)
			return
		}
		if len(out) == 0 {
			e.Log("drain: all obligations discharged; the ledger and the estate agree")
			return
		}
		if time.Now().After(stop) {
			for _, o := range out {
				e.Log("drain: DEADLINE EXPIRED with id=%d %s/%s still outstanding (due %s) — this host stays "+
					"quarantined until a later run reconciles it", o.ID, o.Host, o.Class, o.RestoreDueAt.Format(time.RFC3339))
			}
			return
		}
		// Sleep until the soonest obligation falls due, bounded so we re-check regularly.
		wait := min(max(time.Until(out[0].RestoreDueAt), 15*time.Second), time.Minute)
		e.Log("drain: %d obligation(s) still held; soonest due %s", len(out), out[0].RestoreDueAt.Format(time.RFC3339))
		select {
		case <-ctx.Done():
			// Even a cancelled context must not abandon obligations: keep draining on a detached context.
			ctx = context.WithoutCancel(ctx)
		case <-time.After(wait):
		}
	}
}

// verifyRepaired confirms the estate is actually back, per class.
func (e *Engine) verifyRepaired(ctx context.Context, o Outstanding) (bool, error) {
	switch o.Class {
	case ClassDeviceDown:
		up, err := VerifyGuestRunning(ctx, e.Exec, o.Node, o.FaultRef)
		if err != nil || !up {
			return up, err
		}
		// TG-226 — the guest being back is NOT the estate being back. A hard stop can leave the apps
		// inside with wedged connection pools while pct status, ICMP and device-status all read healthy;
		// that is a ~5h silent outage the harness would have closed as repaired. Only the operator's
		// declared data-path probe can tell the difference.
		probe := e.probeFor(o)
		if probe == "" {
			// LOUD ABOUT THE ABSENCE. A restore verified at guest level only is a weaker claim than one
			// verified through the app, and a check that cannot say "there was nothing to check" reads as
			// full coverage when it is not. This does NOT fail the restore: the probe is opt-in per guest
			// and refusing every undeclared guest would strand the whole pool on first run.
			e.Log("restore: %s device-down verified at GUEST level only (pct status running) — no app-health "+
				"probe declared for this guest, so a wedged connection pool would be invisible here (TG-226)", o.Host)
			return true, nil
		}
		ok, perr := VerifyAppHealthy(ctx, e.Exec, o.Host, probe)
		if perr != nil {
			return false, fmt.Errorf("app-health probe on %s: %w", o.Host, perr)
		}
		if !ok {
			e.Log("restore: %s is RUNNING but its app-health probe FAILED (%s) — the guest came back and the "+
				"application inside did not. The obligation stays outstanding and this host stays quarantined; "+
				"a human restart of the app service is what discharges it (TG-226)", o.Host, probe)
		}
		return ok, nil
	case ClassDiskFill:
		return VerifyFillRemoved(ctx, e.Exec, o.Host, o.FaultRef)
	case ClassLogFill:
		// The log is TRUNCATED, not removed, so "repaired" is a size question, not an existence one — a
		// VerifyFillRemoved here would report the fault stranded forever on a correctly-repaired estate.
		return VerifyLogTruncated(ctx, e.Exec, o.Host, o.FaultRef)
	case ClassContainerDown:
		return VerifyContainerRunning(ctx, e.Exec, o.Host, o.FaultRef)
	case ClassServiceDown:
		return VerifyServiceActive(ctx, e.Exec, o.Host, o.FaultRef)
	default:
		// FAIL CLOSED. This default used to return (true, nil) — "verified repaired" without looking at
		// anything. The caller answers a true here with MarkRestored, which this file documents as PERMANENT
		// (Outstanding selects only 'pending'/'failed', so a closed row is never revisited). So a class that
		// owed a restore and was missing from this switch would have its obligation closed unverified and its
		// fault stranded on the estate forever — silently, and permissively, which is the shape that stranded
		// two guests at 97% root disk and is why this engine exists. An unrecognised class now quarantines its
		// host instead.
		return false, fmt.Errorf("no verifier wired for class %q — refusing to assume the estate is repaired", o.Class)
	}
}

// probeFor returns the outstanding obligation's guest's declared app-health probe, or "" when the guest
// declares none (or is no longer in the pool — a pool edited mid-campaign must not make an outstanding
// obligation unverifiable).
//
// It matches on VMID rather than name because that is what a device-down obligation carries in FaultRef,
// and the VMID is the identity Proxmox itself uses; a name match would break the moment a guest is renamed
// while its fault is outstanding. Host is tried as a fallback so the lookup still works for classes whose
// FaultRef is not a VMID.
func (e *Engine) probeFor(o Outstanding) string {
	for _, g := range e.Pool {
		if g.VMID == o.FaultRef || g.Name == o.Host {
			return g.HealthProbe
		}
	}
	return ""
}

// faultRefFor derives the handle a class's undo needs. Pure, so the exhaustiveness oracle can drive it over
// the CLOSED enumeration without an engine, a store or a machine.
//
// The second return is whether the class was RECOGNISED. It exists because the fallback here is a guest VMID,
// which is a plausible-looking value for every class — an unwired class would silently record a device-down
// handle and the undo would then start a guest that was never stopped while the real fault stayed put.
func faultRefFor(d Decision) (string, bool) {
	switch d.Class {
	case ClassDeviceDown:
		return d.Guest.VMID, true
	case ClassDiskFill:
		return FillPath, true
	case ClassLogFill:
		// The DECLARED path, never a constant: the whole point of the class is that the operator names a real
		// application log, so there is nothing safe to fall back to when it is empty.
		return d.Guest.LogPath, true
	case ClassContainerDown:
		return d.Guest.Container, true
	case ClassServiceDown:
		return d.Guest.Unit, true
	case ClassMemPressure:
		return d.Guest.VMID, true // owes no restore; the handle is never consumed
	default:
		return "", false
	}
}

// armFor returns where the belt-and-braces restore timer is armed and what it runs. Pure, for the same reason
// as faultRefFor.
//
// The HOST is the safety-relevant half. A device-down restore must arm on the owning NODE because the guest
// is about to disappear — arming it inside the guest is exactly how docuseal01's cleanup was destroyed. Every
// other class leaves the guest up, so it arms on the guest. An unrecognised class returns armed=false rather
// than inheriting the device-down default, because inheriting it would arm `pct start` for a fault that never
// stopped a guest: a no-op that reads in the log as a successful arm.
func armFor(d Decision) (host string, undo []string, armed bool) {
	switch d.Class {
	case ClassDeviceDown:
		return d.Guest.Node, []string{"pct", "start", d.Guest.VMID}, true
	case ClassDiskFill:
		return d.Guest.Name, []string{"rm", "-f", "--", FillPath}, true
	case ClassLogFill:
		// Arms on the GUEST (it stays up), and TRUNCATES — the belt-and-braces timer must run the same repair
		// as the reconciler, or the two would disagree about what "restored" means.
		return d.Guest.Name, []string{"truncate", "-s", "0", "--", d.Guest.LogPath}, true
	case ClassServiceDown:
		return d.Guest.Name, []string{"systemctl", "start", d.Guest.Unit}, true
	case ClassContainerDown:
		return d.Guest.Name, []string{"docker", "start", d.Guest.Container}, true
	default:
		return "", nil, false
	}
}

// InjectOnce performs at most one injection. Returns true when a fault was actually injected.
//
// ORDERING IS THE SAFETY PROPERTY: the obligation is recorded BEFORE the effect. A crash between the two
// leaves a recorded-but-not-performed fault, which is harmless because repairs are idempotent. The reverse
// order would leave a performed-but-unrecorded fault — precisely the stranding this engine exists to prevent.
func (e *Engine) InjectOnce(ctx context.Context, injected, cycle int) bool {
	if e.killed() {
		e.Log("kill-switch file present (%s) — stopping", e.KillFile)
		return false
	}
	breakerOpen, berr := e.Store.BreakerOpen(ctx)
	if berr != nil {
		e.Log("breaker read failed, treating as OPEN: %v", berr)
	}
	snap, err := e.Snapshot(ctx)
	if err != nil {
		e.Log("snapshot failed (%v) — refusing to fault blind this tick", err)
		return false
	}
	// The recovery-settle lookback. A restore older than the window can never block a decision, so ask for
	// exactly the window. A read failure must NOT silently disable the guard: an empty map would make every
	// target look settled, which is the fail-OPEN direction — so treat it as "everything is still settling"
	// by refusing to inject this cycle. One skipped injection is cheap; an undetectable fault pollutes the
	// benchmark denominator permanently.
	settling, serr := e.Store.RecentRestores(ctx, e.Limits.SettleWindow)
	if serr != nil && e.Limits.SettleWindow > 0 {
		e.Log("cannot read recent restores (%v) — skipping this injection rather than risk an UNDETECTABLE fault", serr)
		return false
	}

	outstanding, err := e.Store.Outstanding(ctx)
	if err != nil {
		e.Log("cannot read outstanding obligations (%v) — refusing to inject without knowing what is broken", err)
		return false
	}

	st := State{
		Now: time.Now().UTC(), Pool: e.Pool, Allowlist: e.Allowlist, Status: snap,
		Outstanding: outstanding, BreakerOpen: breakerOpen,
		KillSwitch: e.Store.KillSwitchEngaged(ctx), Injected: injected, Cycle: cycle, Limits: e.Limits,
		Settling: settling,
	}
	d := PlanNext(st, e.Rotation)
	if !d.Act {
		e.Log("no injection: %s", d.Reason)
		return false
	}

	ran, _ := e.InjectGuest(ctx, d.Guest, d.Class)
	return ran
}

// InjectGuest performs the assert → record-obligation → effect → arm-restore sequence for an ALREADY-CHOSEN
// guest+class — the body InjectOnce runs after planning. observeprobe.Orchestrator does its own kill/breaker/
// snapshot/outstanding/rotation gating via its seams, so it drives injection through this method, reusing the
// record-before-effect safety ordering rather than re-implementing it. ran is true ONLY when the effect
// committed; a pre-effect refusal returns (false, err wrapping ErrPreEffect), an ambiguous failure returns
// (false, err) with the obligation left PENDING for the reconciler. TG-180.
func (e *Engine) InjectGuest(ctx context.Context, g PoolGuest, c Class) (bool, error) {
	d := Decision{Act: true, Guest: g, Class: c}
	if err := AssertGuestName(ctx, e.Exec, d.Guest.Node, d.Guest.VMID, d.Guest.Name); err != nil {
		e.Log("%v", err)
		return false, err
	}
	faultRef, known := faultRefFor(d)
	if !known {
		return false, fmt.Errorf("%w: class %q has no fault_ref rule", ErrPreEffect, d.Class)
	}
	id, err := e.Store.RecordInjection(ctx, d.Guest.Name, d.Class, d.Guest.Node, faultRef, e.Limits.RestoreAfter, e.Note)
	if err != nil {
		e.Log("could not record the obligation (%v) — NOT injecting; an unrecorded fault is how a guest gets stranded", err)
		return false, fmt.Errorf("record obligation for %s: %w", d.Guest.Name, err)
	}
	if err := e.performInjection(ctx, d); err != nil {
		if errors.Is(err, ErrPreEffect) {
			e.Log("id=%d injection aborted BEFORE any effect on %s: %v — closing the obligation (provably nothing was broken)", id, d.Guest.Name, err)
			_ = e.Store.MarkRestored(ctx, id)
			return false, err
		}
		e.Log("id=%d injection failed AMBIGUOUSLY on %s: %v — obligation stays PENDING (the effect may have committed before the failure); the reconciler owns the repair and the host stays quarantined", id, d.Guest.Name, err)
		_ = e.Store.MarkRestoreFailed(ctx, id, "ambiguous injection failure: "+err.Error())
		return false, err
	}
	if d.Class.OwesRestore() {
		armHost, undo, armed := armFor(d)
		if !armed {
			e.Log("id=%d class %q owes a restore but has no arm rule — skipping the timer; the ledger reconciler still owns the repair", id, d.Class)
		} else if err := ArmDeferredRestore(ctx, e.Exec, armHost, d.Class, d.Guest.VMID, e.Limits.RestoreAfter, undo); err != nil {
			e.Log("id=%d deferred-restore arm failed on %s: %v (the ledger reconciler still owns the repair)", id, armHost, err)
		}
	}
	e.Log("id=%d INJECTED %s on %s (%s) — restore due in %s", id, d.Class, d.Guest.Name, d.Guest.Node, e.Limits.RestoreAfter)
	return true, nil
}

func (e *Engine) performInjection(ctx context.Context, d Decision) error {
	switch d.Class {
	case ClassDeviceDown:
		return InjectDeviceDown(ctx, e.Exec, d.Guest.Node, d.Guest.VMID)
	case ClassContainerDown:
		return InjectContainerDown(ctx, e.Exec, d.Guest.Name, d.Guest.Container)
	case ClassServiceDown:
		return InjectServiceDown(ctx, e.Exec, d.Guest.Name, d.Guest.Unit)
	case ClassLogFill:
		plan, err := InjectLogFill(ctx, e.Exec, d.Guest.Name, d.Guest.LogPath)
		if err != nil {
			return err
		}
		e.Log("  log-fill %s: grew %s by %d MiB, %d MiB free after", d.Guest.Name, d.Guest.LogPath, plan.AllocBytes>>20, plan.FreeAfter>>20)
		return nil
	case ClassDiskFill:
		plan, err := InjectDiskFill(ctx, e.Exec, d.Guest.Name)
		if err != nil {
			return err
		}
		e.Log("  disk-fill %s: allocated %d MiB, %d MiB free after", d.Guest.Name, plan.AllocBytes>>20, plan.FreeAfter>>20)
		return nil
	default:
		// Provably pre-effect: there is no remote command for an unwired class, so nothing ran.
		return fmt.Errorf("%w: class %q has no injector wired", ErrPreEffect, d.Class)
	}
}

// Run is the supervised loop: reconcile, then maybe inject, forever (or until the target/kill-switch).
//
// Reconcile runs FIRST and on EVERY tick, before any new fault is considered, so obligations inherited from a
// crashed predecessor are discharged before more load is added.
func (e *Engine) Run(ctx context.Context) error {
	if err := e.AssertPool(); err != nil {
		return err
	}
	e.Log("faultinjector START — pool=%d cadence=%s restore-after=%s maxdown=%d maxbusy=%d target=%d",
		len(e.Pool), e.Cadence, e.Limits.RestoreAfter, e.Limits.MaxDown, e.Limits.MaxBusy, e.Limits.Target)

	if n := e.ReconcileOnce(ctx); n > 0 {
		e.Log("boot reconcile: discharged %d obligation(s) inherited from a previous run", n)
	}

	injected := 0
	// cycle counts TICKS and drives the class rotation; injected counts LANDED faults and drives the
	// campaign target. Conflating them froze the rotation on a class that could not act (see State.Cycle).
	cycle := 0
	// barren counts consecutive cycles that produced no fault, so a dead campaign escalates instead of
	// reporting a reassuring per-tick sentence forever.
	barren := 0
	for {
		if ctx.Err() != nil {
			e.Log("context cancelled — draining outstanding obligations before exit")
			e.Drain(context.WithoutCancel(ctx), e.DrainDeadline())
			return ctx.Err()
		}
		if e.killed() || e.Store.KillSwitchEngaged(ctx) {
			e.Log("kill-switch engaged — draining outstanding obligations, then stopping (injected=%d)", injected)
			e.Drain(ctx, e.DrainDeadline())
			return nil
		}
		if e.Limits.Target > 0 && injected >= e.Limits.Target {
			e.Log("target reached (%d) — draining outstanding obligations, then stopping", e.Limits.Target)
			e.Drain(ctx, e.DrainDeadline())
			return nil
		}

		e.ReconcileOnce(ctx)
		if e.InjectOnce(ctx, injected, cycle) {
			injected++
			barren = 0
		} else {
			barren++
			// ★ A HARNESS THAT PRODUCES NOTHING MUST SAY SO IN ONE LINE, NOT IN 148. Between 02:19Z and
			// 09:34Z on 2026-07-29 this loop logged a truthful, reassuring sentence every three minutes —
			// "provably nothing was broken" — while the campaign produced ZERO faults and every benchmark
			// axis was being computed over near-empty volume. Each line was correct; the SEQUENCE was the
			// finding, and nothing named it. The per-tick reason stays (it is how the cause is diagnosed);
			// this adds the sentence an operator can actually notice, at the powers of two so a long outage
			// escalates rather than scrolling past at a fixed rate.
			if barren >= 4 && barren&(barren-1) == 0 {
				e.Log("★ CAMPAIGN BARREN: %d consecutive cycles produced no fault (~%s). Every benchmark axis "+
					"is being measured over this window — treat A1/A3/A4/A5 figures from it as unpopulated, "+
					"not as results. Cause is the per-cycle reason logged above.",
					barren, (time.Duration(barren) * e.Cadence).Round(time.Minute))
			}
		}
		// The rotation cursor advances on every CYCLE, landed or not. Tying it to `injected` meant a class
		// that could not act re-selected itself forever and starved every other class out of the campaign —
		// see State.Cycle for the 7.5-hour live incident that behaviour caused.
		cycle++

		select {
		case <-ctx.Done():
		case <-time.After(e.Cadence):
		}
	}
}

// DrainDeadline bounds how long a shutdown will wait for outstanding obligations. It is generous relative to
// the hold window — a fault injected moments before the stop signal must still be discharged rather than
// abandoned — with a floor so a tiny RestoreAfter cannot make the drain give up instantly.
func (e *Engine) DrainDeadline() time.Duration {
	d := e.Limits.RestoreAfter * 2
	if d < 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}

// firstLine keeps a remote command's output to one line for the log — `pct` and `docker` write a short
// diagnostic ("CT 101101209 already running") that is exactly what an operator needs to see, while a failing
// command can emit pages.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
