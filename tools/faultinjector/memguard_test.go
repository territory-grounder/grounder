package faultinjector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const guardFileForTests = `
# a guarded node and its targets
node node-g floor-gib=11 max-age=30s psi-some-avg60-max=1
cost big01 device-down 8
cost big01 container-down 1.5
cost small01 device-down 4
cost small01 service-down 1
`

func testGuard(t *testing.T) *MemGuard {
	t.Helper()
	g, err := ParseMemGuard(strings.NewReader(guardFileForTests))
	if err != nil {
		t.Fatalf("the test guard file must parse: %v", err)
	}
	return g
}

func guardedPool() []PoolGuest {
	return []PoolGuest{
		{VMID: "201", Name: "big01", Node: "node-g", Container: "app"},
		{VMID: "202", Name: "small01", Node: "node-g", Unit: "svc"},
		{VMID: "300", Name: "plain01", Node: "node-a"},
	}
}

func gibBytes(v float64) int64 { return int64(v * (1 << 30)) }

func reading(node string, availGiB, psi float64, at time.Time) NodeTelemetry {
	return NodeTelemetry{Node: node, AvailableBytes: gibBytes(availGiB), PSISomeAvg60: psi, ReadAt: at}
}

func procReading(availGiB, psi float64) string {
	return fmt.Sprintf("MemTotal:       131000000 kB\nMemFree:          2000000 kB\nMemAvailable:   %d kB\nBuffers: 1 kB\nsome avg10=0.00 avg60=%.2f avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n", gibBytes(availGiB)/1024, psi)
}

func TestTheGuardFileStatesEveryThresholdExplicitly(t *testing.T) {
	g := testGuard(t)
	if !g.Guards("node-g") || g.Guards("node-a") {
		t.Fatalf("only node-g is guarded, got nodes %v", g.Nodes())
	}
	want := []string{
		"node node-g floor=11.00 GiB max-age=30s psi-some-avg60-max=1.00",
		"cost big01 container-down 1.50 GiB",
		"cost big01 device-down 8.00 GiB",
		"cost small01 device-down 4.00 GiB",
		"cost small01 service-down 1.00 GiB",
	}
	if got := g.Describe(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("effective configuration:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	bad := map[string]string{
		"no node at all":            "cost big01 device-down 8\n",
		"a node with no floor":      "node n max-age=30s psi-some-avg60-max=1 extra=1\n",
		"a node missing max-age":    "node n floor-gib=11 psi-some-avg60-max=1\n",
		"a zero floor":              "node n floor-gib=0 max-age=30s psi-some-avg60-max=1\n",
		"a negative floor":          "node n floor-gib=-1 max-age=30s psi-some-avg60-max=1\n",
		"a floor that is not a num": "node n floor-gib=lots max-age=30s psi-some-avg60-max=1\n",
		"a max-age of zero":         "node n floor-gib=11 max-age=0s psi-some-avg60-max=1\n",
		"a max-age over five min":   "node n floor-gib=11 max-age=6m psi-some-avg60-max=1\n",
		"a zero pressure limit":     "node n floor-gib=11 max-age=30s psi-some-avg60-max=0\n",
		"a repeated attribute":      "node n floor-gib=11 floor-gib=12 psi-some-avg60-max=1\n",
		"an unknown attribute":      "node n floor-gib=11 max-age=30s colour=blue\n",
		"a node declared twice":     "node n floor-gib=11 max-age=30s psi-some-avg60-max=1\nnode n floor-gib=12 max-age=30s psi-some-avg60-max=1\n",
		"a zero cost":               "node n floor-gib=11 max-age=30s psi-some-avg60-max=1\ncost g device-down 0\n",
		"an unknown class":          "node n floor-gib=11 max-age=30s psi-some-avg60-max=1\ncost g reboot 1\n",
		"a cost declared twice":     "node n floor-gib=11 max-age=30s psi-some-avg60-max=1\ncost g device-down 1\ncost g device-down 2\n",
		"a short cost line":         "node n floor-gib=11 max-age=30s psi-some-avg60-max=1\ncost g device-down\n",
		"an unknown directive":      "node n floor-gib=11 max-age=30s psi-some-avg60-max=1\nreserve g 8\n",
	}
	for name, text := range bad {
		if _, err := ParseMemGuard(strings.NewReader(text)); err == nil {
			t.Errorf("%s was accepted; an unstated or malformed threshold must refuse the file", name)
		}
	}
}

func TestTheGuardMustMatchThePoolAndRotation(t *testing.T) {
	g := testGuard(t)
	rotation := []Class{ClassDeviceDown, ClassContainerDown, ClassServiceDown}
	if err := g.Validate(guardedPool(), rotation); err != nil {
		t.Fatalf("a guard that covers every guarded pair must validate: %v", err)
	}
	if err := g.Validate(guardedPool(), append(rotation, ClassContainerHang)); err == nil || !strings.Contains(err.Error(), "big01 on guarded node node-g has no cost for rotation class container-hang") {
		t.Errorf("a guarded target with no cost for a rotation class it is eligible for must refuse to start, got %v", err)
	}
	if err := g.Validate(guardedPool(), append(rotation, ClassServiceHang)); err == nil || !strings.Contains(err.Error(), "small01") {
		t.Errorf("small01 declares a unit, so service-hang needs a cost, got %v", err)
	}
	moved := guardedPool()
	moved[1].Node = "node-a"
	if err := g.Validate(moved, rotation); err == nil || !strings.Contains(err.Error(), "small01") {
		t.Errorf("a cost for a guest that is not on a guarded node must refuse to start, got %v", err)
	}
}

func TestTheGuardAdmitsOnlyWhenTheNodeCanTakeTheRestoreBack(t *testing.T) {
	g := testGuard(t)
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	big, small, plain := guardedPool()[0], guardedPool()[1], guardedPool()[2]
	cases := []struct {
		name   string
		guest  PoolGuest
		class  Class
		tel    NodeTelemetry
		admit  bool
		reason SkipReason
		need   float64
	}{
		{"an unguarded node needs no reading", plain, ClassDeviceDown, NodeTelemetry{}, true, "", 0},
		{"exactly floor plus cost is enough", big, ClassDeviceDown, reading("node-g", 19, 0, now), true, "", 19},
		{"just under floor plus cost is not", big, ClassDeviceDown, reading("node-g", 18.99, 0, now), false, SkipBelowThreshold, 19},
		{"the smaller guest needs less", small, ClassDeviceDown, reading("node-g", 15, 0, now), true, "", 15},
		{"a container fault needs its own measured cost", big, ClassContainerDown, reading("node-g", 12.4, 0, now), false, SkipBelowThreshold, 12.5},
		{"a read error is unavailable", big, ClassDeviceDown, NodeTelemetry{Node: "node-g", Err: errors.New("ssh: timeout")}, false, SkipTelemetryUnavailable, 0},
		{"no reading at all is unavailable", big, ClassDeviceDown, NodeTelemetry{}, false, SkipTelemetryUnavailable, 0},
		{"a reading older than max-age is stale", big, ClassDeviceDown, reading("node-g", 40, 0, now.Add(-31*time.Second)), false, SkipTelemetryStale, 0},
		{"a reading from the future is stale", big, ClassDeviceDown, reading("node-g", 40, 0, now.Add(10*time.Second)), false, SkipTelemetryStale, 0},
		{"pressure at the limit refuses", big, ClassDeviceDown, reading("node-g", 40, 1, now), false, SkipMemoryPressure, 0},
		{"a pair with no declared cost refuses", small, ClassServiceHang, reading("node-g", 40, 0, now), false, SkipNoDeclaredCost, 0},
	}
	for _, c := range cases {
		ok, skip := g.Evaluate(now, c.guest, c.class, nil, c.tel)
		if ok != c.admit {
			t.Errorf("%s: admit=%v, want %v (%s)", c.name, ok, c.admit, skip.Detail)
			continue
		}
		if !ok && skip.Reason != c.reason {
			t.Errorf("%s: reason %q, want %q (%s)", c.name, skip.Reason, c.reason, skip.Detail)
		}
		if c.need > 0 && skip.RequiredBytes != gibBytes(c.need) {
			t.Errorf("%s: required %d bytes, want %.2f GiB", c.name, skip.RequiredBytes, c.need)
		}
		if !ok && (skip.Host != c.guest.Name || skip.Class != c.class || skip.Node != c.guest.Node || skip.Detail == "") {
			t.Errorf("%s: the skip must name the target, class, node and why: %+v", c.name, skip)
		}
	}
}

func TestOverlappingRestoresOnOneNodeAreAddedUp(t *testing.T) {
	g := testGuard(t)
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	small := guardedPool()[1]
	bigDown := Outstanding{ID: 7, Host: "big01", Class: ClassDeviceDown, Node: "node-g"}
	elsewhere := Outstanding{ID: 8, Host: "plain01", Class: ClassDeviceDown, Node: "node-a"}
	if ok, skip := g.Evaluate(now, small, ClassDeviceDown, []Outstanding{bigDown}, reading("node-g", 22.9, 0, now)); ok || skip.Reason != SkipBelowThreshold || skip.RequiredBytes != gibBytes(23) {
		t.Errorf("with big01's 8 GiB restore still owed, small01 needs 11+4+8=23 GiB; got admit=%v %+v", ok, skip)
	}
	if ok, _ := g.Evaluate(now, small, ClassDeviceDown, []Outstanding{bigDown}, reading("node-g", 23, 0, now)); !ok {
		t.Error("23 GiB covers the floor, this restore and the outstanding one, so the fault is admitted")
	}
	if ok, skip := g.Evaluate(now, small, ClassDeviceDown, []Outstanding{elsewhere}, reading("node-g", 15, 0, now)); !ok {
		t.Errorf("a fault outstanding on ANOTHER node takes nothing back from this one: %s", skip.Detail)
	}
	two := []Outstanding{bigDown, {ID: 9, Host: "small01", Class: ClassServiceDown, Node: "node-g"}}
	if ok, skip := g.Evaluate(now, guardedPool()[0], ClassContainerDown, two, reading("node-g", 21.4, 0, now)); ok || skip.RequiredBytes != gibBytes(21.5) {
		t.Errorf("two outstanding restores add up: 11+1.5+8+1=21.5 GiB; got admit=%v %+v", ok, skip)
	}
	unsized := []Outstanding{{ID: 10, Host: "big01", Class: ClassContainerHang, Node: "node-g"}}
	if ok, skip := g.Evaluate(now, small, ClassDeviceDown, unsized, reading("node-g", 100, 0, now)); ok || skip.Reason != SkipOutstandingWithoutCost {
		t.Errorf("an outstanding fault with no declared cost cannot be sized, so nothing more is admitted on its node; got admit=%v %+v", ok, skip)
	}
}

type telemetryRunner struct {
	out   string
	code  int
	err   error
	calls [][]string
	hosts []string
}

func (r *telemetryRunner) Run(_ context.Context, host string, argv []string) (string, int, error) {
	r.calls = append(r.calls, argv)
	r.hosts = append(r.hosts, host)
	return r.out, r.code, r.err
}

func TestTheReadingIsParsedFromTheNodesOwnFiles(t *testing.T) {
	at := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	r := &telemetryRunner{out: procReading(14.5, 0.07)}
	tel := ReadNodeTelemetry(context.Background(), r, "node-g", clock)
	if tel.Err != nil || tel.AvailableBytes != gibBytes(14.5)/1024*1024 || tel.PSISomeAvg60 != 0.07 || !tel.ReadAt.Equal(at) {
		t.Fatalf("reading = %+v", tel)
	}
	if len(r.calls) != 1 || r.hosts[0] != "node-g" || strings.Join(r.calls[0], " ") != "cat /proc/meminfo /proc/pressure/memory" {
		t.Errorf("the read must be the fixed argv on the node itself, got %v on %v", r.calls, r.hosts)
	}
	broken := map[string]*telemetryRunner{
		"a transport error":     {err: errors.New("ssh: connect timeout")},
		"a non-zero exit":       {code: 1, out: procReading(14, 0)},
		"no MemAvailable":       {out: "MemTotal: 1 kB\nsome avg10=0.00 avg60=0.00 avg300=0.00 total=1\n"},
		"no pressure line":      {out: "MemAvailable: 100 kB\n"},
		"a malformed available": {out: "MemAvailable: lots kB\nsome avg10=0.00 avg60=0.00 avg300=0.00 total=1\n"},
		"a malformed pressure":  {out: "MemAvailable: 100 kB\nsome avg10=0.00 avg60=high avg300=0.00 total=1\n"},
		"a wrong unit":          {out: "MemAvailable: 100 MB\nsome avg10=0.00 avg60=0.00 avg300=0.00 total=1\n"},
		"a NaN pressure":        {out: "MemAvailable: 100 kB\nsome avg10=0.00 avg60=NaN avg300=0.00 total=1\n"},
		"an infinite pressure":  {out: "MemAvailable: 100 kB\nsome avg10=0.00 avg60=+Inf avg300=0.00 total=1\n"},
	}
	for name, br := range broken {
		got := ReadNodeTelemetry(context.Background(), br, "node-g", clock)
		if got.Err == nil || !got.ReadAt.IsZero() {
			t.Errorf("%s must read as unavailable with no timestamp, got %+v", name, got)
		}
	}
}

func guardedState(t *testing.T, availGiB float64) State {
	t.Helper()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	pool := guardedPool()
	return State{
		Now: now, Pool: pool,
		Allowlist: map[string]bool{"big01": true, "small01": true, "plain01": true},
		Status:    map[string]string{"201": "running", "202": "running", "300": "running"},
		Limits:    Limits{MaxDown: 3, MaxBusy: 5, RestoreAfter: 20 * time.Minute},
		Guard:     testGuard(t),
		Telemetry: map[string]NodeTelemetry{"node-g": reading("node-g", availGiB, 0, now)},
	}
}

func TestThePlannerPassesOverARefusedTargetAndTakesTheNext(t *testing.T) {
	d := PlanNext(guardedState(t, 16), []Class{ClassDeviceDown})
	if !d.Act || d.Guest.Name != "small01" {
		t.Fatalf("16 GiB refuses big01 (needs 19) and admits small01 (needs 15); got %+v", d)
	}
	if len(d.Skips) != 1 || d.Skips[0].Host != "big01" || d.Skips[0].Stage != SkipAtPlan || d.Skips[0].Reason != SkipBelowThreshold {
		t.Errorf("the passed-over target must be listed for the skip record: %+v", d.Skips)
	}
	d = PlanNext(guardedState(t, 12), []Class{ClassDeviceDown})
	if !d.Act || d.Guest.Name != "plain01" || len(d.Skips) != 2 {
		t.Errorf("with both guarded guests refused the unguarded one is taken and both refusals are listed; got %+v", d)
	}
}

func TestWithNoEligibleTargetThePlannerSaysTheGuardWasWhy(t *testing.T) {
	st := guardedState(t, 12)
	st.Status["300"] = "stopped"
	d := PlanNext(st, []Class{ClassDeviceDown})
	if d.Act {
		t.Fatalf("no target fits, so nothing may be injected: %+v", d)
	}
	if !strings.Contains(d.Reason, "memory guard passed over 2 guarded candidate(s)") || len(d.Skips) != 2 {
		t.Errorf("an idle tick caused by the guard must say so and list every refusal; got %q, %d skips", d.Reason, len(d.Skips))
	}
	st.Guard = nil
	if d := PlanNext(st, []Class{ClassDeviceDown}); !d.Act || d.Guest.Name != "big01" || len(d.Skips) != 0 {
		t.Errorf("with no guard configured the planner is unchanged; got %+v", d)
	}
}

type guardRunner struct {
	readings []string
	readErr  error
	calls    []string
}

func (r *guardRunner) Run(_ context.Context, host string, argv []string) (string, int, error) {
	r.calls = append(r.calls, host+": "+strings.Join(argv, " "))
	switch {
	case len(argv) > 1 && argv[0] == "pvesh":
		return `[{"vmid":201,"status":"running"},{"vmid":202,"status":"running"},{"vmid":300,"status":"stopped"}]`, 0, nil
	case len(argv) > 2 && argv[0] == "pct" && argv[1] == "config":
		names := map[string]string{"201": "big01", "202": "small01", "300": "plain01"}
		return "hostname: " + names[argv[2]] + "\n", 0, nil
	case len(argv) > 1 && argv[0] == "cat" && argv[1] == "/proc/meminfo":
		if r.readErr != nil {
			return "", -1, r.readErr
		}
		out := r.readings[0]
		if len(r.readings) > 1 {
			r.readings = r.readings[1:]
		}
		return out, 0, nil
	case len(argv) > 1 && argv[1] == "status":
		return "status: running\n", 0, nil
	}
	return "", 0, nil
}

func (r *guardRunner) effects() []string {
	var out []string
	for _, c := range r.calls {
		if strings.Contains(c, " stop ") || strings.Contains(c, "docker stop") || strings.Contains(c, "systemctl stop") {
			out = append(out, c)
		}
	}
	return out
}

type skipSpy struct {
	skips []GuardSkip
	err   error
}

func (s *skipSpy) RecordSkip(_ context.Context, k GuardSkip) error {
	s.skips = append(s.skips, k)
	return s.err
}

func guardedEngine(t *testing.T, run *guardRunner, st *memLedger, spy *skipSpy) *Engine {
	t.Helper()
	at := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	return &Engine{
		Exec: run, Store: st, Log: func(string, ...any) {}, SnapNode: "node-g",
		Pool:      guardedPool(),
		Allowlist: map[string]bool{"big01": true, "small01": true, "plain01": true},
		Limits:    Limits{MaxDown: 3, MaxBusy: 5, RestoreAfter: 20 * time.Minute, SettleWindow: 10 * time.Minute},
		Rotation:  []Class{ClassDeviceDown},
		Guard:     testGuard(t), Skips: spy, Note: "guard-test",
		Clock: func() time.Time { return at },
	}
}

func TestALowReadingSkipsTheTargetAndRecordsWhy(t *testing.T) {
	run := &guardRunner{readings: []string{procReading(12, 0)}}
	st, spy := newMemStore(), &skipSpy{}
	if guardedEngine(t, run, st, spy).InjectOnce(t.Context(), 0, 0) {
		t.Fatal("12 GiB fits neither guarded target and plain01 is stopped, so nothing may be injected")
	}
	if st.recorded || len(run.effects()) != 0 {
		t.Errorf("a skipped target must leave no obligation and no effect: recorded=%v effects=%v", st.recorded, run.effects())
	}
	if len(spy.skips) != 2 {
		t.Fatalf("both refusals must be recorded, got %+v", spy.skips)
	}
	for _, s := range spy.skips {
		if s.Reason != SkipBelowThreshold || s.Stage != SkipAtPlan || !s.HasTelemetry || s.AvailableBytes != gibBytes(12)/1024*1024 || s.Note != "guard-test" {
			t.Errorf("a recorded skip must carry the reason, the stage, the reading and the run note: %+v", s)
		}
	}
}

func TestAnUnreadableNodeSkipsTheTarget(t *testing.T) {
	run := &guardRunner{readErr: errors.New("ssh: no route to host")}
	st, spy := newMemStore(), &skipSpy{}
	if guardedEngine(t, run, st, spy).InjectOnce(t.Context(), 0, 0) {
		t.Fatal("with the guarded node unreadable nothing on it may be faulted, and plain01 is stopped")
	}
	if st.recorded || len(run.effects()) != 0 {
		t.Errorf("an unreadable node must leave no obligation and no effect: recorded=%v effects=%v", st.recorded, run.effects())
	}
	if len(spy.skips) != 2 || spy.skips[0].Reason != SkipTelemetryUnavailable || spy.skips[0].HasTelemetry {
		t.Errorf("both targets must be recorded as skipped for unavailable telemetry, with no reading attached: %+v", spy.skips)
	}
}

func TestTheNodeIsReadAgainImmediatelyBeforeTheEffect(t *testing.T) {
	run := &guardRunner{readings: []string{procReading(20, 0), procReading(15, 0)}}
	st, spy := newMemStore(), &skipSpy{}
	if guardedEngine(t, run, st, spy).InjectOnce(t.Context(), 0, 0) {
		t.Fatal("the planning reading admitted big01, but the fresh reading before the effect shows 15 GiB, under its 19")
	}
	if st.recorded || len(run.effects()) != 0 {
		t.Errorf("a pre-effect refusal must record no obligation and touch nothing: recorded=%v effects=%v", st.recorded, run.effects())
	}
	if len(spy.skips) != 1 || spy.skips[0].Stage != SkipBeforeEffect || spy.skips[0].Host != "big01" || spy.skips[0].AvailableBytes != gibBytes(15)/1024*1024 {
		t.Errorf("the pre-effect refusal must be recorded with its own reading: %+v", spy.skips)
	}
}

func TestAnAdmittedTargetIsInjectedAfterItsFreshReading(t *testing.T) {
	run := &guardRunner{readings: []string{procReading(20, 0), procReading(20, 0)}}
	st, spy := newMemStore(), &skipSpy{}
	if !guardedEngine(t, run, st, spy).InjectOnce(t.Context(), 0, 0) {
		t.Fatalf("20 GiB covers big01's 19, so the fault must land; calls %v", run.calls)
	}
	if !st.recorded || len(spy.skips) != 0 {
		t.Errorf("an admitted fault records its obligation and no skip: recorded=%v skips=%+v", st.recorded, spy.skips)
	}
	last, stop := -1, -1
	for i, c := range run.calls {
		if strings.HasPrefix(c, "node-g: cat /proc/meminfo") {
			last = i
		}
		if c == "node-g: pct stop 201" {
			stop = i
		}
	}
	if last < 0 || stop < 0 || last > stop {
		t.Errorf("the last reading must come before the stop: %v", run.calls)
	}
	for _, c := range run.calls[last+1 : stop] {
		if strings.HasPrefix(c, "node-g: pct config") || strings.HasPrefix(c, "node-g: pvesh") {
			t.Errorf("nothing but the obligation write may sit between the fresh reading and the effect, saw %q", c)
		}
	}
}

func TestAFailedSkipWriteStillInjectsNothing(t *testing.T) {
	run := &guardRunner{readings: []string{procReading(12, 0)}}
	st, spy := newMemStore(), &skipSpy{err: errors.New("db down")}
	if guardedEngine(t, run, st, spy).InjectOnce(t.Context(), 0, 0) || st.recorded || len(run.effects()) != 0 {
		t.Error("a skip whose record failed is still a skip: no obligation and no effect")
	}
}

func TestTheGuardNeverBlocksARestore(t *testing.T) {
	due := Outstanding{ID: 4, Host: "big01", Class: ClassDeviceDown, Node: "node-g", FaultRef: "201", RestoreDueAt: time.Now().Add(-time.Minute)}
	run := &guardRunner{readings: []string{procReading(1, 5)}}
	st, spy := newMemStore(due), &skipSpy{}
	e := guardedEngine(t, run, st, spy)
	if n := e.ReconcileOnce(t.Context()); n != 1 || st.state[4] != "restored" {
		t.Fatalf("a due restore must run whatever the node's memory reads: closed %d, state %q, calls %v", n, st.state[4], run.calls)
	}
	started := false
	for _, c := range run.calls {
		if c == "node-g: pct start 201" {
			started = true
		}
		if strings.HasPrefix(c, "node-g: cat /proc/meminfo") {
			t.Errorf("the restore path must not even consult the guard, but it read %q", c)
		}
	}
	if !started || len(spy.skips) != 0 {
		t.Errorf("the guest must be started and nothing recorded as skipped: started=%v skips=%+v", started, spy.skips)
	}
}

func TestAPauseFileHoldsNewFaultsWhileRestoresContinue(t *testing.T) {
	pause := filepath.Join(t.TempDir(), "PAUSE")
	if err := os.WriteFile(pause, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	due := Outstanding{ID: 4, Host: "small01", Class: ClassDeviceDown, Node: "node-g", FaultRef: "202", RestoreDueAt: time.Now().Add(-time.Minute)}
	run := &guardRunner{readings: []string{procReading(40, 0)}}
	st, spy := newMemStore(due), &skipSpy{}
	e := guardedEngine(t, run, st, spy)
	e.PauseFile, e.Cadence = pause, 5*time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	_ = e.Run(ctx)
	if st.state[4] != "restored" {
		t.Errorf("a pause must never hold a restore: the due fault is %q", st.state[4])
	}
	if st.recorded || len(run.effects()) != 0 {
		t.Errorf("40 GiB would admit big01, so only the pause can explain no new fault: recorded=%v effects=%v", st.recorded, run.effects())
	}
	if err := os.Remove(pause); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel2()
	_ = e.Run(ctx2)
	if !st.recorded {
		t.Error("removing the pause file must resume injection")
	}
}

func TestAnOutstandingFaultWithNoRecordedNodeStillCounts(t *testing.T) {
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	small := guardedPool()[1]
	unplaced := []Outstanding{{ID: 11, Host: "big01", Class: ClassDeviceDown}}
	validated := testGuard(t)
	if err := validated.Validate(guardedPool(), []Class{ClassDeviceDown, ClassContainerDown, ClassServiceDown}); err != nil {
		t.Fatal(err)
	}
	for name, g := range map[string]*MemGuard{"through the pool": validated, "through its declared cost": testGuard(t)} {
		if ok, skip := g.Evaluate(now, small, ClassDeviceDown, unplaced, reading("node-g", 22.9, 0, now)); ok || skip.RequiredBytes != gibBytes(23) {
			t.Errorf("%s: a row with an empty node is still big01's 8 GiB restore, so small01 needs 23 GiB; got admit=%v %+v", name, ok, skip)
		}
	}
	unsized := []Outstanding{{ID: 13, Host: "big01", Class: ClassContainerHang}}
	if ok, skip := validated.Evaluate(now, small, ClassDeviceDown, unsized, reading("node-g", 100, 0, now)); ok || skip.Reason != SkipOutstandingWithoutCost {
		t.Errorf("an unplaced row the pool puts on this node, with no declared cost, cannot be sized, so nothing more is admitted here; got admit=%v %+v", ok, skip)
	}
	elsewhere := []Outstanding{{ID: 12, Host: "plain01", Class: ClassDeviceDown}}
	if ok, skip := validated.Evaluate(now, small, ClassDeviceDown, elsewhere, reading("node-g", 15, 0, now)); !ok {
		t.Errorf("an unplaced row whose guest the pool puts on another node takes nothing back here: %s", skip.Detail)
	}
}

func TestAnUnreadablePausePathHoldsNewFaults(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e := &Engine{PauseFile: filepath.Join(file, "PAUSE"), Log: func(string, ...any) {}}
	if e.pauseReasonNow() == "" {
		t.Error("a pause path that cannot be read must read as paused, never as released")
	}
	e.PauseFile = filepath.Join(t.TempDir(), "PAUSE")
	if e.pauseReasonNow() != "" {
		t.Error("a pause file that cleanly does not exist must not pause")
	}
}

type secondReadFails struct {
	*memLedger
	reads int
}

func (s *secondReadFails) Outstanding(ctx context.Context) ([]Outstanding, error) {
	s.reads++
	if s.reads > 1 {
		return nil, errors.New("connection reset")
	}
	return s.memLedger.Outstanding(ctx)
}

func TestUnreadableObligationsBeforeTheEffectSkip(t *testing.T) {
	run := &guardRunner{readings: []string{procReading(40, 0)}}
	inner, spy := newMemStore(), &skipSpy{}
	st := &secondReadFails{memLedger: inner}
	e := guardedEngine(t, run, inner, spy)
	e.Store = st
	if e.InjectOnce(t.Context(), 0, 0) {
		t.Fatal("the obligations could not be read again before the effect, so the overlap sum is unknown: nothing may be injected")
	}
	if inner.recorded || len(run.effects()) != 0 {
		t.Errorf("no obligation and no effect: recorded=%v effects=%v", inner.recorded, run.effects())
	}
	if len(spy.skips) != 1 || spy.skips[0].Reason != SkipObligationsUnreadable || spy.skips[0].Stage != SkipBeforeEffect {
		t.Errorf("the skip must say the obligations were unreadable before the effect: %+v", spy.skips)
	}
}

func TestAReadingThatAgesPastMaxAgeIsRefusedInTheEngine(t *testing.T) {
	run := &guardRunner{readings: []string{procReading(40, 0)}}
	st, spy := newMemStore(), &skipSpy{}
	e := guardedEngine(t, run, st, spy)
	at := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	e.Clock = func() time.Time { at = at.Add(40 * time.Second); return at }
	if e.InjectOnce(t.Context(), 0, 0) {
		t.Fatal("a reading 40 s old against a 30 s max-age is stale, and plain01 is stopped, so nothing may be injected")
	}
	if len(spy.skips) != 2 || spy.skips[0].Reason != SkipTelemetryStale {
		t.Errorf("both guarded targets must be skipped as stale: %+v", spy.skips)
	}
}

func TestTheGuardNeverHoldsADrain(t *testing.T) {
	due := Outstanding{ID: 5, Host: "big01", Class: ClassDeviceDown, Node: "node-g", FaultRef: "201", RestoreDueAt: time.Now().Add(-time.Minute)}
	run := &guardRunner{readings: []string{procReading(1, 5)}}
	st, spy := newMemStore(due), &skipSpy{}
	e := guardedEngine(t, run, st, spy)
	e.Drain(t.Context(), time.Second)
	if st.state[5] != "restored" {
		t.Errorf("a drain must discharge the due restore whatever the node reads: %q", st.state[5])
	}
	for _, c := range run.calls {
		if strings.HasPrefix(c, "node-g: cat /proc/meminfo") {
			t.Errorf("the drain path must not consult the guard, but it read %q", c)
		}
	}
}

func TestTheKillFileWinsOverThePause(t *testing.T) {
	dir := t.TempDir()
	kill, pause := filepath.Join(dir, "STOP"), filepath.Join(dir, "PAUSE")
	for _, f := range []string{kill, pause} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	due := Outstanding{ID: 6, Host: "small01", Class: ClassDeviceDown, Node: "node-g", FaultRef: "202", RestoreDueAt: time.Now().Add(-time.Minute)}
	run := &guardRunner{readings: []string{procReading(40, 0)}}
	st, spy := newMemStore(due), &skipSpy{}
	e := guardedEngine(t, run, st, spy)
	e.KillFile, e.PauseFile, e.Cadence = kill, pause, 5*time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := e.Run(ctx); err != nil || ctx.Err() != nil {
		t.Fatalf("with the kill file present Run must drain and return on its own, got err=%v ctx=%v", err, ctx.Err())
	}
	if st.state[6] != "restored" || st.recorded {
		t.Errorf("the kill drains the due restore and injects nothing: state=%q recorded=%v", st.state[6], st.recorded)
	}
}

func TestTheThresholdIsComparedToTheByte(t *testing.T) {
	g := testGuard(t)
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	big := guardedPool()[0]
	const need = int64(19) << 30
	if need != 20401094656 {
		t.Fatalf("19 GiB is 20401094656 bytes, got %d", need)
	}
	for _, c := range []struct {
		avail int64
		admit bool
	}{{need, true}, {need + 1, true}, {need - 1, false}, {need - 1024, false}} {
		ok, skip := g.Evaluate(now, big, ClassDeviceDown, nil, NodeTelemetry{Node: "node-g", AvailableBytes: c.avail, ReadAt: now})
		if ok != c.admit || skip.RequiredBytes != need {
			t.Errorf("available %d bytes against %d: admit=%v required=%d, want admit=%v", c.avail, need, ok, skip.RequiredBytes, c.admit)
		}
	}
	at := func() time.Time { return now }
	for _, c := range []struct {
		kb    int64
		admit bool
	}{{need / 1024, true}, {need/1024 - 1, false}} {
		r := &telemetryRunner{out: fmt.Sprintf("MemAvailable: %d kB\nsome avg10=0.00 avg60=0.00 avg300=0.00 total=1\n", c.kb)}
		tel := ReadNodeTelemetry(context.Background(), r, "node-g", at)
		if ok, _ := g.Evaluate(now, big, ClassDeviceDown, nil, tel); ok != c.admit || tel.AvailableBytes != c.kb*1024 {
			t.Errorf("MemAvailable %d kB reads %d bytes, admit=%v; want %d bytes and admit=%v", c.kb, tel.AvailableBytes, ok, c.kb*1024, c.admit)
		}
	}
}

func TestAStaleOrMissingWatcherHeartbeatHoldsNewFaults(t *testing.T) {
	dir := t.TempDir()
	beat := filepath.Join(dir, "HEARTBEAT")
	now := time.Now().UTC()
	e := &Engine{PauseHeartbeat: beat, PauseHeartbeatMaxAge: 90 * time.Second, Log: func(string, ...any) {}, Clock: func() time.Time { return now }}
	if e.pauseReasonNow() == "" {
		t.Error("a missing heartbeat means the watcher is not running, so new faults must be held")
	}
	if err := os.WriteFile(beat, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		mtime  time.Time
		paused bool
	}{
		{"a fresh heartbeat", now.Add(-10 * time.Second), false},
		{"a heartbeat at exactly the max age", now.Add(-90 * time.Second), false},
		{"a heartbeat past the max age", now.Add(-91 * time.Second), true},
		{"a heartbeat dated in the future", now.Add(time.Minute), true},
	} {
		if err := os.Chtimes(beat, c.mtime, c.mtime); err != nil {
			t.Fatal(err)
		}
		if got := (e.pauseReasonNow() != ""); got != c.paused {
			t.Errorf("%s: paused=%v, want %v (%s)", c.name, got, c.paused, e.pauseReasonNow())
		}
	}
}

func TestAStoppedWatcherHoldsFaultsButNotRestores(t *testing.T) {
	beat := filepath.Join(t.TempDir(), "HEARTBEAT")
	if err := os.WriteFile(beat, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(beat, old, old); err != nil {
		t.Fatal(err)
	}
	due := Outstanding{ID: 7, Host: "small01", Class: ClassDeviceDown, Node: "node-g", FaultRef: "202", RestoreDueAt: time.Now().Add(-time.Minute)}
	run := &guardRunner{readings: []string{procReading(40, 0)}}
	st, spy := newMemStore(due), &skipSpy{}
	e := guardedEngine(t, run, st, spy)
	e.Clock = nil
	e.PauseHeartbeat, e.PauseHeartbeatMaxAge, e.Cadence = beat, 90*time.Second, 5*time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	_ = e.Run(ctx)
	if st.state[7] != "restored" {
		t.Errorf("a stopped watcher must never hold a restore: %q", st.state[7])
	}
	if st.recorded || len(run.effects()) != 0 {
		t.Errorf("with the watcher's heartbeat ten minutes old no new fault may land: recorded=%v effects=%v", st.recorded, run.effects())
	}
	if err := os.Chtimes(beat, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel2()
	_ = e.Run(ctx2)
	if !st.recorded {
		t.Error("a fresh heartbeat with no pause file must let injection resume")
	}
}
