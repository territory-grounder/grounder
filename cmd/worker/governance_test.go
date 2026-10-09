package main

// Oracles for the governance boot wiring (TG-222, spec/004 REQ-307/REQ-308).
//
// These drive the REAL armGovernanceSchedules — the same function cmd/worker's main() calls, not a
// reimplementation of it — with a recording ScheduleClient and a recording worker. What is faked is
// Temporal's server and nothing else: the schedule options, the workflow names, the task queue and the
// registration set all come from production code.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/governance"
	tggov "github.com/territory-grounder/grounder/temporal/governance"
)

type recordingScheduler struct {
	opts []client.ScheduleOptions
	err  error
}

func (r *recordingScheduler) Create(_ context.Context, o client.ScheduleOptions) (client.ScheduleHandle, error) {
	r.opts = append(r.opts, o)
	return nil, r.err
}

type recordingWorker struct {
	workflows  int
	activities int
}

func (r *recordingWorker) RegisterWorkflow(any) { r.workflows++ }
func (r *recordingWorker) RegisterActivity(any) { r.activities++ }

func wiredActivities() *tggov.Activities {
	return &tggov.Activities{
		Monitor:    &governance.JudgeLivenessMonitor{Window: 24 * time.Hour, Lag: 2 * time.Hour},
		CrossCheck: &governance.FrontierCrossCheckMonitor{},
	}
}

// THE SCHEDULE EXISTS AFTER BOOT WIRING. Before TG-222 nothing in the tree called CreateSchedules and the
// workflow names matched no function, so this assertion had nothing to make.
func TestGovernanceSchedulesExistAfterBootWiring(t *testing.T) {
	sc := &recordingScheduler{}
	w := &recordingWorker{}
	if err := armGovernanceSchedules(context.Background(), sc, w, wiredActivities(), func(string, ...any) {}); err != nil {
		t.Fatalf("arming failed: %v", err)
	}
	want := map[string]string{
		tggov.JudgeLivenessScheduleID:      "JudgeLivenessWorkflow",
		tggov.FrontierCrossCheckScheduleID: "FrontierCrossCheckWorkflow",
	}
	if len(sc.opts) != len(want) {
		t.Fatalf("created %d schedules, want %d", len(sc.opts), len(want))
	}
	for _, o := range sc.opts {
		wantWF, known := want[o.ID]
		if !known {
			t.Fatalf("unexpected schedule %q", o.ID)
		}
		act, ok := o.Action.(*client.ScheduleWorkflowAction)
		if !ok {
			t.Fatalf("schedule %q has no workflow action", o.ID)
		}
		if act.Workflow != wantWF {
			t.Fatalf("schedule %q runs %v, want %q", o.ID, act.Workflow, wantWF)
		}
		if len(o.Spec.Intervals) != 1 || o.Spec.Intervals[0].Every <= 0 {
			t.Fatalf("schedule %q has no positive interval: %+v", o.ID, o.Spec)
		}
		delete(want, o.ID)
	}
	// Both workflows AND both activities must be registered, or the schedule fires a task nothing executes.
	if w.workflows != 2 || w.activities != 2 {
		t.Fatalf("registered %d workflows / %d activities, want 2 / 2", w.workflows, w.activities)
	}
}

// An arming that creates nothing must be an ERROR, not a quiet success. "No monitor is wired" is precisely
// the pre-TG-222 state, and it reported no failure of any kind.
func TestArmingWithNoWiredMonitorIsAnError(t *testing.T) {
	err := armGovernanceSchedules(context.Background(), &recordingScheduler{}, &recordingWorker{},
		&tggov.Activities{}, func(string, ...any) {})
	if err == nil {
		t.Fatal("arming with no wired monitor must fail loudly — judge death would be UNDETECTABLE")
	}
	if !strings.Contains(err.Error(), "UNDETECTABLE") {
		t.Fatalf("the error must name the consequence, got %q", err)
	}
	if err := armGovernanceSchedules(context.Background(), &recordingScheduler{}, &recordingWorker{},
		nil, func(string, ...any) {}); err == nil {
		t.Fatal("nil activities must fail loudly")
	}
}

// A create failure surfaces rather than being swallowed into a green boot.
func TestScheduleCreateFailureSurfaces(t *testing.T) {
	boom := errors.New("temporal unavailable")
	err := armGovernanceSchedules(context.Background(), &recordingScheduler{err: boom}, &recordingWorker{},
		wiredActivities(), func(string, ...any) {})
	if !errors.Is(err, boom) {
		t.Fatalf("a create failure must surface, got %v", err)
	}
}

// THE JUDGE-DEATH PATH FIRES AND HALTS ACCRUAL, end to end through the production types: the REAL
// JudgeLivenessMonitor drives the REAL JudgeDeadMan over a REAL core/breaker, and the REAL activity is the
// entry point. Only the two data ports and the escalation channel are fakes.
func TestJudgeDeathFiresAndHaltsAccrual(t *testing.T) {
	dm, err := governance.NewJudgeDeadMan(breaker.NewMemStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if halted, _ := dm.Halted(context.Background()); halted {
		t.Fatal("a fresh dead-man must not be halted")
	}
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	// Ten sessions ended four hours ago (past the 2h lag, inside the 24h window); the judge scored one.
	var sessions []governance.Session
	for i := 0; i < 10; i++ {
		sessions = append(sessions, governance.Session{SessionID: string(rune('a' + i)), EndedAt: now.Add(-4 * time.Hour)})
	}
	esc := &recordingEscalator{}
	acts := &tggov.Activities{Monitor: &governance.JudgeLivenessMonitor{
		Sessions:   fakeSessions(sessions),
		Judgments:  fakeJudgments{"a": true},
		Escalation: esc,
		Window:     24 * time.Hour,
		Lag:        2 * time.Hour,
		Halt:       dm,
	}}
	res, err := acts.JudgeLivenessActivity(context.Background(), now)
	if err != nil {
		t.Fatalf("liveness activity: %v", err)
	}
	if !res.Warned || !res.Halted {
		t.Fatalf("a 1-in-10 judged fraction must warn AND halt, got %+v", res)
	}
	if len(esc.kinds) != 1 || esc.kinds[0] != "judge-death" {
		t.Fatalf("the warning must reach the escalation module, got %v", esc.kinds)
	}
	halted, reason := dm.Halted(context.Background())
	if !halted {
		t.Fatal("judge death must leave the dead-man HALTED — a warning that stops nothing is the failure mode")
	}
	if !strings.Contains(reason, "halted") {
		t.Fatalf("the halt must carry an operator-facing reason, got %q", reason)
	}
	// The halt does not self-heal: only a deliberate Rearm clears it.
	if err := dm.Rearm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if halted, _ := dm.Halted(context.Background()); halted {
		t.Fatal("Rearm must clear the halt")
	}
}

// A dead-man whose state store cannot be read reads HALTED. Accruing graduation decisions on a judge whose
// health is unobservable is the unsafe direction.
func TestUnreadableDeadManStateHaltsAccrual(t *testing.T) {
	dm, err := governance.NewJudgeDeadMan(unreadableStore{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	halted, reason := dm.Halted(context.Background())
	if !halted {
		t.Fatal("an unreadable judge-death breaker must read HALTED (fail closed)")
	}
	if !strings.Contains(reason, "CLOSED") {
		t.Fatalf("the reason must name the fail direction, got %q", reason)
	}
}

type unreadableStore struct{}

func (unreadableStore) Load(context.Context, string) (breaker.Record, bool, error) {
	return breaker.Record{}, false, errors.New("store down")
}
func (unreadableStore) Save(context.Context, breaker.Record) error { return errors.New("store down") }
func (unreadableStore) List(context.Context) ([]breaker.Record, error) {
	return nil, errors.New("store down")
}

type fakeSessions []governance.Session

func (f fakeSessions) RecentlyEnded(context.Context) ([]governance.Session, error) {
	return []governance.Session(f), nil
}

type fakeJudgments map[string]bool

func (f fakeJudgments) HasRealJudgment(_ context.Context, id string) bool { return f[id] }

type recordingEscalator struct{ kinds []string }

func (r *recordingEscalator) Warn(_ context.Context, kind, _ string) error {
	r.kinds = append(r.kinds, kind)
	return nil
}
