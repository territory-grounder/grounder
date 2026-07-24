// Package skillgen is the CREATION HALF of the graduation flywheel as a Temporal CRON workflow (spec/014
// REQ-1314): the daily generate -> offline-admit -> trial-start cycle the flywheel had no production
// caller for. GenerateCandidates/AdmitToTrial/StartTrial existed but nothing fired them, so no candidate
// was ever generated, admitted, or trialed; this cron closes that gap. Same visible-scheduler-by-
// construction pattern as skilltrial/skilljudge — a Temporal cron shows last-run/next-run in the UI and
// a missed run is an alarmed workflow, not a quiet nothing.
//
// It is GENERATE-ONLY and COMPETENCE-plane: it lands draft rows, runs the offline gate, and starts
// online A/B trials over agent PROMPT CONTENT through the audited draft->trial->production state machine.
// Nothing here mutates the estate and mutation_enabled is untouched (INV-08) — no model output becomes
// control flow (the drafts are data rows; the audited Transition is the only status mutator).
package skillgen

import (
	"context"
	"log"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// CronSchedule is the daily generation cadence — 04:37, off-peak and on a DIFFERENT minute/hour than the
// finalizer (03:17) and the judge (every 2h at :13) so the three crons never collide on the worker.
const CronSchedule = "37 4 * * *"

// WorkflowID is the singleton cron id.
const WorkflowID = "tg/skill-generator"

// Deps are the worker-side collaborators — the whole creation-half cycle is expressed as the
// skillstore.CreationDeps the durable stores + gateway satisfy.
type Deps struct {
	Creation skillstore.CreationDeps
}

// Activities carries Deps for registration.
type Activities struct{ D Deps }

// Outcome is the serializable run summary (the workflow result — visible in the Temporal UI).
type Outcome struct {
	Generated     int
	Admitted      int
	RefusedAdmit  int
	TrialsStarted int
	RefusedStart  int
	Skipped       int
	// DeferredGen/DeferredAdmit surface the TG-63 per-run caps in the Temporal UI: work that regressed /
	// awaits admission but was bounded out of THIS run and drains on later runs (not dropped).
	DeferredGen   int
	DeferredAdmit int
	// AdmitSkippedAtCap surfaces the TG-65 arm cap in the Temporal UI: drafts held because their skill is
	// already at the admitted-candidate cap with no trial draining it (reconsidered once a trial drains).
	AdmitSkippedAtCap int
	Errors            []string
}

// GenerateActivity runs one generate -> offline-admit -> trial-start cycle (skillstore.RunCreationHalf).
// The run id is stamped from the activity clock so each cycle's generated drafts carry a traceable
// provenance in their source. Best-effort throughout: a per-skill/per-draft failure is recorded in the
// report and never aborts the cycle; only failing to LIST the production set is an activity error (the
// retry policy handles it).
func (a *Activities) GenerateActivity(ctx context.Context, now time.Time) (Outcome, error) {
	d := a.D.Creation
	d.RunID = now.UTC().Format("20060102T150405")
	d.Log = func(format string, args ...any) { log.Printf("skillgen: "+format, args...) }
	rep, err := skillstore.RunCreationHalf(ctx, d, now)
	if err != nil {
		return Outcome{}, err
	}
	log.Printf("skillgen: cycle complete — %d generated, %d admitted (%d refused offline), %d trial(s) started (%d refused starvation), %d deduped, %d gen-deferred, %d admit-deferred, %d held-at-cap, %d error(s) — generate-only, mutation OFF",
		rep.Generated, rep.Admitted, rep.RefusedAdmit, rep.TrialsStarted, rep.RefusedStart, rep.Skipped, rep.DeferredGen, rep.DeferredAdmit, rep.AdmitSkippedAtCap, len(rep.Errors))
	return Outcome{
		Generated: rep.Generated, Admitted: rep.Admitted, RefusedAdmit: rep.RefusedAdmit,
		TrialsStarted: rep.TrialsStarted, RefusedStart: rep.RefusedStart, Skipped: rep.Skipped,
		DeferredGen: rep.DeferredGen, DeferredAdmit: rep.DeferredAdmit, AdmitSkippedAtCap: rep.AdmitSkippedAtCap,
		Errors: rep.Errors,
	}, nil
}

// GeneratorWorkflow is the daily cron body (DISTINCT registered name — Temporal registers by BARE
// function name, and a second exported `Workflow` boot-loops the worker; see skilltrial.FinalizerWorkflow
// and skilljudge.JudgeWorkflow, and the finalizer_names_test.go collision guard, which registers THIS
// workflow too). One activity, workflow-time-stamped (workflow.Now — deterministic on replay). The
// timeout is sized for a full offline-gate pass (each admitted draft scores candidate vs production over
// several incidents with reasoning-model judge calls). Two attempts: a transient DB/model failure
// retries once; the next cron run reconsiders.
func GeneratorWorkflow(ctx workflow.Context) (Outcome, error) {
	opts := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 2},
	}
	var out Outcome
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts),
		new(Activities).GenerateActivity, workflow.Now(ctx).UTC()).Get(ctx, &out)
	return out, err
}

// PromptCompleter adapts the message-based model gateway to skillstore.Completer (the single-prompt
// surface GenerateCandidates uses): it wraps the one generation prompt as a single user message.
type PromptCompleter struct{ M agent.Completer }

// NewPromptCompleter wraps a gateway as a skillstore.Completer.
func NewPromptCompleter(m agent.Completer) skillstore.Completer { return PromptCompleter{M: m} }

// Complete implements skillstore.Completer.
func (p PromptCompleter) Complete(ctx context.Context, user, modelName, prompt string) (string, error) {
	return p.M.Complete(ctx, user, modelName, []model.Message{{Role: "user", Content: prompt}})
}
