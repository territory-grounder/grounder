// Package skilltrial is the trial finalizer as a Temporal CRON workflow (spec/014 REQ-1308) — visible
// scheduler state by construction. The predecessor's finalizer was a Cronicle event that got silently
// disabled behind a crontab of inert #CRONICLE# comments (scheduler split-brain); a Temporal cron shows
// last-run/next-run in the UI and on the console, and a missed run is an alarmed workflow, not a quiet
// nothing.
package skilltrial

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// CronSchedule is the daily finalize cadence (the predecessor ran 03:17; the exact minute is not
// load-bearing, off-peak is).
const CronSchedule = "17 3 * * *"

// WorkflowID is the singleton cron id.
const WorkflowID = "tg/skill-trial-finalizer"

// Deps are the worker-side collaborators.
type Deps struct {
	Trials skillstore.TrialStore
	Store  skillstore.Store
	Ledger skillstore.Ledger
	// Watch arms the post-graduation regression watch (REQ-1310) after each successful graduation: the
	// judge cron's ObserveJudgedSession feed compares the graduate's judged sessions against the
	// trial's control mean on the trial's dimension. nil ⇒ no watch is armed (the no-DB oracle path).
	Watch skillstore.WatchStore
}

// Activities carries Deps for registration.
type Activities struct{ D Deps }

// Outcome is the serializable run summary (the workflow result — visible in the Temporal UI).
type Outcome struct {
	Swept     int // aborted_timeout
	Graduated int // completed → version transitioned to production
	Watched   int // regression watches armed for graduates (REQ-1310)
	NoWinner  int
	StillOpen int
	Errors    []string
}

// FinalizeActivity runs the sweep-then-decide pass and GRADUATES each completed trial's winner through
// the single audited Transition (structural supersede + ledger, REQ-1301/1302); losing candidates are
// transitioned to rejected. A per-trial error is recorded and does not abort the rest of the run.
// After a successful graduation the regression watch is armed (REQ-1310): the trial's control-arm mean
// (recomputed from ArmScores) + dimension + the production version the supersede retires — captured
// BEFORE Transition, since afterwards the graduate IS the production version.
func (a *Activities) FinalizeActivity(ctx context.Context, now time.Time) (Outcome, error) {
	// Snapshot the active trials BEFORE finalize: a completed outcome's trial row is terminal after
	// FinalizeTrials, and the watch needs its skill name, dimension and lift.
	active := map[int64]skillstore.Trial{}
	if trials, err := a.D.Trials.ActiveTrials(ctx); err == nil {
		for _, tr := range trials {
			active[tr.ID] = tr
		}
	}
	outcomes, err := skillstore.FinalizeTrials(ctx, a.D.Trials, now)
	if err != nil {
		return Outcome{}, err
	}
	var out Outcome
	for _, o := range outcomes {
		switch o.Status {
		case "aborted_timeout":
			out.Swept++
		case "aborted_no_winner":
			out.NoWinner++
		case "active":
			out.StillOpen++
		case "completed":
			// The version this graduation retires (the watch's restore target) — read BEFORE Transition.
			tr, known := active[o.TrialID]
			var priorID int64
			if known {
				if prev, ok, perr := a.D.Store.ProductionVersion(ctx, tr.SkillName); perr == nil && ok {
					priorID = prev.ID
				}
			}
			if _, terr := skillstore.Transition(ctx, a.D.Store, a.D.Ledger, o.WinnerID,
				skillstore.StatusProduction, "trial graduation — "+o.Reason); terr != nil {
				// The trial row is already terminal (completed) with its winner recorded; a refused
				// graduation (winner meanwhile retired/rejected, ledger error) strands ONLY that
				// version — recovery is the operator promote route with the trial's stats as the
				// rationale (visible: the console shows completed-without-production).
				out.Errors = append(out.Errors, terr.Error())
				continue
			}
			out.Graduated++
			// Arm the regression watch — best-effort: a watch failure never revokes the graduation, it
			// is recorded (an unwatched graduate is visible in the run summary, never a quiet nothing).
			if a.D.Watch == nil || !known {
				continue
			}
			scores, serr := a.D.Trials.ArmScores(ctx, o.TrialID)
			if serr != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("watch for trial %d: arm scores: %v", o.TrialID, serr))
				continue
			}
			control := scores[-1]
			var controlMean float64
			for _, v := range control {
				controlMean += v
			}
			if len(control) > 0 {
				controlMean /= float64(len(control))
			}
			if werr := skillstore.OpenWatch(ctx, a.D.Watch, o.WinnerID, priorID, tr.SkillName,
				tr.Dimension, controlMean, tr.MinLift, now); werr != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("watch for trial %d: open: %v", o.TrialID, werr))
				continue
			}
			out.Watched++
		}
	}
	return out, nil
}

// FinalizerWorkflow is the daily cron body (distinct registered name — see skillwrite.TransitionWorkflow): one activity, workflow-time-stamped (workflow.Now — deterministic on
// replay). Two attempts: a transient DB failure retries once; a refusal is terminal for the day (the
// next cron run reconsiders).
func FinalizerWorkflow(ctx workflow.Context) (Outcome, error) {
	opts := workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 2},
	}
	var out Outcome
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts),
		new(Activities).FinalizeActivity, workflow.Now(ctx).UTC()).Get(ctx, &out)
	return out, err
}
