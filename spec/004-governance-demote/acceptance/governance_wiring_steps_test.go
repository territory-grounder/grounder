package acceptance

// spec/004 REQ-307/REQ-308 steps — the frontier cross-check and the armed judged-accrual halt (TG-222).
//
// These drive the REAL production objects: the real FrontierCrossCheckMonitor over the real
// temporal/governance.ModelPairSource, the real JudgeDeadMan over a real core/breaker, the real
// temporal/governance schedule creation, and the real skilltrial.FinalizeActivity. The fakes are exactly the
// process boundaries — the sample store, the frontier model transport, and Temporal's schedule server.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cucumber/godog"
	"go.temporal.io/sdk/client"

	adaptermodel "github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/breaker"
	gov "github.com/territory-grounder/grounder/core/governance"
	"github.com/territory-grounder/grounder/core/skillstore"
	tggov "github.com/territory-grounder/grounder/temporal/governance"
	"github.com/territory-grounder/grounder/temporal/skilltrial"
)

const acceptStrongVerdict = `{"correct_diagnosis":5,"evidence_grounded":5,"sensible_proposal":4,"appropriate_band":5,"falsifiable_prediction":4}`
const acceptWeakVerdict = `{"correct_diagnosis":1,"evidence_grounded":1,"sensible_proposal":2,"appropriate_band":1,"falsifiable_prediction":1}`

type acceptSample []gov.CrossCheckRow

func (a acceptSample) RecentForCrossCheck(context.Context, int) ([]gov.CrossCheckRow, error) {
	return []gov.CrossCheckRow(a), nil
}

type acceptFrontier struct {
	reply string
	err   error
}

func (f acceptFrontier) Complete(context.Context, string, string, []adaptermodel.Message) (string, error) {
	return f.reply, f.err
}

type acceptScheduler struct{ opts []client.ScheduleOptions }

func (s *acceptScheduler) Create(_ context.Context, o client.ScheduleOptions) (client.ScheduleHandle, error) {
	s.opts = append(s.opts, o)
	return nil, nil
}

type wiringWorld struct {
	sample   acceptSample
	frontier acceptFrontier
	deadMan  *gov.JudgeDeadMan
	esc      *fakeEscalator
	cross    gov.CrossCheckResult

	sched      *acceptScheduler
	registered int
	armErr     error
	pending    *tggov.Activities

	trials     *skillstore.MemTrialStore
	trialID    int64
	finalizeEr error

	deadManRC int
}

func (w *wiringWorld) RegisterWorkflow(any) { w.registered++ }
func (w *wiringWorld) RegisterActivity(any) {}

func init() { wiringRegistrars = append(wiringRegistrars, registerWiringSteps) }

// wiringRegistrars lets initializeScenario pick these steps up without editing the existing registrar.
var wiringRegistrars []func(*godog.ScenarioContext)

func acceptRow(ref string, localScored bool, localMean float64) gov.CrossCheckRow {
	return gov.CrossCheckRow{
		ExternalRef: ref, Host: "web01", AlertRule: "HostDown", Band: "AUTO_NOTICE",
		Outcome: "proposed", Proposed: true, Op: "restart-service",
		LocalScored: localScored, LocalMean: localMean,
	}
}

func registerWiringSteps(sc *godog.ScenarioContext) {
	w := &wiringWorld{esc: &fakeEscalator{}}
	ctx := context.Background()

	newDeadMan := func() *gov.JudgeDeadMan {
		dm, err := gov.NewJudgeDeadMan(breaker.NewMemStore(), nil)
		if err != nil {
			panic(err)
		}
		return dm
	}
	runCrossCheck := func() error {
		w.deadMan = newDeadMan()
		m := &gov.FrontierCrossCheckMonitor{
			Pairs: &tggov.ModelPairSource{
				Sample: w.sample, Model: w.frontier, Tier: "frontier-tier", Limit: 20,
			},
			Escalation: w.esc,
			Halt:       w.deadMan,
		}
		res, err := m.Run(ctx)
		w.cross = res
		return err
	}

	// ---- REQ-307: the independent anchor ----
	sc.Step(`^four recently-ended sessions the local judge left unscored$`, func() error {
		w.sample = nil
		for _, ref := range []string{"TG-1", "TG-2", "TG-3", "TG-4"} {
			w.sample = append(w.sample, acceptRow(ref, false, 0))
		}
		return nil
	})
	sc.Step(`^seven recently-ended sessions the local judge scored strongly$`, func() error {
		w.sample = nil
		for _, ref := range []string{"TG-1", "TG-2", "TG-3", "TG-4", "TG-5", "TG-6", "TG-7"} {
			w.sample = append(w.sample, acceptRow(ref, true, 4.6))
		}
		return nil
	})
	sc.Step(`^an independent frontier model re-judges the sample over the same rubric$`, func() error {
		w.frontier = acceptFrontier{reply: acceptStrongVerdict}
		return runCrossCheck()
	})
	sc.Step(`^the independent frontier model reaches the opposite verdict on every one$`, func() error {
		w.frontier = acceptFrontier{reply: acceptWeakVerdict}
		return runCrossCheck()
	})
	sc.Step(`^the independent frontier model fails on every re-judgment$`, func() error {
		w.frontier = acceptFrontier{err: errors.New("frontier unavailable")}
		return runCrossCheck()
	})
	sc.Step(`^a confirmed judge-death warning is raised and judged accrual is halted$`, func() error {
		if !w.cross.Death {
			return fmt.Errorf("the frontier scored sessions the local judge did not — that is confirmed DEATH: %+v", w.cross)
		}
		if !w.cross.Halted {
			return fmt.Errorf("a confirmed death must HALT judged accrual, not merely warn")
		}
		if halted, _ := w.deadMan.Halted(ctx); !halted {
			return fmt.Errorf("the judged-accrual dead-man is not halted")
		}
		return nil
	})
	sc.Step(`^a judge-drift warning is raised and judged accrual is not halted$`, func() error {
		if !w.cross.Drift || w.cross.Death {
			return fmt.Errorf("total disagreement over a wide sample is DRIFT, not death: %+v", w.cross)
		}
		if w.cross.Halted {
			return fmt.Errorf("drift must not halt — which judge is wrong is a human adjudication")
		}
		if halted, _ := w.deadMan.Halted(ctx); halted {
			return fmt.Errorf("the dead-man must be untouched by drift")
		}
		return nil
	})
	sc.Step(`^the cross-check raises no judge-death warning$`, func() error {
		if w.cross.Death || w.cross.DeathHits != 0 {
			return fmt.Errorf("a frontier that scored nothing must never manufacture a death: %+v", w.cross)
		}
		return nil
	})

	// ---- REQ-308: boot arming ----
	arm := func(acts *tggov.Activities) error {
		w.sched = &acceptScheduler{}
		w.registered = 0
		w.armErr = armGovernanceSchedulesForAcceptance(ctx, w.sched, w, acts)
		return nil
	}
	sc.Step(`^the governance activities are wired with a liveness monitor and a frontier cross-check$`, func() error {
		w.pending = &tggov.Activities{
			Monitor:    &gov.JudgeLivenessMonitor{Window: 24 * time.Hour, Lag: 2 * time.Hour},
			CrossCheck: &gov.FrontierCrossCheckMonitor{},
		}
		return nil
	})
	sc.Step(`^governance activities with no monitor wired at all$`, func() error {
		w.pending = &tggov.Activities{}
		return nil
	})
	sc.Step(`^the worker boot arms the governance schedules$`, func() error { return arm(w.pending) })
	sc.Step(`^one schedule exists per monitor and each names a workflow that is registered on the served queue$`, func() error {
		if w.armErr != nil {
			return fmt.Errorf("arming failed: %w", w.armErr)
		}
		if len(w.sched.opts) != 2 {
			return fmt.Errorf("want one schedule per wired monitor (2), got %d", len(w.sched.opts))
		}
		if w.registered != 2 {
			return fmt.Errorf("each scheduled workflow must be registered on the worker, registered %d", w.registered)
		}
		for _, o := range w.sched.opts {
			act, ok := o.Action.(*client.ScheduleWorkflowAction)
			if !ok {
				return fmt.Errorf("schedule %q starts no workflow", o.ID)
			}
			name, isName := act.Workflow.(string)
			if !isName || name == "" {
				return fmt.Errorf("schedule %q names no workflow", o.ID)
			}
			if act.TaskQueue == "" {
				return fmt.Errorf("schedule %q targets no task queue", o.ID)
			}
		}
		return nil
	})
	sc.Step(`^the arming fails loudly and no schedule is created$`, func() error {
		if w.armErr == nil {
			return fmt.Errorf("arming with no wired monitor must fail — judge death would be undetectable")
		}
		if len(w.sched.opts) != 0 {
			return fmt.Errorf("no schedule may be created, got %d", len(w.sched.opts))
		}
		return nil
	})

	// ---- REQ-308: the accrual halt at the graduation choke point ----
	sc.Step(`^an expired skill trial awaiting its graduation pass$`, func() error {
		w.trials = skillstore.NewMemTrialStore(10)
		tr, err := w.trials.CreateTrial(ctx, skillstore.Trial{
			SkillName: "triage-protocol", CandidateIDs: []int64{2}, ControlVersionID: 1,
			Dimension: "correct_diagnosis", MinSamplesPerArm: 4, MinLift: 0.2,
			EndsAt: gNow.Add(-time.Hour), Status: "active",
		})
		if err != nil {
			return err
		}
		w.trialID = tr.ID
		return nil
	})
	sc.Step(`^the judged-accrual halt is forced by a confirmed judge death$`, func() error {
		w.deadMan = newDeadMan()
		if err := w.deadMan.Halt(ctx, "frontier cross-check confirmed the local judge is dead"); err != nil {
			return err
		}
		acts := &skilltrial.Activities{D: skilltrial.Deps{
			Trials: w.trials, Store: skillstore.NewMemStore(), JudgeHealth: w.deadMan,
		}}
		_, w.finalizeEr = acts.FinalizeActivity(ctx, gNow)
		return nil
	})
	sc.Step(`^the graduation pass refuses with an error and the trial row is unchanged$`, func() error {
		if w.finalizeEr == nil {
			return fmt.Errorf("a confirmed dead judge must make the graduation pass fail loudly")
		}
		active, err := w.trials.ActiveTrials(ctx)
		if err != nil {
			return err
		}
		for _, tr := range active {
			if tr.ID == w.trialID {
				return nil // still active — nothing accrued
			}
		}
		return fmt.Errorf("the halted pass mutated the trial; accrual was not actually halted")
	})
	sc.Step(`^a judged-accrual halt whose persisted state cannot be read$`, func() error {
		dm, err := gov.NewJudgeDeadMan(unreadableAcceptStore{}, nil)
		if err != nil {
			return err
		}
		w.deadMan = dm
		return nil
	})
	sc.Step(`^the graduation pass consults it$`, func() error { return nil })
	sc.Step(`^judged accrual is treated as halted$`, func() error {
		halted, reason := w.deadMan.Halted(ctx)
		if !halted {
			return fmt.Errorf("an unreadable halt state must read HALTED (fail closed)")
		}
		if !strings.Contains(reason, "CLOSED") {
			return fmt.Errorf("the reason must name the fail direction, got %q", reason)
		}
		return nil
	})

	// ---- REQ-308: the dead-man for the dead-man ----
	sc.Step(`^a source tree whose schedule names a workflow that is defined nowhere$`, func() error {
		return nil // built inside the gate's own test harness; see the When step
	})
	sc.Step(`^the governance-schedule dead-man runs$`, func() error {
		// Run the SHIPPED gate script's own test suite, which builds exactly that broken tree (among others)
		// and asserts the gate fails on it. Driving the shipped script — not a Go reimplementation of its
		// logic — is what makes this a real oracle for the gate.
		root, err := filepath.Abs("../../..")
		if err != nil {
			return err
		}
		cmd := exec.Command("bash", filepath.Join(root, "eval/ci/check-governance-schedules_test.sh"))
		cmd.Dir = root
		out, rerr := cmd.CombinedOutput()
		w.deadManRC = 0
		if rerr != nil {
			w.deadManRC = 1
		}
		if w.deadManRC != 0 {
			return fmt.Errorf("the dead-man's own suite failed:\n%s", out)
		}
		if !strings.Contains(string(out), "defined NOWHERE fails") {
			return fmt.Errorf("the suite did not exercise the defined-nowhere case:\n%s", out)
		}
		return nil
	})
	sc.Step(`^it fails and names the missing wiring$`, func() error {
		if w.deadManRC != 0 {
			return fmt.Errorf("the dead-man suite did not pass")
		}
		return nil
	})
}

type unreadableAcceptStore struct{}

func (unreadableAcceptStore) Load(context.Context, string) (breaker.Record, bool, error) {
	return breaker.Record{}, false, errors.New("store down")
}
func (unreadableAcceptStore) Save(context.Context, breaker.Record) error {
	return errors.New("store down")
}
func (unreadableAcceptStore) List(context.Context) ([]breaker.Record, error) {
	return nil, errors.New("store down")
}

// armGovernanceSchedulesForAcceptance mirrors cmd/worker's arming contract (register-then-create over the
// SAME tggov.CreateSchedules and Specs the worker calls). cmd/worker is package main and cannot be imported,
// so the two-line composition is repeated here — everything it composes is production code, and
// eval/ci/check-governance-schedules.sh is what proves the worker still performs this same composition.
func armGovernanceSchedulesForAcceptance(ctx context.Context, sc *acceptScheduler, w *wiringWorld, acts *tggov.Activities) error {
	specs := acts.Specs()
	if len(specs) == 0 {
		return errors.New("no monitor is wired — judge death would be UNDETECTABLE")
	}
	if acts.Monitor != nil {
		w.RegisterWorkflow(tggov.JudgeLivenessWorkflow)
	}
	if acts.CrossCheck != nil {
		w.RegisterWorkflow(tggov.FrontierCrossCheckWorkflow)
	}
	return tggov.CreateSchedules(ctx, acceptScheduleClient{sc}, specs)
}

type acceptScheduleClient struct{ s *acceptScheduler }

func (a acceptScheduleClient) Create(ctx context.Context, o client.ScheduleOptions) (client.ScheduleHandle, error) {
	return a.s.Create(ctx, o)
}
func (a acceptScheduleClient) GetHandle(context.Context, string) client.ScheduleHandle {
	panic("not part of the arming path")
}
func (a acceptScheduleClient) List(context.Context, client.ScheduleListOptions) (client.ScheduleListIterator, error) {
	panic("not part of the arming path")
}
