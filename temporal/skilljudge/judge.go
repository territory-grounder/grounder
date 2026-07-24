// Package skilljudge is the durable session judge as a Temporal CRON workflow (task #26 / TG-37): the
// production path that scores every triage session on the five quality dimensions, asynchronously and
// read-only over the Runner's compact session_triage record (spec/012 REQ-1106). Its session_judgment
// rows are what the skill-store's live trials read (ArmScores / JudgedSessionRate, REQ-1306/1309) and
// what feeds the post-graduation regression watch (REQ-1310) — until this cron runs, trials honestly
// refuse to start (JudgedSessionRate=0). The judge semantics are the eval harness's, ported to
// core/judge and shared: ONE prompt, ONE parser, never two drifting copies.
//
// Same visible-scheduler-by-construction pattern as skilltrial: a Temporal cron shows last-run /
// next-run in the UI, and a missed run is an alarmed workflow, not a quiet nothing.
package skilljudge

import (
	"context"
	"fmt"
	"log"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// CronSchedule is the judge cadence — every 2 hours (minute 13, avoiding the top-of-hour crowd).
const CronSchedule = "13 */2 * * *"

// WorkflowID is the singleton cron id.
const WorkflowID = "tg/session-judge"

// JudgeWindow bounds how far back a session may be judged: older unjudged sessions stay unjudged
// (honest — judging a stale record against a moved estate scores noise, not quality).
const JudgeWindow = 48 * time.Hour

// BatchLimit bounds one run's model spend; the 2-hour cadence drains any backlog.
const BatchLimit = 50

// Store is the judge's persistence surface — the slice of db.TriageStore it needs, an interface so
// the CI oracles drive the batch with an in-memory fake (no Postgres in CI, constraint D5).
type Store interface {
	UnjudgedSince(ctx context.Context, window time.Duration, limit int) ([]judge.TriageRow, error)
	WriteJudgment(ctx context.Context, externalRef, dimension string, score float64, comment string) error
	MarkJudged(ctx context.Context, externalRef string) error
}

// Deps are the worker-side collaborators.
type Deps struct {
	// Model is the LLM gateway the judge adjudicates with (the same gateway the worker builds; a
	// scripted model in tests). The judge uses the "primary" reasoning ladder — one call per session,
	// quality over latency (the eval harness's choice, kept).
	Model agent.Completer
	Store Store
	// Watch + Skills + Ledger + Escalate feed the post-graduation regression watch
	// (skillstore.ObserveJudgedSession, REQ-1310). Watch nil ⇒ the watch feed is skipped.
	Watch    skillstore.WatchStore
	Skills   skillstore.Store
	Ledger   skillstore.Ledger
	Escalate skillstore.Escalator
}

// Activities carries Deps for registration.
type Activities struct{ D Deps }

// Outcome is the serializable run summary (the workflow result — visible in the Temporal UI).
type Outcome struct {
	Judged   int // sessions fully judged (all parsed dimensions written + marked)
	Skipped  int // sessions skipped this run (model/parse/write failure — retried next run)
	WatchFed int // judged sessions whose scores fed the regression watch
	Errors   []string
}

// JudgeBatchActivity drains one batch of unjudged sessions: for each, build the shared judge prompt
// from the compact record, call the model, parse defensively, write one session_judgment row per
// scored dimension, mark the session judged, then feed the regression watch with the per-dimension
// scores for every store version the session composed. Best-effort per session: one bad session is
// skipped (logged, retried next run) and never aborts the batch; only the batch READ failing is an
// activity error (the retry policy handles it).
func (a *Activities) JudgeBatchActivity(ctx context.Context, now time.Time) (Outcome, error) {
	rows, err := a.D.Store.UnjudgedSince(ctx, JudgeWindow, BatchLimit)
	if err != nil {
		return Outcome{}, err
	}
	var out Outcome
	for _, row := range rows {
		sc, jerr := a.judgeOne(ctx, row)
		if jerr != nil {
			out.Skipped++
			out.Errors = append(out.Errors, row.ExternalRef+": "+jerr.Error())
			log.Printf("session judge: %s skipped: %v (retried next run)", row.ExternalRef, jerr)
			continue
		}
		wrote := true
		for _, dim := range judge.Dimensions {
			v, scored := sc.Scores[dim]
			if !scored {
				continue // the judge omitted this dimension — no row is honest, never a fabricated score
			}
			if dim == judge.DimFalsifiablePrediction && !judge.PredictionApplicable(row.Facts()) {
				continue // N/A for a grounded stand-down — no action, no prediction to falsify (TG-61 seq C)
			}
			if werr := a.D.Store.WriteJudgment(ctx, row.ExternalRef, dim, float64(v), sc.Comment); werr != nil {
				// Leave the session unmarked so the next run re-judges it whole (the upsert makes the
				// partial writes harmless).
				out.Skipped++
				out.Errors = append(out.Errors, row.ExternalRef+": "+werr.Error())
				log.Printf("session judge: %s judgment write failed: %v (retried next run)", row.ExternalRef, werr)
				wrote = false
				break
			}
		}
		if !wrote {
			continue
		}
		if merr := a.D.Store.MarkJudged(ctx, row.ExternalRef); merr != nil {
			// The judgments are durable; the next run re-judges and upserts identically — record it.
			out.Errors = append(out.Errors, row.ExternalRef+": mark judged: "+merr.Error())
			log.Printf("session judge: %s mark-judged failed: %v (re-judged next run)", row.ExternalRef, merr)
		}
		out.Judged++

		// Feed the regression watch (REQ-1310): the store versions this session composed, matched by
		// each open watch on ITS trial's dimension. Best-effort — a watch feed failure is recorded and
		// never voids the judgment.
		ids := judge.StoreVersionIDs(row.SkillLoads)
		if a.D.Watch == nil || len(ids) == 0 {
			continue
		}
		scores := make(map[string]float64, len(sc.Scores))
		for d, v := range sc.Scores {
			if d == judge.DimFalsifiablePrediction && !judge.PredictionApplicable(row.Facts()) {
				continue // N/A for a stand-down — keep it out of the regression watch too (TG-61 seq C)
			}
			scores[d] = float64(v)
		}
		if werr := skillstore.ObserveJudgedSession(ctx, a.D.Watch, a.D.Skills, a.D.Ledger, a.D.Escalate,
			ids, scores, now); werr != nil {
			out.Errors = append(out.Errors, row.ExternalRef+": watch: "+werr.Error())
			log.Printf("session judge: %s watch feed failed: %v", row.ExternalRef, werr)
			continue
		}
		out.WatchFed++
	}
	return out, nil
}

// judgeOne adjudicates one compact record: the shared prompt, one model call on the canonical judge tier
// (JudgeParams.Model — the one source, same tier the eval harness/rejudge/shadowbench use), the shared
// defensive parser.
func (a *Activities) judgeOne(ctx context.Context, row judge.TriageRow) (judge.Score, error) {
	raw, err := a.D.Model.Complete(ctx, "session-judge", judge.DefaultParams().Model,
		[]model.Message{{Role: "user", Content: judge.Prompt(row.Facts())}})
	if err != nil {
		return judge.Score{}, fmt.Errorf("judge model: %w", err)
	}
	return judge.ParseScore(row.ExternalRef, raw)
}

// JudgeWorkflow is the 2-hourly cron body (distinct registered name — Temporal registers by BARE
// function name, and a second exported `Workflow` boot-loops the worker; see
// skilltrial.FinalizerWorkflow): one activity, workflow-time-stamped (workflow.Now — deterministic on
// replay). The activity timeout is sized for a full batch of reasoning-model judge calls (50 sessions
// with rate-limit failover can legitimately take many minutes — the eval harness's lesson). Two
// attempts: a transient DB/batch-read failure retries once; the next cron run drains whatever remains.
func JudgeWorkflow(ctx workflow.Context) (Outcome, error) {
	opts := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 2},
	}
	var out Outcome
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts),
		new(Activities).JudgeBatchActivity, workflow.Now(ctx).UTC()).Get(ctx, &out)
	return out, err
}
