package faultinjector

import (
	"testing"
	"time"
)

// DETECTION IS A STATE TRANSITION, NOT A STATE.
//
// Measured live 2026-07-28: two service-down faults landed on dc1openwebui01 two minutes apart (the first
// restored 04:15:22, the second injected 04:17:04) while the monitoring check polls every 5 minutes. The check
// never observed the recovered state, so it never flipped 2 -> 0 -> 2, the alert never cleared, it never
// re-raised, and TG received NOTHING for the second fault.
//
// The damage is not just a missed heal. injected_fault recorded TWO faults while TG had ONE opportunity, so a
// detection rate computed as detections/injections scores the second as a MISS THAT WAS NEVER DETECTABLE —
// an instrument artefact read as a TG failure, which is the same error class that once buried the diagnosis
// number by two sign errors.
//
// INVARIANT 2 already refuses to STACK faults, protecting the ESTATE. This guard extends the same idea past
// the restore, protecting the MEASUREMENT.

func settleState(now time.Time, window time.Duration, restored map[string]time.Time) State {
	return State{
		Now:       now,
		Pool:      []PoolGuest{{VMID: "100", Name: "guest-a", Node: "node-a", Unit: "some.service", Container: "some-container"}},
		Allowlist: map[string]bool{"guest-a": true},
		Status:    map[string]string{"100": "running"},
		Settling:  restored,
		Limits:    Limits{MaxDown: 3, MaxBusy: 5, RestoreAfter: 20 * time.Minute, SettleWindow: window},
	}
}

// TestATargetRestoredTooRecentlyIsNotRefaulted is the live incident as an oracle.
func TestATargetRestoredTooRecentlyIsNotRefaulted(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	st := settleState(now, 10*time.Minute, map[string]time.Time{
		settleKey("guest-a", ClassServiceDown): now.Add(-2 * time.Minute), // restored 2 min ago
	})
	d := PlanNext(st, []Class{ClassServiceDown})
	if d.Act {
		t.Errorf("re-faulted %s with %s 2 minutes after its restore — the monitoring check has not polled the "+
			"recovered state, so this fault raises no alert and is UNDETECTABLE, yet it still lands in the "+
			"denominator", d.Guest.Name, d.Class)
	}
}

// TestATargetSettledLongEnoughIsFaultedAgain — the guard must not become a permanent ban. Once the recovery
// has been observable for longer than the window, the target is eligible again.
func TestATargetSettledLongEnoughIsFaultedAgain(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	st := settleState(now, 10*time.Minute, map[string]time.Time{
		settleKey("guest-a", ClassServiceDown): now.Add(-11 * time.Minute),
	})
	if d := PlanNext(st, []Class{ClassServiceDown}); !d.Act {
		t.Errorf("a target restored 11 minutes ago (window 10m) was still refused: %q — the guard must expire, "+
			"or the pool drains to nothing", d.Reason)
	}
}

// TestTheSettleGuardIsPerClass — a service-down restore says nothing about whether a DEVICE-down would be
// detectable, because they raise different checks. Blocking across classes would idle the injector for no
// measurement benefit.
func TestTheSettleGuardIsPerClass(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	st := settleState(now, 10*time.Minute, map[string]time.Time{
		settleKey("guest-a", ClassServiceDown): now.Add(-1 * time.Minute),
	})
	if d := PlanNext(st, []Class{ClassDeviceDown}); !d.Act {
		t.Errorf("a recent service-down restore blocked a DEVICE-down fault (%q) — different classes raise "+
			"different checks, so one says nothing about the other's detectability", d.Reason)
	}
}

// TestAnUnknownTargetIsNotBlocked — a host that has never carried this class has nothing to wait for. The
// guard must key on evidence of a recent restore, never on its absence.
func TestAnUnknownTargetIsNotBlocked(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	for name, restored := range map[string]map[string]time.Time{
		"nil map":     nil,
		"empty map":   {},
		"other host":  {settleKey("guest-z", ClassServiceDown): now.Add(-time.Minute)},
		"other class": {settleKey("guest-a", ClassDiskFill): now.Add(-time.Minute)},
		"zero time":   {settleKey("guest-a", ClassServiceDown): {}},
	} {
		if d := PlanNext(settleState(now, 10*time.Minute, restored), []Class{ClassServiceDown}); !d.Act {
			t.Errorf("%s: a target with no recent restore of this class was refused (%q)", name, d.Reason)
		}
	}
}

// TestAZeroSettleWindowDisablesTheGuard — the operator must be able to switch it off, and switching it off
// must be total rather than partial.
func TestAZeroSettleWindowDisablesTheGuard(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	// The restore time is in the FUTURE, which is the only input that can tell "the guard is off" apart from
	// "the guard is on but the arithmetic happens to say zero". Clock skew between the DB and the engine makes
	// a future restored_at genuinely reachable, and it must not block forever either.
	st := settleState(now, 0, map[string]time.Time{
		settleKey("guest-a", ClassServiceDown): now.Add(time.Minute),
	})
	if d := PlanNext(st, []Class{ClassServiceDown}); !d.Act {
		t.Errorf("SettleWindow=0 must disable the guard entirely, got refusal %q", d.Reason)
	}
}

// TestTheSettleGuardDoesNotOverrideTheStackingGuard — INVARIANT 2 still owns the estate-safety decision. A
// host that OWES a restore must stay ineligible regardless of what the settle map says.
func TestTheSettleGuardDoesNotOverrideTheStackingGuard(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	st := settleState(now, 10*time.Minute, map[string]time.Time{})
	st.Outstanding = []Outstanding{{ID: 1, Host: "guest-a", Class: ClassServiceDown, RestoreDueAt: now.Add(time.Minute)}}
	if d := PlanNext(st, []Class{ClassServiceDown}); d.Act {
		t.Error("a host that still OWES a restore was faulted again — INVARIANT 2 must not be weakened by the " +
			"settle guard")
	}
}

// TestAStoreFailureRefusesToInjectRatherThanRiskAnUndetectableFault is the ENGINE-level half. The planner
// cannot see a database error; only InjectOnce can. An empty settle map reads as "every target is settled",
// which is the fail-OPEN direction, so a read failure must stop the injection rather than proceed blind.
func TestAStoreFailureRefusesToInjectRatherThanRiskAnUndetectableFault(t *testing.T) {
	st := newMemStore()
	st.restoresErr = errRestores
	e := &Engine{
		Exec: &scriptedRunner{}, Store: st, Log: func(string, ...any) {}, SnapNode: "node-a",
		Pool:      []PoolGuest{{VMID: "100", Name: "guest-a", Node: "node-a", Unit: "some.service"}},
		Allowlist: map[string]bool{"guest-a": true},
		Limits:    Limits{MaxDown: 3, MaxBusy: 5, RestoreAfter: 20 * time.Minute, SettleWindow: 10 * time.Minute},
		Rotation:  []Class{ClassServiceDown},
	}
	if e.InjectOnce(t.Context(), 0, 0) {
		t.Error("injected while the recent-restore read was FAILING — an empty settle map reads as 'everything " +
			"is settled', so proceeding risks a fault nothing can detect while still counting it")
	}
	if st.recorded {
		t.Error("an obligation was recorded despite the settle read failing")
	}
}
