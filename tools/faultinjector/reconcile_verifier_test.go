package faultinjector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// THE REPAIR'S EXIT CODE IS NOT THE ANSWER; REAL STATE IS.
//
// Measured live on 2026-07-28: `pct start` on a guest that is ALREADY RUNNING exits 255 with "CT <id> already
// running". The reconciler read 255 as "ssh transport failure, retry", so an obligation whose estate was
// already in the desired state could never discharge. It retried forever and held its host permanently busy,
// until one stranded row plus four legitimate faults hit the 5/5 throttle and stalled the entire campaign —
// blocking a fault class that had just been armed from ever running once.
//
// 255 is overloaded: ssh uses it for its own transport errors AND passes through whatever the remote command
// exited with. No exit code can distinguish "already fine" from "never ran". Only a read of real state can,
// which is what the verifier does.

// scriptedRunner answers the repair and the verify differently, so a test can express "the repair reported X
// and the estate is actually Y" — the exact situation the reconciler has to resolve.
type scriptedRunner struct {
	repairOut  string
	repairCode int
	repairErr  error
	// verifyRunning is what a status read reports. The verifiers key on this.
	verifyRunning bool
	verifyErr     error
	verifyCalls   int
	repairCalls   int
}

func (r *scriptedRunner) Run(_ context.Context, _ string, argv []string) (string, int, error) {
	if len(argv) >= 2 && (argv[1] == "status" || argv[1] == "inspect" || argv[1] == "is-active") {
		r.verifyCalls++
		if r.verifyErr != nil {
			return "", -1, r.verifyErr
		}
		if r.verifyRunning {
			return "status: running\nrunning\nactive\n", 0, nil
		}
		return "status: stopped\nexited\ninactive\n", 0, nil
	}
	// A valid cluster snapshot, so InjectOnce reaches the decisions under test instead of bailing early with
	// "cluster snapshot too short". A fake that stops the code short of the property makes the test vacuous —
	// which is exactly how a mutation control on the settle-read failed to fire.
	if len(argv) > 1 && argv[0] == "pvesh" {
		return `[{"vmid":100,"status":"running"}]`, 0, nil
	}
	// The name-assert: InjectOnce refuses to act on a vmid whose hostname it cannot read ("refusing to act
	// blind"), the guard that exists because a wrong-guest stop happened once. A fake that cannot answer it
	// stops every InjectOnce test before the logic under test — which is how a mutation control on the
	// settle-read read GREEN twice.
	if len(argv) > 1 && argv[0] == "pct" && argv[1] == "config" {
		return "hostname: guest-a\n", 0, nil
	}
	r.repairCalls++
	return r.repairOut, r.repairCode, r.repairErr
}

// memLedger records which decision the reconciler took. It carries no row marshalling on purpose — see the
// note on LedgerStore.
type memLedger struct {
	restoresErr error
	recorded    bool
	rows        []Outstanding
	state       map[int64]string
	reason      map[int64]string
}

func newMemStore(rows ...Outstanding) *memLedger {
	return &memLedger{rows: rows, state: map[int64]string{}, reason: map[int64]string{}}
}

// Outstanding mirrors the REAL query, which selects only 'pending'/'failed' — a closed obligation is gone.
// The fake used to return every seeded row forever, so Drain could never observe "all discharged" and always
// exited via its DEADLINE instead. That made a 2-second drain test take 15 seconds, and worse, it would have
// hidden a drain that never terminates: the loop's success path was untested by construction.
func (m *memLedger) Outstanding(context.Context) ([]Outstanding, error) {
	var out []Outstanding
	for _, r := range m.rows {
		if m.state[r.ID] == "restored" {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
func (m *memLedger) RecordInjection(context.Context, string, Class, string, string, time.Duration, string) (int64, error) {
	m.recorded = true
	return 1, nil
}
func (m *memLedger) MarkRestored(_ context.Context, id int64) error {
	m.state[id] = "restored"
	return nil
}
func (m *memLedger) MarkRestoreFailed(_ context.Context, id int64, reason string) error {
	m.state[id] = "failed"
	m.reason[id] = reason
	return nil
}

var errRestores = errors.New("recent-restore read failed")

func (m *memLedger) RecentRestores(context.Context, time.Duration) (map[string]time.Time, error) {
	if m.restoresErr != nil {
		return nil, m.restoresErr
	}
	return map[string]time.Time{}, nil
}
func (m *memLedger) BreakerOpen(context.Context) (bool, error) { return false, nil }
func (m *memLedger) KillSwitchEngaged(context.Context) bool    { return false }

func reconcileOnce(t *testing.T, run *scriptedRunner, st *memLedger) int {
	t.Helper()
	e := &Engine{Exec: run, Store: st, Log: func(string, ...any) {}}
	return e.ReconcileOnce(context.Background())
}

// TestAnAlreadyInDesiredStateRepairDischarges is the live bug, as an oracle. The repair exits 255 because the
// guest is already running; the verifier reads "running"; the obligation MUST close. Before this fix it
// retried forever and the host stayed busy for good.
func TestAnAlreadyInDesiredStateRepairDischarges(t *testing.T) {
	st := newMemStore(Outstanding{ID: 1, Host: "guest-a", Class: ClassDeviceDown, Node: "node-a",
		FaultRef: "101", RestoreDueAt: time.Now().Add(-time.Minute)})
	run := &scriptedRunner{repairOut: "CT 101 already running", repairCode: 255, verifyRunning: true}

	if closed := reconcileOnce(t, run, st); closed != 1 {
		t.Fatalf("closed %d obligations, want 1 — a repair that exits 255 because the estate is ALREADY in the "+
			"desired state must discharge, or the row strands and holds its host busy forever", closed)
	}
	if st.state[1] != "restored" {
		t.Errorf("obligation is %q, want restored", st.state[1])
	}
	if run.verifyCalls == 0 {
		t.Error("the verifier was never consulted — the exit code decided, which is the whole defect")
	}
}

// TestARepairThatNeverReachedTheHostDoesNotDischarge is the property the old short-circuit was protecting, and
// it must survive: an unreachable host fails the VERIFIER's read too, so nothing closes.
func TestARepairThatNeverReachedTheHostDoesNotDischarge(t *testing.T) {
	st := newMemStore(Outstanding{ID: 1, Host: "guest-a", Class: ClassDeviceDown, Node: "node-a",
		FaultRef: "101", RestoreDueAt: time.Now().Add(-time.Minute)})
	run := &scriptedRunner{repairOut: "ssh: connect to host node-a port 22: No route to host",
		repairCode: 255, verifyErr: context.DeadlineExceeded}

	if closed := reconcileOnce(t, run, st); closed != 0 {
		t.Fatalf("closed %d obligations, want 0 — a repair that never reached the host must NOT pass as a "+
			"successful repair", closed)
	}
	if st.state[1] == "restored" {
		t.Error("an unverifiable repair closed its obligation — this is the fail-open the exit-code check existed to stop")
	}
}

// TestARepairThatRanButDidNotFixItDoesNotDischarge — exit 0 is not proof either. The fault is still present,
// so the host stays quarantined with the reason recorded.
func TestARepairThatRanButDidNotFixItDoesNotDischarge(t *testing.T) {
	st := newMemStore(Outstanding{ID: 1, Host: "guest-a", Class: ClassDeviceDown, Node: "node-a",
		FaultRef: "101", RestoreDueAt: time.Now().Add(-time.Minute)})
	run := &scriptedRunner{repairCode: 0, verifyRunning: false}

	if closed := reconcileOnce(t, run, st); closed != 0 {
		t.Fatal("a repair that exited 0 while the fault is still present must not discharge")
	}
	if st.state[1] != "failed" {
		t.Errorf("obligation is %q, want failed (quarantined with a reason)", st.state[1])
	}
	if !strings.Contains(st.reason[1], "still present") {
		t.Errorf("quarantine reason %q does not say what was wrong", st.reason[1])
	}
}

// TestTheVerifierIsAlwaysConsulted is the structural version of the above three: whatever the repair returns,
// real state gets read. A code path that skips the verifier is a code path that decides on an exit code.
func TestTheVerifierIsAlwaysConsulted(t *testing.T) {
	for _, code := range []int{0, 1, 42, 255, -1} {
		st := newMemStore(Outstanding{ID: 1, Host: "guest-a", Class: ClassDeviceDown, Node: "node-a",
			FaultRef: "101", RestoreDueAt: time.Now().Add(-time.Minute)})
		run := &scriptedRunner{repairCode: code, verifyRunning: true}
		reconcileOnce(t, run, st)
		if run.verifyCalls == 0 {
			t.Errorf("repair exit %d skipped the verifier — no exit code can distinguish 'already fine' from "+
				"'never ran', so none of them may decide", code)
		}
	}
}
