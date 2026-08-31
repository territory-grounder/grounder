package faultinjector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// InjectGuest is the effect sequence extracted from InjectOnce (TG-180) so observeprobe.Orchestrator can drive
// injection directly, reusing the record-BEFORE-effect safety ordering. These oracles pin that ordering and
// the three failure lanes — a pre-effect refusal CLOSES the obligation, an ambiguous failure QUARANTINES it,
// and a failed name-assert records nothing at all.

// eventLog is a shared, ordered sink so one test can assert that the obligation was recorded BEFORE the remote
// effect ran — the safety property that stops a performed-but-unrecorded fault stranding a guest.
type eventLog struct{ ev []string }

func (l *eventLog) add(s string) { l.ev = append(l.ev, s) }

func indexOf(evs []string, want string) int {
	for i, e := range evs {
		if e == want {
			return i
		}
	}
	return -1
}

func anyEventHasPrefix(evs []string, p string) bool {
	for _, e := range evs {
		if strings.HasPrefix(e, p) {
			return true
		}
	}
	return false
}

// orderRunner answers the name-assert (`pct config`) with a scriptable hostname and records every OTHER command
// it is asked to run into the shared log, so the effect's position relative to the record is observable. A
// mismatching configName (or non-zero configCode) fails the name-assert exactly as a stale pool entry would.
type orderRunner struct {
	log        *eventLog
	configName string
	configCode int
	code       map[string]int
	err        map[string]error
}

func (r *orderRunner) Run(_ context.Context, _ string, argv []string) (string, int, error) {
	if len(argv) >= 2 && argv[0] == "pct" && argv[1] == "config" {
		r.log.add("assert")
		return "hostname: " + r.configName + "\n", r.configCode, nil
	}
	r.log.add(strings.Join(argv, " "))
	verb := ""
	if len(argv) > 0 {
		verb = argv[0]
	}
	return "", r.code[verb], r.err[verb]
}

// orderStore is a LedgerStore spy that records the obligation lifecycle (and the order RecordInjection happened
// in, via the shared log). It carries no row marshalling — the same deliberately-narrow shape as memLedger.
type orderStore struct {
	log       *eventLog
	recorded  bool
	restored  []int64
	failed    []int64
	reasons   []string
	recordErr error
}

const orderStoreID int64 = 7

func (s *orderStore) RecordInjection(context.Context, string, Class, string, string, time.Duration, string) (int64, error) {
	if s.recordErr != nil {
		return 0, s.recordErr
	}
	s.recorded = true
	s.log.add("record")
	return orderStoreID, nil
}
func (s *orderStore) MarkRestored(_ context.Context, id int64) error {
	s.restored = append(s.restored, id)
	return nil
}
func (s *orderStore) MarkRestoreFailed(_ context.Context, id int64, reason string) error {
	s.failed = append(s.failed, id)
	s.reasons = append(s.reasons, reason)
	return nil
}
func (s *orderStore) Outstanding(context.Context) ([]Outstanding, error) { return nil, nil }
func (s *orderStore) RecentRestores(context.Context, time.Duration) (map[string]time.Time, error) {
	return nil, nil
}
func (s *orderStore) BreakerOpen(context.Context) (bool, error) { return false, nil }
func (s *orderStore) KillSwitchEngaged(context.Context) bool    { return false }

func newInjectEngine(run Runner, st LedgerStore) *Engine {
	return &Engine{Store: st, Exec: run, Limits: Limits{RestoreAfter: 30 * time.Minute},
		Note: "test", Log: func(string, ...any) {}}
}

// A failed name-assert must return (false, err) and record NOTHING — the obligation is only written once the
// guest identity is confirmed, or a stale vmid strands a phantom obligation against the wrong guest.
func TestInjectGuest_NameAssertFailDoesNotRecord(t *testing.T) {
	log := &eventLog{}
	run := &orderRunner{log: log, configName: "someone-else", code: map[string]int{}, err: map[string]error{}}
	st := &orderStore{log: log}
	e := newInjectEngine(run, st)

	ran, err := e.InjectGuest(context.Background(), PoolGuest{VMID: "100", Name: "guest-a", Node: "node-a"}, ClassDeviceDown)
	if ran {
		t.Fatal("a failed name-assert must not report the fault as injected")
	}
	if err == nil {
		t.Fatal("a failed name-assert must return an error")
	}
	if st.recorded {
		t.Fatal("RecordInjection ran after a failed name-assert — the obligation must be recorded ONLY once identity is confirmed")
	}
	for _, ev := range log.ev {
		if ev != "assert" {
			t.Fatalf("nothing but the name-assert may run when identity is unconfirmed; saw %q", ev)
		}
	}
}

// The happy path: a clean device-down records the obligation BEFORE the effect, reports ran=true, and arms the
// belt-and-braces restore timer.
func TestInjectGuest_HappyDeviceDownRecordsBeforeEffectAndArms(t *testing.T) {
	log := &eventLog{}
	run := &orderRunner{log: log, configName: "guest-a", code: map[string]int{}, err: map[string]error{}}
	st := &orderStore{log: log}
	e := newInjectEngine(run, st)

	ran, err := e.InjectGuest(context.Background(), PoolGuest{VMID: "100", Name: "guest-a", Node: "node-a"}, ClassDeviceDown)
	if err != nil || !ran {
		t.Fatalf("a clean device-down must inject; got ran=%v err=%v", ran, err)
	}
	if !st.recorded {
		t.Fatal("the obligation was never recorded")
	}
	idxRecord := indexOf(log.ev, "record")
	idxEffect := indexOf(log.ev, "pct stop 100")
	if idxRecord < 0 || idxEffect < 0 {
		t.Fatalf("expected both a record and a pct-stop effect; events=%v", log.ev)
	}
	if idxRecord > idxEffect {
		t.Fatalf("RecordInjection must PRECEDE the effect (record-before-effect is the safety ordering); events=%v", log.ev)
	}
	// device-down owes a restore, armed on the owning NODE via systemd-run.
	if !anyEventHasPrefix(log.ev, "systemd-run ") {
		t.Fatalf("the belt-and-braces restore timer was never armed; events=%v", log.ev)
	}
	if len(st.restored) != 0 || len(st.failed) != 0 {
		t.Fatalf("a clean inject must neither close nor quarantine the obligation; restored=%v failed=%v", st.restored, st.failed)
	}
}

// A provably PRE-EFFECT refusal (container-down with no declared container never reaches the host) CLOSES the
// obligation — nothing was broken, so leaving it pending would quarantine a healthy guest forever.
func TestInjectGuest_PreEffectRefusalClosesObligation(t *testing.T) {
	log := &eventLog{}
	run := &orderRunner{log: log, configName: "guest-a", code: map[string]int{}, err: map[string]error{}}
	st := &orderStore{log: log}
	e := newInjectEngine(run, st)

	// Container "" is an undeclared container: InjectContainerDown refuses BEFORE running anything.
	ran, err := e.InjectGuest(context.Background(), PoolGuest{VMID: "100", Name: "guest-a", Node: "node-a"}, ClassContainerDown)
	if ran {
		t.Fatal("a pre-effect refusal must not report a fault as injected")
	}
	if err == nil || !errors.Is(err, ErrPreEffect) {
		t.Fatalf("want an ErrPreEffect, got %v", err)
	}
	if !st.recorded {
		t.Fatal("the obligation must be recorded before the effect is attempted, even when the effect then refuses")
	}
	if len(st.restored) != 1 || st.restored[0] != orderStoreID {
		t.Fatalf("a provably pre-effect refusal must CLOSE the obligation (MarkRestored); restored=%v", st.restored)
	}
	if len(st.failed) != 0 {
		t.Fatalf("a pre-effect refusal must NOT quarantine the host; failed=%v", st.failed)
	}
	// Nothing may have run on the guest itself — only the node name-assert.
	if anyEventHasPrefix(log.ev, "docker ") {
		t.Fatalf("a refused container-down ran a docker command; events=%v", log.ev)
	}
}

// An AMBIGUOUS failure (the effect command errored non-zero — it may have committed before failing) leaves the
// obligation PENDING/quarantined for the reconciler; it must never close.
func TestInjectGuest_AmbiguousFailureQuarantinesAndDoesNotClose(t *testing.T) {
	log := &eventLog{}
	// pct stop exits 255 (transport lost) — the guest may already be stopped, so this is not provably pre-effect.
	run := &orderRunner{log: log, configName: "guest-a", code: map[string]int{"pct": 255}, err: map[string]error{}}
	st := &orderStore{log: log}
	e := newInjectEngine(run, st)

	ran, err := e.InjectGuest(context.Background(), PoolGuest{VMID: "100", Name: "guest-a", Node: "node-a"}, ClassDeviceDown)
	if ran {
		t.Fatal("an ambiguous injection failure must not report the fault as injected")
	}
	if err == nil || errors.Is(err, ErrPreEffect) {
		t.Fatalf("an ambiguous failure must be a NON-pre-effect error; got %v", err)
	}
	if len(st.restored) != 0 {
		t.Fatalf("an ambiguous failure must NOT close the obligation (the effect may have committed); restored=%v", st.restored)
	}
	if len(st.failed) != 1 || st.failed[0] != orderStoreID {
		t.Fatalf("an ambiguous failure must quarantine the host (MarkRestoreFailed); failed=%v", st.failed)
	}
	if len(st.reasons) != 1 || !strings.Contains(st.reasons[0], "ambiguous injection failure") {
		t.Fatalf("the quarantine reason must name the ambiguity; reasons=%v", st.reasons)
	}
}
