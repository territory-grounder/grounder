package observeprobe

import (
	"reflect"
	"testing"

	"github.com/territory-grounder/grounder/tools/faultinjector"
)

// a running-everything snapshot for the given guests.
func runningSnap(guests ...faultinjector.PoolGuest) map[string]string {
	s := map[string]string{}
	for _, g := range guests {
		s[g.VMID] = "running"
	}
	return s
}

func gp(name, vmid string) faultinjector.PoolGuest {
	return faultinjector.PoolGuest{VMID: vmid, Name: name, Node: "nodeX"}
}

// baseState is a healthy, probeable single-candidate state; individual tests mutate one axis.
func baseState() ProbeState {
	g := gp("gp-01", "101")
	return ProbeState{
		Unobservable: []string{"gp-01"},
		Pool:         []faultinjector.PoolGuest{g},
		Allowlist:    map[string]bool{"gp-01": true},
		Status:       runningSnap(g),
		Classes:      []faultinjector.Class{faultinjector.ClassDeviceDown},
	}
}

// A census-unobservable host that IS a sanctioned, allowlisted, running, free guinea-pig is selected, with a
// class it can host.
func TestPlanProbe_SelectsValidGuineaPig(t *testing.T) {
	d := PlanProbe(baseState())
	if !d.Act {
		t.Fatalf("Act=false, want a probe of gp-01: %s", d.Reason)
	}
	if d.Guest.Name != "gp-01" || d.Class != faultinjector.ClassDeviceDown {
		t.Fatalf("selected %s/%s, want gp-01/device-down", d.Guest.Name, d.Class)
	}
	if len(d.Uncoverable) != 0 {
		t.Fatalf("Uncoverable=%v, want empty — gp-01 is coverable", d.Uncoverable)
	}
}

// REFUSES WHEN THE POOL DISAGREES. A census-unobservable host that is NOT in the guinea-pig pool cannot be
// probed — it is reported as uncoverable and the planner refuses rather than guessing a victim.
func TestPlanProbe_RefusesWhenPoolDisagrees(t *testing.T) {
	st := baseState()
	st.Unobservable = []string{"stranger-99"} // real blind spot, but not a guinea-pig
	d := PlanProbe(st)
	if d.Act {
		t.Fatalf("Act=true for a non-pool host — the injector must never probe a host outside the guinea-pig pool")
	}
	if !reflect.DeepEqual(d.Uncoverable, []string{"stranger-99"}) {
		t.Fatalf("Uncoverable=%v, want [stranger-99] surfaced explicitly (no silent sampling caps)", d.Uncoverable)
	}
}

// A guinea-pig that TG may NOT actuate (absent from the allowlist) is uncoverable — probing a host TG cannot
// heal would risk stranding it.
func TestPlanProbe_NotAllowlistedIsUncoverable(t *testing.T) {
	st := baseState()
	st.Allowlist = map[string]bool{} // gp-01 in the pool but TG may not actuate it
	d := PlanProbe(st)
	if d.Act {
		t.Fatal("Act=true for a non-allowlisted guest")
	}
	if !reflect.DeepEqual(d.Uncoverable, []string{"gp-01"}) {
		t.Fatalf("Uncoverable=%v, want [gp-01]", d.Uncoverable)
	}
}

// NO-STACKING. A host that already owes a restore (faultinjector's ledger) is never probed — stacking a probe
// on a pending fault is how an in-guest cleanup is destroyed.
func TestPlanProbe_NoStackingOnBusyHost(t *testing.T) {
	st := baseState()
	st.Outstanding = []faultinjector.Outstanding{{Host: "gp-01", Class: faultinjector.ClassDiskFill}}
	if d := PlanProbe(st); d.Act {
		t.Fatalf("Act=true on gp-01 while it owes a restore — INVARIANT 2 (no-stacking) violated")
	}
}

// A host already carrying a pending probe or a terminal verdict is not re-probed.
func TestPlanProbe_AlreadyProbedIsSkipped(t *testing.T) {
	st := baseState()
	st.AlreadyProbed = map[string]bool{"gp-01": true}
	if d := PlanProbe(st); d.Act {
		t.Fatalf("Act=true on an already-probed host")
	}
}

// A stopped guest cannot be perturbed into an alert, so it is skipped this cycle (probe it once it is back).
func TestPlanProbe_StoppedGuestSkipped(t *testing.T) {
	st := baseState()
	st.Status = map[string]string{"101": "stopped"}
	if d := PlanProbe(st); d.Act {
		t.Fatalf("Act=true on a stopped guest")
	}
}

// FAIL-CLOSED on every safety signal.
func TestPlanProbe_FailsClosed(t *testing.T) {
	cases := map[string]func(*ProbeState){
		"kill-switch":  func(s *ProbeState) { s.KillSwitch = true },
		"breaker-open": func(s *ProbeState) { s.BreakerOpen = true },
		"empty-pool":   func(s *ProbeState) { s.Pool = nil },
		"no-classes":   func(s *ProbeState) { s.Classes = nil },
		"no-targets":   func(s *ProbeState) { s.Unobservable = nil },
		// snapshot too short to even describe the pool — never probe blind
		"blind-snapshot": func(s *ProbeState) { s.Status = map[string]string{} },
	}
	for name, mut := range cases {
		st := baseState()
		mut(&st)
		if d := PlanProbe(st); d.Act {
			t.Errorf("%s: Act=true, want a fail-closed refusal", name)
		}
	}
}

// Class selection walks the operator's preference order and picks the first the guest can host; a guest that
// hosts NONE of the configured classes is skipped rather than probed with a class it cannot satisfy.
func TestPlanProbe_ClassSelectionRespectsGuestSupport(t *testing.T) {
	// gp-01 declares no unit, so service-down is unhostable; the rotation falls through to device-down.
	st := baseState()
	st.Classes = []faultinjector.Class{faultinjector.ClassServiceDown, faultinjector.ClassDeviceDown}
	d := PlanProbe(st)
	if !d.Act || d.Class != faultinjector.ClassDeviceDown {
		t.Fatalf("selected %v/%q, want gp-01/device-down (service-down unhostable, falls through)", d.Act, d.Class)
	}

	// With ONLY service-down configured and no unit declared, the guest hosts no configured class → refuse.
	st.Classes = []faultinjector.Class{faultinjector.ClassServiceDown}
	if d := PlanProbe(st); d.Act {
		t.Fatalf("Act=true with only service-down configured and no unit declared — nothing to host")
	}
}

// A service-down probe IS planned when the guest declares a unit (config-not-code eligibility, shared with the
// campaign planner via faultinjector.GuestSupportsClass).
func TestPlanProbe_ServiceDownWhenUnitDeclared(t *testing.T) {
	g := faultinjector.PoolGuest{VMID: "202", Name: "gp-svc", Node: "nodeX", Unit: "app.service"}
	st := ProbeState{
		Unobservable: []string{"gp-svc"},
		Pool:         []faultinjector.PoolGuest{g},
		Allowlist:    map[string]bool{"gp-svc": true},
		Status:       runningSnap(g),
		Classes:      []faultinjector.Class{faultinjector.ClassServiceDown},
	}
	d := PlanProbe(st)
	if !d.Act || d.Class != faultinjector.ClassServiceDown {
		t.Fatalf("selected %v/%q, want gp-svc/service-down", d.Act, d.Class)
	}
}

// The uncoverable remainder is reported EVEN ON A CYCLE THAT ALSO PROBES — one host is coverable and picked,
// another is a non-pool blind spot and named.
func TestPlanProbe_UncoverableSurfacedAlongsideAProbe(t *testing.T) {
	st := baseState()
	st.Unobservable = []string{"gp-01", "stranger-99"}
	d := PlanProbe(st)
	if !d.Act || d.Guest.Name != "gp-01" {
		t.Fatalf("want a probe of gp-01, got Act=%v guest=%s", d.Act, d.Guest.Name)
	}
	if !reflect.DeepEqual(d.Uncoverable, []string{"stranger-99"}) {
		t.Fatalf("Uncoverable=%v, want [stranger-99] surfaced alongside the probe", d.Uncoverable)
	}
}
