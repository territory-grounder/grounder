package observeprobe

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/tools/faultinjector"
)

// ---- fakes: no estate, no DB ----

type spyInjector struct {
	calls  int
	ran    bool // what Inject reports (did the fault commit?)
	err    error
	lastG  faultinjector.PoolGuest
	lastC  faultinjector.Class
	lastWi time.Duration
}

func (s *spyInjector) Inject(_ context.Context, g faultinjector.PoolGuest, c faultinjector.Class, w time.Duration) (bool, error) {
	s.calls++
	s.lastG, s.lastC, s.lastWi = g, c, w
	return s.ran, s.err
}

type storedProbe struct {
	id      int64
	run     ProbeRun
	verdict Verdict
}

type fakeStore struct {
	rows   []*storedProbe
	nextID int64
}

func (s *fakeStore) RecordProbe(_ context.Context, run ProbeRun) (int64, error) {
	s.nextID++
	s.rows = append(s.rows, &storedProbe{id: s.nextID, run: run, verdict: VerdictPending})
	return s.nextID, nil
}
func (s *fakeStore) SetVerdict(_ context.Context, id int64, v Verdict) error {
	for _, r := range s.rows {
		if r.id == id {
			r.verdict = v
			return nil
		}
	}
	return fmt.Errorf("no probe id=%d", id)
}
func (s *fakeStore) PendingProbes(context.Context) ([]PendingProbe, error) {
	var out []PendingProbe
	for _, r := range s.rows {
		if r.verdict == VerdictPending {
			out = append(out, PendingProbe{ID: r.id, Run: r.run})
		}
	}
	return out, nil
}
func (s *fakeStore) ConfirmedHosts(context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	for _, r := range s.rows {
		if r.verdict.Terminal() {
			out[r.run.Host] = true
		}
	}
	return out, nil
}
func (s *fakeStore) verdictOf(id int64) Verdict {
	for _, r := range s.rows {
		if r.id == id {
			return r.verdict
		}
	}
	return ""
}

type fakeAlerts struct{ byHost map[string][]time.Time }

func (a *fakeAlerts) AlertTimes(_ context.Context, host string, since, until time.Time) ([]time.Time, error) {
	var out []time.Time
	for _, t := range a.byHost[host] {
		if !t.Before(since) && !t.After(until) {
			out = append(out, t)
		}
	}
	return out, nil
}

// newOrch builds a single-candidate (gp-01) orchestrator over the fakes and a caller-controlled clock.
func newOrch(enabled bool, inj Injector, store ProbeStore, alerts AlertReader, clock *time.Time) *Orchestrator {
	g := gp("gp-01", "101")
	return &Orchestrator{
		Enabled:      enabled,
		Injector:     inj,
		Store:        store,
		Alerts:       alerts,
		Unobservable: func() []string { return []string{"gp-01"} },
		Snapshot:     func(context.Context) (map[string]string, error) { return runningSnap(g), nil },
		Outstanding:  func(context.Context) ([]faultinjector.Outstanding, error) { return nil, nil },
		Pool:         []faultinjector.PoolGuest{g},
		Allowlist:    map[string]bool{"gp-01": true},
		Classes:      []faultinjector.Class{faultinjector.ClassDeviceDown},
		Window:       window,
		Now:          func() time.Time { return *clock },
		Log:          func(string, ...any) {},
	}
}

// THE DEFAULT-OFF PROOF. With arming absent, a full cycle over a state that WOULD probe injects NOTHING and
// records NO run — the injector is never called and the ledger stays empty. This is the single guarantee that
// no fault reaches the estate until an owner arms it.
func TestOrchestrator_DefaultOff_InjectsNothing(t *testing.T) {
	clock := t0
	spy := &spyInjector{ran: true}
	store := &fakeStore{}
	o := newOrch(false, spy, store, &fakeAlerts{}, &clock)

	o.RunCycle(context.Background())

	if spy.calls != 0 {
		t.Fatalf("injector called %d times with probing DISABLED — the default-OFF gate leaked a real injection", spy.calls)
	}
	if len(store.rows) != 0 {
		t.Fatalf("recorded %d probe run(s) while disabled — nothing should be recorded when nothing is injected", len(store.rows))
	}
}

// Armed, the same state fires the injector exactly once with the planned guest/class/window and records a
// PENDING run whose window is one Window ahead — proving the gate is the ONLY thing that held the injection.
func TestOrchestrator_Enabled_FiresInjectorAndRecordsPending(t *testing.T) {
	clock := t0
	spy := &spyInjector{ran: true}
	store := &fakeStore{}
	o := newOrch(true, spy, store, &fakeAlerts{}, &clock)

	o.RunCycle(context.Background())

	if spy.calls != 1 {
		t.Fatalf("injector called %d times, want exactly 1", spy.calls)
	}
	if spy.lastG.Name != "gp-01" || spy.lastC != faultinjector.ClassDeviceDown || spy.lastWi != window {
		t.Fatalf("injected %s/%s/%s, want gp-01/device-down/%s", spy.lastG.Name, spy.lastC, spy.lastWi, window)
	}
	if len(store.rows) != 1 || store.rows[0].verdict != VerdictPending {
		t.Fatalf("store rows=%d verdict=%q, want 1 pending", len(store.rows), store.verdictOf(1))
	}
	if !store.rows[0].run.WindowEnd.Equal(t0.Add(window)) {
		t.Fatalf("WindowEnd=%v, want t0+window", store.rows[0].run.WindowEnd)
	}
}

// THE CROSS-CYCLE FIXTURE. A probe injected in cycle 1 must NOT be decided until its window closes in a later
// cycle. The defect (concluding a gap prematurely) only shows across cycles: after cycle 1 the verdict is still
// PENDING; only after the window closes, in cycle 2, does the same run — with no alert — become a confirmed
// gap. A one-cycle orchestrator would have branded it a gap immediately.
func TestOrchestrator_CrossCycle_ConfirmsGapOnlyAfterWindow(t *testing.T) {
	clock := t0
	spy := &spyInjector{ran: true}
	store := &fakeStore{}
	o := newOrch(true, spy, store, &fakeAlerts{}, &clock) // no alerts for gp-01

	// Cycle 1 at t0: inject + record pending. Reconcile runs first (nothing pending yet), then injects.
	o.RunCycle(context.Background())
	if v := store.verdictOf(1); v != VerdictPending {
		t.Fatalf("after cycle 1 verdict=%q, want pending — a probe must not be decided in the cycle it was injected", v)
	}

	// A second cycle STILL inside the window must not decide it either (and must not re-probe the pending host).
	clock = t0.Add(window / 2)
	o.RunCycle(context.Background())
	if v := store.verdictOf(1); v != VerdictPending {
		t.Fatalf("mid-window verdict=%q, want pending — absence of an alert is not yet evidence", v)
	}
	if spy.calls != 1 {
		t.Fatalf("injector called %d times — a host with a pending probe must not be re-probed", spy.calls)
	}

	// Cycle 3 AFTER the window closes: reconcile decides the gap.
	clock = t0.Add(window + time.Second)
	o.RunCycle(context.Background())
	if v := store.verdictOf(1); v != VerdictUnobservableConfirmed {
		t.Fatalf("post-window verdict=%q, want unobservable_confirmed", v)
	}
}

// The cross-cycle OBSERVABLE path: an alert that surfaced inside the window makes the post-window verdict
// observable, not a gap.
func TestOrchestrator_CrossCycle_ObservableWhenAlertSurfaced(t *testing.T) {
	clock := t0
	spy := &spyInjector{ran: true}
	store := &fakeStore{}
	alerts := &fakeAlerts{byHost: map[string][]time.Time{"gp-01": {t0.Add(3 * time.Minute)}}}
	o := newOrch(true, spy, store, alerts, &clock)

	o.RunCycle(context.Background()) // cycle 1: inject
	clock = t0.Add(window + time.Second)
	o.RunCycle(context.Background()) // cycle 2: decide

	if v := store.verdictOf(1); v != VerdictObservable {
		t.Fatalf("verdict=%q, want observable — an in-window alert reclassifies the entity", v)
	}
}

// A pre-effect abort (Inject reports ran=false) is recorded INCONCLUSIVE immediately — never left pending to
// ripen into a false gap. This is the never-ran-is-not-a-gap discriminator at the orchestrator level.
func TestOrchestrator_Enabled_PreEffectAbort_IsInconclusive(t *testing.T) {
	clock := t0
	spy := &spyInjector{ran: false} // injection refused before any effect
	store := &fakeStore{}
	o := newOrch(true, spy, store, &fakeAlerts{}, &clock)

	o.RunCycle(context.Background())
	if spy.calls != 1 {
		t.Fatalf("injector calls=%d, want 1", spy.calls)
	}
	if v := store.verdictOf(1); v != VerdictInconclusive {
		t.Fatalf("verdict=%q, want inconclusive for a probe that aborted pre-effect", v)
	}
	// And it must never read as a confirmed gap, even long after the window would have closed.
	clock = t0.Add(window + time.Hour)
	o.RunCycle(context.Background())
	if v := store.verdictOf(1); v == VerdictUnobservableConfirmed {
		t.Fatal("a never-ran probe ripened into unobservable_confirmed — the false-gap trap")
	}
}

// Reconcile runs even when DISABLED — an owner who disarms mid-flight still gets the verdicts for probes
// already in flight — and it never injects while doing so.
func TestOrchestrator_Disabled_StillReconcilesPendingWithoutInjecting(t *testing.T) {
	clock := t0.Add(window + time.Second) // the window has already closed
	spy := &spyInjector{ran: true}
	store := &fakeStore{}
	// seed a pending probe injected at t0 whose window has closed, with no alert.
	_, _ = store.RecordProbe(context.Background(), ProbeRun{Host: "gp-01", Class: faultinjector.ClassDeviceDown, InjectedAt: t0, WindowEnd: t0.Add(window), Ran: true})
	o := newOrch(false, spy, store, &fakeAlerts{}, &clock)

	o.RunCycle(context.Background())
	if spy.calls != 0 {
		t.Fatalf("injector called %d times while disabled — reconcile must never inject", spy.calls)
	}
	if v := store.verdictOf(1); v != VerdictUnobservableConfirmed {
		t.Fatalf("verdict=%q, want unobservable_confirmed — a disabled orchestrator still finishes in-flight verdicts", v)
	}
}

// CRASH-WINDOW SAFETY: a pending row whose stored Ran is false (a probe recorded aborted, then a crash before
// its inconclusive verdict was written) must reconcile to INCONCLUSIVE, never to a confirmed gap — the verdict
// trusts the stored Ran flag rather than assuming a pending row committed.
func TestOrchestrator_ReconcilePendingButNeverRan_IsInconclusiveNotGap(t *testing.T) {
	clock := t0.Add(window + time.Second) // window closed
	store := &fakeStore{}
	// a pending row that never actually ran, no alerts.
	_, _ = store.RecordProbe(context.Background(), ProbeRun{Host: "gp-01", Class: faultinjector.ClassDeviceDown, InjectedAt: t0, WindowEnd: t0.Add(window), Ran: false})
	o := newOrch(true, &spyInjector{}, store, &fakeAlerts{}, &clock)

	o.reconcileVerdicts(context.Background())
	if v := store.verdictOf(1); v != VerdictInconclusive {
		t.Fatalf("verdict=%q, want inconclusive — a pending row that never ran must not become a confirmed gap", v)
	}
}

// The coverage dimension is published even DEFAULT-OFF: the denominator is the current unobservable count and
// the numerator is 0 (nothing confirmed), i.e. "0 of N probe-confirmed" — the honest default-OFF scorecard.
func TestOrchestrator_CoverageNow_DefaultOffIsZeroOverDenominator(t *testing.T) {
	clock := t0
	store := &fakeStore{}
	o := newOrch(false, &spyInjector{}, store, &fakeAlerts{}, &clock)

	cov, err := o.CoverageNow(context.Background())
	if err != nil {
		t.Fatalf("CoverageNow: %v", err)
	}
	if cov.Unobservable != 1 || cov.Confirmed != 0 || cov.Ratio() != 0 {
		t.Fatalf("coverage=%+v ratio=%v, want denominator 1, numerator 0 (default-OFF)", cov, cov.Ratio())
	}

	// Once a probe confirms gp-01, the numerator moves — the denominator is unchanged, so coverage is real.
	_, _ = store.RecordProbe(context.Background(), ProbeRun{Host: "gp-01"})
	_ = store.SetVerdict(context.Background(), 1, VerdictUnobservableConfirmed)
	cov, _ = o.CoverageNow(context.Background())
	if cov.Confirmed != 1 || cov.Ratio() != 1 {
		t.Fatalf("coverage after a confirm=%+v ratio=%v, want numerator 1 / ratio 1", cov, cov.Ratio())
	}
}
