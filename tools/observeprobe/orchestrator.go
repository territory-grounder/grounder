package observeprobe

import (
	"context"
	"time"

	"github.com/territory-grounder/grounder/tools/faultinjector"
)

// Injector performs ONE probe fault injection. The production implementation wraps tools/faultinjector's engine
// — which name-asserts the guest, records the estate-mutation ledger BEFORE the effect, and owns the
// self-reverting restore — so this package never re-implements injection. Tests supply a spy that records the
// call WITHOUT touching an estate, which is how the DEFAULT-OFF proof asserts "injected nothing".
//
// ran is true ONLY when the fault actually committed on the estate. A pre-effect refusal (blind snapshot, a
// name-assert mismatch, an undeclared target) returns ran=false so the verdict reads INCONCLUSIVE — a probe
// that never perturbed the estate is never counted as a confirmed coverage gap.
type Injector interface {
	Inject(ctx context.Context, g faultinjector.PoolGuest, c faultinjector.Class, window time.Duration) (ran bool, err error)
}

// PendingProbe is a probe awaiting a verdict — its window may or may not have closed yet.
type PendingProbe struct {
	ID  int64
	Run ProbeRun
}

// ProbeStore is the DURABLE probe ledger the orchestrator reasons from. Pending runs MUST be durable: a probe
// injected in one cycle is decided in a LATER cycle after its window closes, so holding that obligation in
// process memory would lose it across a restart — the exact class of failure faultinjector's durable ledger
// exists to prevent, here applied to the measurement rather than the estate.
type ProbeStore interface {
	RecordProbe(ctx context.Context, run ProbeRun) (int64, error)
	SetVerdict(ctx context.Context, id int64, v Verdict) error
	PendingProbes(ctx context.Context) ([]PendingProbe, error)
	// ConfirmedHosts is the set of hosts with a TERMINAL verdict — the coverage numerator AND (unioned with the
	// pending hosts) the "already probed" set the planner must not re-probe.
	ConfirmedHosts(ctx context.Context) (map[string]bool, error)
}

// AlertReader returns the received-at times of admitted alerts on a host within [since, until] — the
// front-door evidence the verdict reads (ingest_alert = what TG actually admitted, not a raw provider feed).
type AlertReader interface {
	AlertTimes(ctx context.Context, host string, since, until time.Time) ([]time.Time, error)
}

// Orchestrator ties the planner to the (default-OFF) injection and the verdict. It does not own a loop — the
// caller ticks RunCycle, exactly as the census collector is scraped — so it is testable a cycle at a time with
// a fake clock, and there is no foreground injector restart (the one-cycle-suite / foreground-restart traps).
type Orchestrator struct {
	// Enabled is the DEFAULT-OFF arming gate. False (TG_OBSERVE_PROBE_ENABLED absent) ⇒ the orchestrator PLANS
	// and LOGS what it would probe but injects NOTHING and records NO new run — the whole perturbing path is
	// dark. Arming is an owner decision (the epic's lowest safety sub-score); this one field is the single point
	// that decides whether a real fault ever reaches the estate.
	Enabled bool

	Injector Injector
	Store    ProbeStore
	Alerts   AlertReader

	// Live-input seams — plain functions so the orchestrator needs no DB or estate to be exercised.
	Unobservable func() []string                                            // current census-unobservable hosts, fresh each cycle
	Snapshot     func(context.Context) (map[string]string, error)           // vmid -> running/stopped
	Outstanding  func(context.Context) ([]faultinjector.Outstanding, error) // faultinjector's restore ledger (no-stacking)
	BreakerOpen  func(context.Context) (bool, error)                        // TG mutation breaker; nil ⇒ treated closed
	KillSwitch   func(context.Context) bool                                 // nil ⇒ not engaged

	Pool      []faultinjector.PoolGuest
	Allowlist map[string]bool
	Classes   []faultinjector.Class
	Window    time.Duration

	Now func() time.Time
	Log func(string, ...any)
}

// RunCycle performs one orchestration tick: reconcile any verdicts whose window has closed (this NEVER injects,
// so it runs whether or not probing is armed — an owner who disables mid-flight still gets the verdicts for
// probes already in flight), then plan and, only if armed, inject at most one new probe.
func (o *Orchestrator) RunCycle(ctx context.Context) {
	o.reconcileVerdicts(ctx)
	o.maybeProbe(ctx)
}

// reconcileVerdicts decides every pending probe whose observation window has fully closed, reading the alerts
// that did (or did not) surface and recording the terminal verdict. It touches the store and the alert reader
// only — never the injector — so it adds no estate load.
func (o *Orchestrator) reconcileVerdicts(ctx context.Context) {
	pend, err := o.Store.PendingProbes(ctx)
	if err != nil {
		o.logf("observation probe: cannot read pending probes: %v", err)
		return
	}
	now := o.now()
	for _, p := range pend {
		if now.Before(p.Run.WindowEnd) {
			continue // window still open — decide on a later cycle (the cross-cycle discipline)
		}
		alerts, aerr := o.Alerts.AlertTimes(ctx, p.Run.Host, p.Run.InjectedAt, p.Run.WindowEnd)
		if aerr != nil {
			o.logf("observation probe: id=%d cannot read alerts for %s (%v) — leaving pending", p.ID, p.Run.Host, aerr)
			continue
		}
		// Decide from the STORED run — including its Ran flag. A committed probe recorded Ran=true; an aborted
		// probe is set inconclusive at inject time and never reaches here. The one edge is a crash BETWEEN
		// recording an aborted probe (Ran=false) and writing its inconclusive verdict: that row is left pending
		// with Ran=false, and Decide then returns INCONCLUSIVE rather than a false gap. Trusting the stored flag
		// (not hardcoding "it ran") is what keeps a never-ran probe from ripening into a confirmed blind spot.
		v, reason := Decide(p.Run, alerts, now)
		if err := o.Store.SetVerdict(ctx, p.ID, v); err != nil {
			o.logf("observation probe: id=%d verdict write failed: %v", p.ID, err)
			continue
		}
		o.logf("observation probe FINDING: id=%d %s [%s] -> %s: %s", p.ID, p.Run.Host, p.Run.Class, v, reason)
	}
}

// maybeProbe plans one probe and, only when armed, injects it and records the run. When disarmed it logs the
// plan and returns without calling the injector or the store — the DEFAULT-OFF property. Either way it surfaces
// the uncoverable remainder, so no census-unobservable host is silently excluded from the coverage question.
func (o *Orchestrator) maybeProbe(ctx context.Context) {
	st, ok := o.planState(ctx)
	if !ok {
		return // an input read failed and planState logged why; fail closed — no probe this cycle
	}
	d := PlanProbe(st)

	if len(d.Uncoverable) > 0 {
		o.logf("observation probe: %d census-unobservable host(s) are UNCOVERABLE (not a guinea-pig or not TG-actuatable) — the probe cannot test them, so the coverage denominator names them explicitly: %v",
			len(d.Uncoverable), d.Uncoverable)
	}

	if !o.Enabled {
		if d.Act {
			o.logf("observation probe DISABLED (TG_OBSERVE_PROBE_ENABLED unset) — WOULD probe %s (%s) with %s over %s; injecting nothing",
				d.Guest.Name, d.Guest.VMID, d.Class, o.Window)
		} else {
			o.logf("observation probe DISABLED — no probe this cycle: %s", d.Reason)
		}
		return
	}

	if !d.Act {
		o.logf("observation probe: no probe this cycle: %s", d.Reason)
		return
	}

	// ARMED. Perturb the estate via faultinjector (which records the mutation ledger and owns the restore),
	// then record the probe run so a LATER cycle can decide it once the window closes.
	now := o.now()
	ran, ierr := o.Injector.Inject(ctx, d.Guest, d.Class, o.Window)
	if ierr != nil {
		o.logf("observation probe: injection on %s (%s) errored: %v", d.Guest.Name, d.Class, ierr)
	}
	run := ProbeRun{Host: d.Guest.Name, Class: d.Class, InjectedAt: now, WindowEnd: now.Add(o.Window), Ran: ran}
	id, rerr := o.Store.RecordProbe(ctx, run)
	if rerr != nil {
		o.logf("observation probe: could not record probe run for %s: %v", d.Guest.Name, rerr)
		return
	}
	if !ran {
		// Injection aborted before any effect — nothing was perturbed, so there is nothing to observe. Decide
		// it INCONCLUSIVE now rather than waiting a whole window for an alert that cannot come; this is the
		// discriminator that keeps a never-ran probe out of the confirmed-gap count.
		_ = o.Store.SetVerdict(ctx, id, VerdictInconclusive)
		o.logf("observation probe FINDING: id=%d %s -> inconclusive: injection aborted before any effect; nothing perturbed, nothing to observe", id, d.Guest.Name)
		return
	}
	o.logf("observation probe: id=%d INJECTED %s on %s (%s) — verdict due after %s", id, d.Class, d.Guest.Name, d.Guest.VMID, o.Window)
}

// planState assembles the pure planner's input from the live seams, failing CLOSED (returns ok=false) on any
// read error — a probe planned on a missing snapshot or an unknown fault ledger could stack a fault or fault
// blind, so a doubtful cycle simply does not probe.
func (o *Orchestrator) planState(ctx context.Context) (ProbeState, bool) {
	snap, err := o.Snapshot(ctx)
	if err != nil {
		o.logf("observation probe: snapshot failed (%v) — refusing to probe blind this cycle", err)
		return ProbeState{}, false
	}
	out, err := o.Outstanding(ctx)
	if err != nil {
		o.logf("observation probe: cannot read outstanding faults (%v) — refusing to probe without knowing what is broken", err)
		return ProbeState{}, false
	}
	breaker := false
	if o.BreakerOpen != nil {
		if b, berr := o.BreakerOpen(ctx); berr != nil {
			o.logf("observation probe: breaker read failed (%v) — treating as OPEN", berr)
			breaker = true
		} else {
			breaker = b
		}
	}
	kill := false
	if o.KillSwitch != nil {
		kill = o.KillSwitch(ctx)
	}
	already, aerr := o.alreadyProbed(ctx)
	if aerr != nil {
		o.logf("observation probe: cannot read probe history (%v) — refusing to probe (a re-probe of a busy host risks stacking)", aerr)
		return ProbeState{}, false
	}
	return ProbeState{
		Unobservable:  o.Unobservable(),
		Pool:          o.Pool,
		Allowlist:     o.Allowlist,
		Status:        snap,
		Outstanding:   out,
		AlreadyProbed: already,
		Classes:       o.Classes,
		BreakerOpen:   breaker,
		KillSwitch:    kill,
	}, true
}

// alreadyProbed is the union of hosts with a terminal verdict and hosts with a probe still pending — the set
// the planner must not re-probe. An inconclusive-only host is in NEITHER, so it is re-probeable.
func (o *Orchestrator) alreadyProbed(ctx context.Context) (map[string]bool, error) {
	confirmed, err := o.Store.ConfirmedHosts(ctx)
	if err != nil {
		return nil, err
	}
	pend, err := o.Store.PendingProbes(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(confirmed)+len(pend))
	for h := range confirmed {
		out[h] = true
	}
	for _, p := range pend {
		out[p.Run.Host] = true
	}
	return out, nil
}

// CoverageNow computes the current coverage-of-the-unmeasured dimension: the CURRENT census-unobservable set
// against the probe-confirmed hosts, BOTH read here together so the denominator and numerator share one
// freshness. Safe to call whether or not probing is armed — disarmed, ConfirmedHosts is empty and the honest
// reading is "0 of N unobservable entities probe-confirmed", which is exactly the default-OFF scorecard.
func (o *Orchestrator) CoverageNow(ctx context.Context) (CoverageResult, error) {
	confirmed, err := o.Store.ConfirmedHosts(ctx)
	if err != nil {
		return CoverageResult{}, err
	}
	return Coverage(o.Unobservable(), confirmed), nil
}

func (o *Orchestrator) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now().UTC()
}

func (o *Orchestrator) logf(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}
