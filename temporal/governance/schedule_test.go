package governance

import (
	"errors"
	"fmt"
	"testing"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	coregov "github.com/territory-grounder/grounder/core/governance"
	tg "github.com/territory-grounder/grounder/temporal"
)

// CreateSchedules is genuinely idempotent: an already-existing schedule is skipped, and the loop still
// reaches the LATER (frontier cross-check) schedule. The naive abort-on-first-error shape silently dropped
// the later dead-man whenever the first already existed, leaving that judge-death control uninstalled.
func TestCreateSchedulesIdempotent(t *testing.T) {
	specs := (&Activities{
		Monitor:    &coregov.JudgeLivenessMonitor{},
		CrossCheck: &coregov.FrontierCrossCheckMonitor{},
	}).Specs()
	// judge-liveness already exists (ErrScheduleAlreadyRunning); the frontier cross-check does not.
	var created []string
	err := createSchedules(specs, func(opts client.ScheduleOptions) error {
		created = append(created, opts.ID)
		if opts.ID == JudgeLivenessScheduleID {
			return temporal.ErrScheduleAlreadyRunning
		}
		return nil
	})
	if err != nil {
		t.Fatalf("an already-existing schedule must not be fatal: %v", err)
	}
	// BOTH schedules must have been attempted — the second is not skipped by the first's already-exists.
	if len(created) != 2 || created[1] != FrontierCrossCheckScheduleID {
		t.Fatalf("the loop must reach the frontier cross-check schedule after the first already exists, got %v", created)
	}
	// a genuine (non-already-exists) error still aborts and surfaces.
	boom := errors.New("boom")
	if err := createSchedules(specs, func(client.ScheduleOptions) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("a real create error must surface, got %v", err)
	}
}

// A schedule is created ONLY for a monitor that is actually wired. A schedule whose activity has a nil
// collaborator fires into a guaranteed failure, and a governance alarm that is permanently red trains an
// operator to stop reading governance alarms — which is how the real one gets missed.
func TestNoScheduleForAnUnwiredMonitor(t *testing.T) {
	if got := (&Activities{}).Specs(); len(got) != 0 {
		t.Fatalf("an Activities with no wired monitor must register NO schedule, got %v", got)
	}
	only := (&Activities{Monitor: &coregov.JudgeLivenessMonitor{}}).Specs()
	if len(only) != 1 || only[0].ID != JudgeLivenessScheduleID {
		t.Fatalf("only the wired monitor's schedule may be registered, got %v", only)
	}
	// The demote worker (finding #5 / TG-219) has no live incident source, so it is deliberately NOT
	// schedulable here even when a Demoter is present.
	for _, s := range (&Activities{Demoter: &coregov.Demoter{}}).Specs() {
		if s.ID == GovernanceMetricsScheduleID {
			t.Fatal("the demote worker must not be scheduled until it has a live incident source")
		}
	}
}

// Every schedule spec must name a workflow that EXISTS and is registered on the worker's queue. The
// pre-TG-222 file named two workflows as string literals that no Go function anywhere matched — the
// schedules could not have run even if something had created them.
func TestEverySpecNamesALiveWorkflowOnAServedQueue(t *testing.T) {
	live := map[string]any{
		"JudgeLivenessWorkflow":      JudgeLivenessWorkflow,
		"FrontierCrossCheckWorkflow": FrontierCrossCheckWorkflow,
	}
	specs := (&Activities{
		Monitor:    &coregov.JudgeLivenessMonitor{},
		CrossCheck: &coregov.FrontierCrossCheckMonitor{},
	}).Specs()
	if len(specs) == 0 {
		t.Fatal("no schedule specs at all")
	}
	var queues []string
	if err := createSchedules(specs, func(opts client.ScheduleOptions) error {
		act, ok := opts.Action.(*client.ScheduleWorkflowAction)
		if !ok {
			return errors.New("a schedule action must start a workflow")
		}
		name, isName := act.Workflow.(string)
		if !isName {
			return fmt.Errorf("schedule %s names its workflow as %T, not a registrable name", opts.ID, act.Workflow)
		}
		if _, exists := live[name]; !exists {
			return fmt.Errorf("schedule %s names workflow %q, which is defined nowhere", opts.ID, name)
		}
		queues = append(queues, act.TaskQueue)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range queues {
		// The sole worker.New in the tree listens on TaskQueueRunner; a schedule aimed at any other queue
		// would enqueue workflow tasks nothing ever polls — a dead schedule wearing a live one's appearance.
		if q != tg.TaskQueueRunner {
			t.Fatalf("schedule action targets task queue %q, but no worker polls it", q)
		}
	}
}
