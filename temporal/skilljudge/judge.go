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
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/estate"
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
	WriteJudgment(ctx context.Context, externalRef, dimension string, score float64, comment, rubricVersion string) error
	MarkJudged(ctx context.Context, externalRef string) error
}

// EstateReader is the judge's READ-ONLY view of the live causal estate graph — satisfied by *estate.Holder,
// so every batch reads the CURRENT snapshot and a topology refresh reaches the judge without a redeploy.
//
// It is an interface, and a narrow one, because the judge must never be able to write the estate: this whole
// path is an adjudication of a record and it stays read-only over the world (the graph is consulted, never
// touched). nil is a supported configuration — a deployment with no estate wired scores exactly as it did
// before TG-202, with the estate axis honestly N/A.
type EstateReader interface{ Graph() *estate.Graph }

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
	// Estate is the causal graph the estate_grounded dimension is computed against (TG-202). Without it the
	// judge scores a root cause with no access to the topology that decides whether the named cause can even
	// reach the symptom. nil ⇒ that one axis is N/A for every session (no row), and nothing else changes.
	Estate EstateReader
}

// Activities carries Deps for registration.
type Activities struct{ D Deps }

// Outcome is the serializable run summary (the workflow result — visible in the Temporal UI).
type Outcome struct {
	Judged   int // sessions fully judged (all parsed dimensions written + marked)
	Skipped  int // sessions skipped this run (model/parse/write failure — retried next run)
	WatchFed int // judged sessions whose scores fed the regression watch
	// EstateScored is how many sessions this batch actually got an estate_grounded row for (TG-202). It is
	// the axis's YIELD REGISTER: the dimension is N/A whenever the graph cannot place the alert host or the
	// claimed cause, which is correct and also indistinguishable — on a scorecard — from the graph never
	// having been wired at all. A run that judges 50 sessions and scores 0 of them topologically is visible
	// here, in the Temporal UI, instead of being a dimension nobody notices is missing.
	EstateScored int
	// EstateNA counts, per reason, why the estate axis declined to score. A permanently-N/A axis and an
	// axis that has simply not seen a qualifying session yet look identical from the row count alone —
	// measured 2026-08-05, this axis had written ZERO rows across 3,233 judged sessions and nothing said
	// which of its four gates was rejecting. TG-307 and TG-314 both wait on "once the axis accrues
	// samples", a precondition nobody could tell was unreachable.
	EstateNA map[string]int
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
	// ONE estate snapshot for the whole batch (TG-202): every session in this run is judged against the same
	// topology, so two records scored in the same batch can never disagree because a refresh landed between
	// them. nil when no estate is wired — the estate axis is then N/A throughout, which is exactly how this
	// cron behaved before the dimension existed.
	var estateGraph *estate.Graph
	if a.D.Estate != nil {
		estateGraph = a.D.Estate.Graph()
	}
	for _, row := range rows {
		// The record's facts, computed ONCE and shared by every scorer below. The estate block is attached
		// here, at the judging boundary, by traversal over the graph — it is not on the durable row and is
		// never read out of the session's own prose (the model authors nothing that grades it).
		facts := row.Facts()
		facts.Estate = judge.GroundInEstate(estateGraph, facts)
		sc, jerr := a.judgeOne(ctx, row)
		if jerr != nil {
			// FAIL LOUD on a model-breaker trip (TG-221 / audit finding #24). A per-session skip is the right
			// answer to a per-session fault (a bad record, an unparseable reply) — but an OPEN circuit is a
			// SYSTEMIC condition: every remaining row in this batch will short-circuit identically, so
			// continuing would burn the whole batch into a quiet `Skipped: 50` that reads like routine noise.
			// That quiet counter is exactly how a judge stays dead for weeks. Returning the error instead makes
			// the cron run RED in the Temporal UI, retried once by the workflow's policy, and leaves every row
			// unmarked so the next run re-judges it whole. No scorecard is ever fabricated or emptied here.
			if errors.Is(jerr, breaker.ErrOpen) {
				out.Skipped++
				out.Errors = append(out.Errors, row.ExternalRef+": "+jerr.Error())
				log.Printf("session judge: HALTING batch — the model circuit is OPEN (%v); %d session(s) judged, "+
					"%d left unjudged and UNMARKED for the next run", jerr, out.Judged, len(rows)-out.Judged)
				return out, fmt.Errorf("session judge: model circuit open — batch halted after %d/%d judged "+
					"(no session is silently marked judged): %w", out.Judged, len(rows), jerr)
			}
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
			if dim == judge.DimFalsifiablePrediction && !judge.PredictionApplicable(facts) {
				continue // N/A for a grounded stand-down — no action, no prediction to falsify (TG-61 seq C)
			}
			// The stamp is constant per batch: every row this loop writes was scored by the ONE
			// in-process rubric singleton (TG-194).
			if werr := a.D.Store.WriteJudgment(ctx, row.ExternalRef, dim, float64(v), sc.Comment, judge.RubricVersion()); werr != nil {
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
		// THE DETERMINISTIC AXIS (TG-201 part 1): score the session's typed diagnosis and write it beside the
		// model's five. It is computed HERE rather than asked of the judge model because every input —
		// whether an evidence ref matched an id the orchestrator really captured — is a fact this record
		// already carries, decided at bind time against something the model could not author (INV-11).
		//
		// Without this write the typed claim is decoration: an agent could state a root cause its own cited
		// evidence refutes, propose the action anyway, and no dimension anywhere would move. The N/A rule is
		// the same one falsifiable_prediction follows — a session the axis does not apply to gets NO row,
		// never a floored one, because a dimension floored across a whole population is what fired the
		// flywheel's Regressed trigger for every skill at once (TG-61 seq C).
		diagScore, diagWhy, diagApplies := judge.ScoreDiagnosis(facts)
		if diagApplies {
			if werr := a.D.Store.WriteJudgment(ctx, row.ExternalRef, judge.DimDiagnosisGrounded,
				float64(diagScore), diagWhy, judge.RubricVersion()); werr != nil {
				// Same failure direction as the loop above: leave the session UNMARKED so the next run
				// re-judges it whole (WriteJudgment upserts, so the partial writes are harmless).
				out.Skipped++
				out.Errors = append(out.Errors, row.ExternalRef+": "+werr.Error())
				log.Printf("session judge: %s diagnosis judgment write failed: %v (retried next run)", row.ExternalRef, werr)
				continue
			}
		}
		// THE SECOND DETERMINISTIC AXIS (TG-202): score the stated root cause against the CAUSAL ESTATE GRAPH.
		// Same reasoning as the diagnosis axis and one step further out — diagnosis_grounded checks the claim
		// against the session's OWN captured evidence, this checks it against the topology of the estate the
		// session ran on. It is the answer to "core/judge has zero estate references": until this write, a
		// diagnosis blaming a hypervisor the alerting guest does not run on scored exactly like a correct one.
		//
		// N/A whenever the graph does not KNOW (no estate wired, an unplaceable host, a stale or heuristic-only
		// endpoint) — no row, never a floored one. A thin graph must not mark every diagnosis impossible at once.
		estScore, estWhy, estApplies := judge.ScoreEstateGrounded(facts)
		if !estApplies {
			if out.EstateNA == nil {
				out.EstateNA = map[string]int{}
			}
			out.EstateNA[facts.Estate.NAReason()]++
		}
		if estApplies {
			if werr := a.D.Store.WriteJudgment(ctx, row.ExternalRef, judge.DimEstateGrounded,
				float64(estScore), estWhy, judge.RubricVersion()); werr != nil {
				out.Skipped++
				out.Errors = append(out.Errors, row.ExternalRef+": "+werr.Error())
				log.Printf("session judge: %s estate judgment write failed: %v (retried next run)", row.ExternalRef, werr)
				continue
			}
			out.EstateScored++
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
			if d == judge.DimFalsifiablePrediction && !judge.PredictionApplicable(facts) {
				continue // N/A for a stand-down — keep it out of the regression watch too (TG-61 seq C)
			}
			scores[d] = float64(v)
		}
		// The deterministic axis feeds the regression watch on the same terms as the model-scored ones: a
		// skill version that teaches the agent to bind its claims (or to stop asserting causes its evidence
		// refutes) must be able to show that as a measured improvement, and one that regresses it must be
		// caught. Omitted for a session the axis is N/A for, exactly as it was omitted from the durable row.
		if diagApplies {
			scores[judge.DimDiagnosisGrounded] = float64(diagScore)
		}
		// Same terms for the estate axis: a skill version that teaches the agent to name causes its own estate
		// graph can actually connect to the symptom must be able to show that as a measured improvement.
		if estApplies {
			scores[judge.DimEstateGrounded] = float64(estScore)
		}
		if werr := skillstore.ObserveJudgedSession(ctx, a.D.Watch, a.D.Skills, a.D.Ledger, a.D.Escalate,
			ids, scores, now); werr != nil {
			out.Errors = append(out.Errors, row.ExternalRef+": watch: "+werr.Error())
			log.Printf("session judge: %s watch feed failed: %v", row.ExternalRef, werr)
			continue
		}
		out.WatchFed++
	}
	// REPORT WHY THE ESTATE AXIS DECLINED. A run that judged 50 sessions and wrote 0 estate rows is
	// indistinguishable from a healthy quiet one unless the reason is said out loud — and it stayed
	// indistinguishable for 3,233 sessions.
	if len(out.EstateNA) > 0 {
		reasons := make([]string, 0, len(out.EstateNA))
		for r, n := range out.EstateNA {
			reasons = append(reasons, fmt.Sprintf("%s=%d", r, n))
		}
		sort.Strings(reasons) // deterministic: a line that reorders between runs is one nobody diffs
		log.Printf("session judge: estate_grounded scored %d/%d — N/A breakdown: %s",
			out.EstateScored, out.Judged, strings.Join(reasons, " "))
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
