package faultinjector

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

func pool3() []PoolGuest {
	return []PoolGuest{
		{VMID: "101", Name: "alpha", Node: "pve01"},
		{VMID: "102", Name: "bravo", Node: "pve01"},
		{VMID: "103", Name: "charlie", Node: "pve03"},
	}
}

func allow(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func running(vmids ...string) map[string]string {
	m := map[string]string{}
	for _, v := range vmids {
		m[v] = "running"
	}
	return m
}

func baseState() State {
	return State{
		Now:       now,
		Pool:      pool3(),
		Allowlist: allow("alpha", "bravo", "charlie"),
		Status:    running("101", "102", "103"),
		Limits:    Limits{MaxDown: 2, MaxBusy: 2, Target: 0, RestoreAfter: 30 * time.Minute},
	}
}

var rot = []Class{ClassDeviceDown, ClassDiskFill, ClassMemPressure}

// ---------------------------------------------------------------------------------------------------
// REGRESSION: the two live strandings of 2026-07-26. These are the reason this package exists; if either
// ever goes green-to-red, the engine can again leave a production guest broken.
// ---------------------------------------------------------------------------------------------------

// PATH A — "the timer dies with its guest". The bash engine disk-filled docuseal01 (arming a TRANSIENT
// in-guest cleanup unit), then 19 minutes later device-downed the SAME guest. Stopping the guest destroyed
// the pending unit, so the 5.2GB fill was never cleaned and the guest sat at 97% root disk.
//
// The fix is structural: a host that owes a restore is never selected again, for ANY class.
func TestPlanNext_NeverStacksAFaultOnAHostThatOwesARestore(t *testing.T) {
	st := baseState()
	st.Outstanding = []Outstanding{{
		ID: 1, Host: "alpha", Class: ClassDiskFill, Node: "pve01",
		FaultRef: "/var/tmp/tg-fill.img", RestoreDueAt: now.Add(20 * time.Minute),
	}}

	// Sweep the whole rotation and every offset: "alpha" must never be chosen while it owes a restore.
	for i := range 30 {
		st.Injected = i
		d := PlanNext(st, rot)
		if d.Act && d.Guest.Name == "alpha" {
			t.Fatalf("injected=%d: selected %q which owes a %s restore — this is the docuseal01 stranding (PATH A)",
				i, d.Guest.Name, ClassDiskFill)
		}
	}
}

// PATH B — "memory resets on restart". The bash engine held busy-ness in an in-process map. It died, was
// restarted, came back with an empty map, and immediately re-faulted myspeed01 which still had an
// un-restored fill — stranding it at 97% disk.
//
// The fix is that busy-ness is derived ONLY from the durable ledger, so a process with NO history reaches the
// same decision as one that has been running for hours.
func TestPlanNext_FreshProcessHonoursLedgerBusyness(t *testing.T) {
	outstanding := []Outstanding{
		{ID: 1, Host: "alpha", Class: ClassDiskFill, RestoreDueAt: now.Add(10 * time.Minute)},
		{ID: 2, Host: "bravo", Class: ClassDeviceDown, RestoreDueAt: now.Add(10 * time.Minute)},
	}

	// A brand-new planner (Injected=0, no in-memory history whatsoever) sees 2 busy hosts against MaxBusy=2
	// and must throttle rather than re-fault.
	fresh := baseState()
	fresh.Outstanding = outstanding
	if d := PlanNext(fresh, rot); d.Act {
		t.Fatalf("a fresh process re-faulted %q despite the ledger showing 2 outstanding restores — this is the myspeed01 stranding (PATH B)", d.Guest.Name)
	}

	// And with headroom raised, it must still refuse the two hosts that owe restores and pick only the third.
	fresh.Limits.MaxBusy = 3
	d := PlanNext(fresh, rot)
	if !d.Act {
		t.Fatalf("want an injection into the one free guest, got refusal: %s", d.Reason)
	}
	if d.Guest.Name != "charlie" {
		t.Fatalf("want the only guest owing nothing (charlie), got %q", d.Guest.Name)
	}
}

// ---------------------------------------------------------------------------------------------------
// Fail-closed refusals
// ---------------------------------------------------------------------------------------------------

func TestPlanNext_FailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*State)
		want   string
	}{
		{"kill-switch", func(s *State) { s.KillSwitch = true }, "kill-switch engaged"},
		{"breaker open", func(s *State) { s.BreakerOpen = true }, "TG mutation breaker is OPEN — the estate is already unhappy; not adding load"},
		{"target reached", func(s *State) { s.Limits.Target = 5; s.Injected = 5 }, "target reached (5)"},
		{"empty pool", func(s *State) { s.Pool = nil }, "empty pool"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := baseState()
			tc.mutate(&st)
			d := PlanNext(st, rot)
			if d.Act {
				t.Fatalf("want refusal, got an injection into %q", d.Guest.Name)
			}
			if d.Reason != tc.want {
				t.Fatalf("reason = %q, want %q", d.Reason, tc.want)
			}
		})
	}
}

// A short or absent cluster snapshot means we cannot distinguish running from stopped. Faulting then risks
// stopping a guest an operator is already working on. Refusing is always safe.
func TestPlanNext_RefusesToFaultBlindOnAShortSnapshot(t *testing.T) {
	st := baseState()
	st.Status = running("101") // 1 entry for a 3-guest pool
	d := PlanNext(st, rot)
	if d.Act {
		t.Fatal("faulted on a short snapshot — the engine must never fault blind")
	}
	if got := d.Reason; got == "" || got[:24] != "cluster snapshot too sho" {
		t.Fatalf("want a snapshot-too-short reason, got %q", got)
	}
}

// A pool guest absent from TG's actuation allowlist can never be healed by TG, so every fault there is a
// guaranteed A1/A3 miss that looks like a TG failure. It must be skipped, not drilled. (searxng01, live.)
func TestPlanNext_SkipsGuestsOutsideTheActuationAllowlist(t *testing.T) {
	st := baseState()
	st.Allowlist = allow("charlie") // alpha + bravo are drilled but NOT actuatable
	for i := range 12 {
		st.Injected = i
		d := PlanNext(st, rot)
		if d.Act && d.Guest.Name != "charlie" {
			t.Fatalf("injected=%d: selected non-allowlisted %q — every fault there is an automatic miss", i, d.Guest.Name)
		}
	}
}

func TestPlanNext_SkipsGuestsNotRunning(t *testing.T) {
	st := baseState()
	st.Status = map[string]string{"101": "stopped", "102": "stopped", "103": "running"}
	d := PlanNext(st, rot)
	if !d.Act {
		t.Fatalf("want the one running guest, got refusal: %s", d.Reason)
	}
	if d.Guest.Name != "charlie" {
		t.Fatalf("selected %q which is not running", d.Guest.Name)
	}
}

// The outage budget is counted from the LIVE snapshot, so a guest an operator stopped by hand still counts.
// When it is spent, the engine switches to a non-stopping class rather than idling.
func TestPlanNext_SwitchesAwayFromDeviceDownWhenTheOutageBudgetIsSpent(t *testing.T) {
	st := baseState()
	st.Limits.MaxDown = 1
	st.Limits.MaxBusy = 3
	st.Status = map[string]string{"101": "stopped", "102": "running", "103": "running"}
	st.Injected = 0 // rotation[0] is device-down

	d := PlanNext(st, rot)
	if !d.Act {
		t.Fatalf("want an injection, got refusal: %s", d.Reason)
	}
	if d.Class == ClassDeviceDown {
		t.Fatal("kept device-down with the outage budget spent — must switch to a non-stopping class")
	}
	if d.Class.OwesRestore() && d.Class != ClassDiskFill {
		t.Fatalf("unexpected substitute class %q", d.Class)
	}
}

func TestPlanNext_ThrottlesOnMaxBusy(t *testing.T) {
	st := baseState()
	st.Limits.MaxBusy = 1
	st.Outstanding = []Outstanding{{ID: 1, Host: "alpha", Class: ClassDiskFill, RestoreDueAt: now.Add(time.Minute)}}
	if d := PlanNext(st, rot); d.Act {
		t.Fatalf("want throttle at MaxBusy=1 with 1 outstanding, got injection into %q", d.Guest.Name)
	}
}

// A host whose repair FAILED stays quarantined rather than being re-faulted — a host we could not fix is the
// last one that should receive more load.
func TestPlanNext_QuarantinesAHostWhoseRepairFailed(t *testing.T) {
	st := baseState()
	st.Limits.MaxBusy = 3
	st.Outstanding = []Outstanding{{
		ID: 1, Host: "alpha", Class: ClassDiskFill,
		RestoreDueAt: now.Add(-2 * time.Hour), Failed: true,
	}}
	for i := range 12 {
		st.Injected = i
		if d := PlanNext(st, rot); d.Act && d.Guest.Name == "alpha" {
			t.Fatal("re-faulted a host whose repair failed — it must stay quarantined for an operator")
		}
	}
}

// Every refusal must say why: an unattended engine that goes quiet is indistinguishable from a stuck one.
func TestPlanNext_EveryRefusalCarriesAReason(t *testing.T) {
	st := baseState()
	st.Allowlist = allow() // nothing allowlisted ⇒ no eligible guest
	d := PlanNext(st, rot)
	if d.Act {
		t.Fatal("want refusal")
	}
	if d.Reason == "" {
		t.Fatal("refusal with no reason — a silent no-op is indistinguishable from a stuck engine")
	}
}

// ---------------------------------------------------------------------------------------------------
// Reconcile
// ---------------------------------------------------------------------------------------------------

func TestReconcile_ReturnsDueAndOverdueSoonestFirst(t *testing.T) {
	out := []Outstanding{
		{ID: 1, Host: "alpha", Class: ClassDiskFill, RestoreDueAt: now.Add(-10 * time.Minute)},
		{ID: 2, Host: "bravo", Class: ClassDeviceDown, RestoreDueAt: now.Add(-2 * time.Hour)},
		{ID: 3, Host: "charlie", Class: ClassDiskFill, RestoreDueAt: now.Add(30 * time.Minute)}, // not yet due
	}
	got := Reconcile(now, out)
	if len(got) != 2 {
		t.Fatalf("want 2 due repairs, got %d", len(got))
	}
	if got[0].Fault.ID != 2 {
		t.Fatalf("want the most overdue (id=2) first, got id=%d", got[0].Fault.ID)
	}
	if got[0].Overdue != 2*time.Hour {
		t.Fatalf("overdue = %v, want 2h — the lateness of a repair is the safety signal", got[0].Overdue)
	}
}

// A self-releasing class owes nothing, so it must never appear as a repair — otherwise the reconciler would
// endlessly "repair" memory hogs that already released themselves.
func TestReconcile_IgnoresClassesThatOweNoRestore(t *testing.T) {
	out := []Outstanding{{ID: 1, Host: "alpha", Class: ClassMemPressure, RestoreDueAt: now.Add(-time.Hour)}}
	if got := Reconcile(now, out); len(got) != 0 {
		t.Fatalf("want no repairs for a self-releasing class, got %d", len(got))
	}
}

// A previously-failed repair is retried, not abandoned. Repairs are idempotent by construction (removing an
// absent file, starting a running guest), so retrying is safe — and giving up silently is how a guest stays
// at 97% disk.
func TestReconcile_RetriesAFailedRepair(t *testing.T) {
	out := []Outstanding{{
		ID: 1, Host: "alpha", Class: ClassDiskFill,
		RestoreDueAt: now.Add(-time.Hour), Failed: true,
	}}
	if got := Reconcile(now, out); len(got) != 1 {
		t.Fatalf("want a failed repair retried, got %d actions", len(got))
	}
}

// The crash-survival property, end to end: a brand-new process holding NOTHING but the ledger must repair
// everything a dead predecessor left behind.
func TestReconcile_AFreshProcessRepairsWhatACrashedOneLeftBehind(t *testing.T) {
	stranded := []Outstanding{
		{ID: 7, Host: "docuseal01", Class: ClassDiskFill, FaultRef: "/var/tmp/tg-tier1-fill.img", RestoreDueAt: now.Add(-77 * time.Minute)},
		{ID: 8, Host: "myspeed01", Class: ClassDiskFill, FaultRef: "/var/tmp/tg-tier1-fill.img", RestoreDueAt: now.Add(-87 * time.Minute)},
	}
	got := Reconcile(now, stranded)
	if len(got) != 2 {
		t.Fatalf("a fresh process must repair both stranded guests, got %d", len(got))
	}
	for _, a := range got {
		if a.Overdue <= 0 {
			t.Fatalf("%s: overdue must be positive so lateness is visible", a.Fault.Host)
		}
		if a.Fault.FaultRef == "" {
			t.Fatalf("%s: repair needs the fault handle, and a fresh process can only get it from the ledger", a.Fault.Host)
		}
	}
}

// ---------------------------------------------------------------------------------------------------
// Boot-time pool assertion
// ---------------------------------------------------------------------------------------------------

func TestPoolMismatch_ReportsBothDirections(t *testing.T) {
	// The live 2026-07-26 configuration: searxng01 + servarr01 were drilled but not actuatable, while
	// habitica01 + postiz01 were actuatable but never drilled.
	p := []PoolGuest{{VMID: "1", Name: "searxng01"}, {VMID: "2", Name: "servarr01"}, {VMID: "3", Name: "mealie01"}}
	a := allow("mealie01", "habitica01", "postiz01")

	notAllowlisted, notDrilled := PoolMismatch(p, a)
	if len(notAllowlisted) != 2 || notAllowlisted[0] != "searxng01" || notAllowlisted[1] != "servarr01" {
		t.Fatalf("notAllowlisted = %v, want [searxng01 servarr01] — faults there are automatic A1/A3 misses", notAllowlisted)
	}
	if len(notDrilled) != 2 || notDrilled[0] != "habitica01" || notDrilled[1] != "postiz01" {
		t.Fatalf("notDrilled = %v, want [habitica01 postiz01]", notDrilled)
	}
}

func TestPoolMismatch_CleanConfigurationReportsNothing(t *testing.T) {
	p := pool3()
	notAllowlisted, notDrilled := PoolMismatch(p, allow("alpha", "bravo", "charlie"))
	if len(notAllowlisted) != 0 || len(notDrilled) != 0 {
		t.Fatalf("a matched pool must report no mismatch, got %v / %v", notAllowlisted, notDrilled)
	}
}

// REGRESSION (live, 2026-07-26 id=68): when the outage budget was spent, the planner substituted a
// HARDCODED class rather than one from the configured rotation, emitting mem-pressure even though the
// operator had deliberately excluded it (its detection rate is 1/14 on this estate, so injecting it
// manufactures A1 misses). The fail-closed injector caught it so nothing was broken — but a planner must
// never widen its own mandate.
func TestPlanNext_SubstitutesOnlyFromTheConfiguredRotation(t *testing.T) {
	st := baseState()
	st.Limits.MaxDown = 0 // budget spent immediately
	st.Limits.MaxBusy = 3
	only := []Class{ClassDeviceDown, ClassDiskFill} // mem-pressure deliberately EXCLUDED

	for i := range 20 {
		st.Injected = i
		d := PlanNext(st, only)
		if d.Act && d.Class == ClassMemPressure {
			t.Fatalf("injected=%d: emitted mem-pressure, which the operator excluded from the rotation", i)
		}
		if d.Act && d.Class == ClassDeviceDown {
			t.Fatalf("injected=%d: kept device-down with the outage budget spent", i)
		}
	}
}

// A rotation of ONLY device-down, with the budget spent, must refuse with a reason rather than invent a class.
func TestPlanNext_RefusesWhenTheRotationHasNoNonStoppingClass(t *testing.T) {
	st := baseState()
	st.Limits.MaxDown = 0
	st.Limits.MaxBusy = 3
	d := PlanNext(st, []Class{ClassDeviceDown})
	if d.Act {
		t.Fatalf("invented class %q when the rotation offers no alternative", d.Class)
	}
	if !strings.Contains(d.Reason, "no non-stopping class") {
		t.Fatalf("reason = %q, want an explanation naming the exhausted rotation", d.Reason)
	}
}
