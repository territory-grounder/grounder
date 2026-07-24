package skillstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The CREATION HALF of the graduation flywheel (spec/014 REQ-1314) — the production caller the
// generate -> offline-admit -> trial-start machinery lacked. GenerateCandidates (REQ-1312), AdmitToTrial
// (REQ-1307) and StartTrial (REQ-1309) are the mechanisms; RunCreationHalf is the daily orchestration
// that fires them from the durable judge signal (session_judgment rolling means). It is GENERATE-ONLY
// and COMPETENCE-plane: it lands draft rows, runs the offline gate, and starts online A/B trials —
// nothing here mutates the estate and mutation_enabled is untouched (INV-08). Every status change still
// passes through the single audited Transition; a draft has no effect on composition until it clears the
// offline gate AND wins a statistical trial.

// AuthorFlywheel marks the rows the creation half creates — the generator's drafts and their trial
// lineage. It is the dedup/selection key the orchestration filters on (generate.go stamps the same
// literal on every candidate it lands).
const AuthorFlywheel = "flywheel"

// genSourcePrefix is the eval-failure generation source's fixed prefix; GenSource appends the target
// dimension and the run id so the admission phase recovers the dimension a draft was generated to
// improve without re-deriving it.
const genSourcePrefix = "flywheel:eval-failure:"

// DefaultGenThreshold — a judged dimension whose rolling mean falls below this is a generation trigger.
const DefaultGenThreshold = 3.5

// DefaultGenMinSamples — generation is evidence-gated: it never fires on fewer than this many judged
// samples (thin data is noise, not a regression).
const DefaultGenMinSamples = 5

// DefaultMaxGenSkillsPerRun bounds how many regressed skills the creation half DRAFTS for in one run
// (TG-63). A judge dimension can floor GLOBALLY (e.g. falsifiable_prediction ~1.1 across every skill's
// sessions), which makes Regressed fire for every non-pinned production skill at once. Without this cap
// generate drafts 3-lens candidates for ALL of them in a single Temporal activity, and the admit phase
// then offline-scores every one of those drafts with reasoning-model judge calls — far past the activity
// budget, so the run times out having admitted/started nothing. Bounded to the WORST-regressed K per run;
// the daily cron drains the ranked backlog across runs (default 1 — one worst-regressed skill per run).
const DefaultMaxGenSkillsPerRun = 1

// DefaultMaxAdmitPerRun bounds how many open flywheel DRAFTS the offline admission gate processes in one
// run (TG-63). Each admit offline-scores candidate-vs-production with reasoning-model judge calls, so an
// unbounded admit-all over a large backlog (17 drafts observed live) exceeds the activity timeout. Bounded
// to the OLDEST J per run (deterministic order); the daily cron drains the backlog across runs (default 3).
const DefaultMaxAdmitPerRun = 3

// DefaultMaxCandidatesPerTrial bounds how many admitted candidates a single trial pits against control as
// ARMS (TG-65; <=0 ⇒ this default). The follow-up to TG-63: TG-63 capped admit to ≤3 drafts/run, but the
// START phase still armed a trial with EVERY admitted candidate for a skill, so the admitted-but-not-yet-
// trialed set grew every run and StartTrial's traffic gate (MinSamplesPerArm × (1+arms) judged sessions
// before the end date) needed an ever-rising sample count — a 3-arm trial at bootstrap traffic needed ~45
// sessions at ~1/day and refused forever. Bounded to control + the TOP-K admitted candidates by offline
// discovery-delta; the rest wait for a later trial (a skill trials in WAVES, not one mega-trial). Default 1
// — a clean 2-arm control-vs-candidate trial, which clears the traffic gate at bootstrap traffic.
const DefaultMaxCandidatesPerTrial = 1

// DimensionStat is one judge dimension's rolling judged mean + sample count for a production version
// over the trailing window — the regressed-dimension input (REQ-1314). It is DATA about measurements;
// no model token participates (INV-08).
type DimensionStat struct {
	Dimension string
	Mean      float64
	Samples   int
}

// GenSource builds the generation source carrying the target dimension and the run id.
func GenSource(dimension, runID string) string { return genSourcePrefix + dimension + ":" + runID }

// dimensionFromSource recovers the target dimension from a source built by GenSource. Empty when the
// source is not a flywheel eval-failure source (a resolved-incident lesson draft, say) — such a draft is
// not admitted by this cron because an honest offline gate needs a known target dimension.
func dimensionFromSource(src string) string {
	if !strings.HasPrefix(src, genSourcePrefix) {
		return ""
	}
	rest := strings.TrimPrefix(src, genSourcePrefix)
	if i := strings.LastIndex(rest, ":"); i > 0 {
		return rest[:i]
	}
	return ""
}

// Regressed returns the WORST-scoring dimension whose rolling mean fell below threshold with at least
// minSamples judged samples. ok=false when every measured dimension is healthy or under-sampled — the
// generator stays idle on thin or healthy data (never fires on noise).
func Regressed(stats []DimensionStat, threshold float64, minSamples int) (DimensionStat, bool) {
	var worst DimensionStat
	found := false
	for _, s := range stats {
		if s.Samples < minSamples || s.Mean >= threshold {
			continue
		}
		if !found || s.Mean < worst.Mean {
			worst, found = s, true
		}
	}
	return worst, found
}

// minTrialArms is the smallest trial the flywheel runs for a dimension: control + one candidate. Dimension
// fillability is projected against THIS minimal trial — TG-65 already caps a trial's arm count to the top-K
// admitted candidates (default one), so if even a 2-arm trial cannot fill the dimension, no trial can, and
// if it can, the operator's arm cap governs the actual trial's size separately. Projecting the minimal
// trial keeps dimension selection decoupled from MaxCandidatesPerTrial (a large cap must not make an
// otherwise-fillable dimension read as unfillable).
const minTrialArms = 2

// DefaultFillHorizon is the default steering-projection cap (CreationConfig.FillHorizon): a dimension a
// minimal trial can complete on within ~2 weeks is a practical steer target; a dimension that only "fills"
// over a much longer TrialDuration is proposer-only-sparse and would just timeout-abort. TG-67 follow-up.
const DefaultFillHorizon = 14 * 24 * time.Hour

// dimensionFillsTrial projects whether the minimal (2-arm) trial on this dimension can reach its per-arm
// sample minimum on EVERY arm before the trial window closes, at the dimension's OBSERVED scored-sample
// rate (TG-67). The rate is stat.Samples over cfg.Window — and DimensionMeans counts only sessions that
// actually SCORED the dimension (score>0), so for falsifiable_prediction, seq C means only PROPOSING
// sessions count and its rate reflects the true proposal supply, not overall traffic. Every judged scored
// session is assigned across the arms, so completion needs MinSamplesPerArm × arms scored sessions. Pure —
// a fill projection over measurement data, no model token participates (INV-08). Missing config or zero
// supply ⇒ not fillable (fail closed).
func dimensionFillsTrial(stat DimensionStat, cfg CreationConfig) bool {
	need := float64(cfg.MinSamplesPerArm) * float64(minTrialArms)
	winHours := cfg.Window.Hours()
	// Project fillability over the STEERING horizon, not the full TrialDuration. An over-long TrialDuration
	// (set to clear StartTrial's traffic bar on a data-starved estate) would otherwise make even a
	// proposer-only-sparse dimension read as fillable and defeat the anti-starvation steering — the loop then
	// chases an unmovable dimension (e.g. falsifiable_prediction, only proposing sessions score it) and only
	// timeout-aborts. Cap at FillHorizon so steering stays on dimensions a trial can PRACTICALLY complete on.
	durHours := cfg.TrialDuration.Hours()
	if fh := cfg.FillHorizon.Hours(); fh > 0 && fh < durHours {
		durHours = fh // FillHorizon<=0 keeps TrialDuration (behaviour-preserving)
	}
	if need <= 0 || winHours <= 0 || durHours <= 0 || stat.Samples <= 0 {
		return false
	}
	ratePerHour := float64(stat.Samples) / winHours
	projectedHours := need / ratePerHour
	return projectedHours <= durHours
}

// FillableRegression is Regressed made FILL-AWARE (TG-67): it returns the worst-scoring regressed
// dimension that can still fill a trial before its window closes (dimensionFillsTrial), and reports
// whether a strictly-worse regression was passed over because it is proposer-only-sparse (skipped, for
// honest logging). A dimension can floor the judged mean (falsifiable_prediction ~1.1) yet be scored by so
// few sessions — after seq C, ONLY proposing sessions score it — that a trial on it can never reach its
// sample minimum on mostly-stand-down traffic and only ever timeout-aborts. Targeting such a dimension
// burns the skill's single trial slot and completes nothing: the predecessor's zero-trials-in-three-months
// starvation, reintroduced through a proposer-only trigger dimension. So the flywheel instead targets the
// worst dimension it can ACTUALLY complete a trial on — typically a DENSE dimension every judged session
// scores (evidence_grounded, correct_diagnosis), which the abundant grounded-stand-down traffic fills. When
// NO regressed dimension can fill, ok=false and the skill waits for a later run (traffic may rise); skipped
// still reports that a real regression exists so it is visible. Pure — no model token participates (INV-08).
func FillableRegression(stats []DimensionStat, cfg CreationConfig) (worst DimensionStat, ok bool, skipped bool) {
	var reg []DimensionStat
	for _, s := range stats {
		if s.Samples < cfg.MinSamples || s.Mean >= cfg.Threshold {
			continue
		}
		reg = append(reg, s)
	}
	// Worst-first: lowest mean is the most severe regression; deterministic dimension-name tie-break so the
	// same means always select the same target (pure/replay-safe).
	sort.Slice(reg, func(i, j int) bool {
		if reg[i].Mean != reg[j].Mean {
			return reg[i].Mean < reg[j].Mean
		}
		return reg[i].Dimension < reg[j].Dimension
	})
	for _, s := range reg {
		if dimensionFillsTrial(s, cfg) {
			return s, true, skipped
		}
		skipped = true // a worse-or-equal regression exists but its scored supply can't fill a trial
	}
	return DimensionStat{}, false, skipped
}

// MeansReader supplies the rolling per-dimension judged means for a production version — the judge
// spine's session_judgment joined to the composing session's provenance (pgx in production, a fake in
// the oracles). It is a SEPARATE collaborator from the skill store: the means come from the judge
// spine's data, not the version rows.
type MeansReader interface {
	DimensionMeans(ctx context.Context, versionID int64, window time.Duration) ([]DimensionStat, error)
}

// FlywheelStore is the creation half's skill-store surface: the Store the generator lands drafts on plus
// the version-derived reads the orchestration needs. Every read is a pure function of the skill_version
// rows (pgx queries in production; MemStore derives them from its map), so the oracle and the database
// enforce the same shape.
type FlywheelStore interface {
	Store
	CreateVersion(ctx context.Context, v Version) (Version, error)
	// ProductionVersions lists the current production version of every skill (one per skill).
	ProductionVersions(ctx context.Context) ([]Version, error)
	// OpenCandidates counts the OPEN (draft or trial) flywheel candidates parented on a production
	// version — the generator's dedup so a pending batch is never piled onto.
	OpenCandidates(ctx context.Context, parentVersionID int64) (int, error)
	// FlywheelDrafts lists the open flywheel DRAFT versions awaiting the offline admission gate.
	FlywheelDrafts(ctx context.Context) ([]Version, error)
	// AdmittedCandidates lists the flywheel TRIAL-status versions parented on a production version
	// (offline-passed, awaiting an active trial).
	AdmittedCandidates(ctx context.Context, parentVersionID int64) ([]Version, error)
}

// CreationConfig parameterizes the creation-half cron (env-driven in the worker, defaults in tests).
type CreationConfig struct {
	Threshold        float64       // a dimension mean below this fires generation (default 3.5)
	MinSamples       int           // minimum judged samples before generation fires (default 5)
	Window           time.Duration // the rolling means window
	MinSamplesPerArm int           // a started trial's per-arm sample minimum
	MinLift          float64       // a started trial's required lift over control
	PThreshold       float64       // a started trial's Welch significance threshold
	TrialDuration    time.Duration // how long a started trial runs before the timeout sweep
	// FillHorizon bounds the STEERING projection (dimensionFillsTrial): the flywheel targets a regressed
	// dimension a trial can PRACTICALLY complete on within THIS horizon, NOT the full TrialDuration. An
	// over-long TrialDuration (set to clear StartTrial's traffic bar on a data-starved estate — e.g. 50d)
	// would otherwise make even a proposer-only-sparse dimension project as "fillable" and DEFEAT the
	// anti-starvation steering (the loop then chases an unmovable dimension and only timeout-aborts). <=0 ⇒
	// falls back to TrialDuration (behaviour-preserving); default DefaultFillHorizon (TG-67 follow-up).
	FillHorizon time.Duration
	// MaxGenSkillsPerRun bounds how many worst-regressed skills generate DRAFTS for per run (TG-63; <=0 ⇒
	// DefaultMaxGenSkillsPerRun). A global-low dimension must not flood every skill in one activity.
	MaxGenSkillsPerRun int
	// MaxAdmitPerRun bounds how many OLDEST open flywheel drafts admit offline-scores per run (TG-63; <=0 ⇒
	// DefaultMaxAdmitPerRun). Keeps admit-all inside the activity budget regardless of backlog size.
	MaxAdmitPerRun int
	// MaxCandidatesPerTrial bounds a started trial's ARM count to control + the top-K admitted candidates by
	// offline discovery-delta (TG-65; <=0 ⇒ DefaultMaxCandidatesPerTrial). It also caps how many admitted
	// candidates the admit phase lets accumulate on one production version, so the arm count — and thus
	// StartTrial's traffic bar — can never grow without bound and starve the start forever.
	MaxCandidatesPerTrial int
}

// CreationDeps are the creation half's collaborators.
type CreationDeps struct {
	Store  FlywheelStore
	Means  MeansReader
	Trials TrialStore
	Ledger Ledger
	Model  Completer
	Runner OfflineRunner
	Cfg    CreationConfig
	RunID  string                           // stamped into the generation source for provenance
	Log    func(format string, args ...any) // honest per-step logging; nil = discard
}

func (d CreationDeps) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log(format, args...)
	}
}

// CreationReport is the run summary (Temporal-visible; logged honestly).
type CreationReport struct {
	Generated     int
	Admitted      int
	RefusedAdmit  int
	TrialsStarted int
	RefusedStart  int
	Skipped       int
	// DeferredGen counts regressed skills that needed NEW drafting this run but exceeded
	// MaxGenSkillsPerRun (TG-63) — not dropped, reconsidered next run (the daily cron drains the backlog).
	DeferredGen int
	// DeferredAdmit counts open flywheel drafts left unadmitted this run by MaxAdmitPerRun (TG-63) —
	// not dropped, processed oldest-first on later runs.
	DeferredAdmit int
	// AdmitSkippedAtCap counts drafts held back this run because their production version already holds
	// MaxCandidatesPerTrial admitted-but-not-yet-trialed candidates and no active trial is draining them
	// (TG-65) — admitting more would only grow a later trial's arm count past what StartTrial can complete.
	// They stay draft and are reconsidered once a trial drains the admitted set.
	AdmitSkippedAtCap int
	Errors            []string
}

// RunCreationHalf runs the daily generate -> offline-admit -> trial-start cycle over every non-pinned
// production skill. Each phase is best-effort per skill/draft: one failure is recorded in the report and
// never aborts the rest of the run. Only listing the production set is fatal to the run (there is
// nothing to iterate). It is generate-only throughout — the audited Transition is the sole status
// mutator and estate mutation is never reachable from here.
func RunCreationHalf(ctx context.Context, d CreationDeps, now time.Time) (CreationReport, error) {
	var rep CreationReport
	prods, err := d.Store.ProductionVersions(ctx)
	if err != nil {
		return rep, fmt.Errorf("creation-half: list production versions: %w", err)
	}
	d.generate(ctx, prods, &rep)
	d.admit(ctx, &rep)
	d.start(ctx, prods, now, &rep)
	return rep, nil
}

// generate is phase 1: per non-pinned production skill, if a judged dimension regressed (below threshold
// with enough samples) and no open candidate is already pending on this version, ask GenerateCandidates
// for draft rewrites. Generate-only — the drafts have no effect until admitted and trialed.
//
// BOUNDED PER RUN (TG-63): a dimension that floors GLOBALLY makes every non-pinned skill regress at once,
// so drafting for all of them in one activity (and then offline-scoring every draft in admit) blows the
// activity budget. The regressed skills are ranked worst-first and at most MaxGenSkillsPerRun of them are
// drafted this run; the rest are DEFERRED (not dropped) — the daily cron drains the backlog worst-first.
// The dedup skip (a batch already pending on a version) is unchanged and does not consume a draft slot.
func (d CreationDeps) generate(ctx context.Context, prods []Version, rep *CreationReport) {
	maxSkills := d.Cfg.MaxGenSkillsPerRun
	if maxSkills <= 0 {
		maxSkills = DefaultMaxGenSkillsPerRun
	}
	// Pass 1: collect every non-pinned production skill whose worst judged dimension regressed. Under a
	// global-low dimension this set can be the entire production surface — hence the cap below.
	type regression struct {
		prod Version
		dim  DimensionStat
	}
	var regressed []regression
	for _, prod := range prods {
		sk, err := d.Store.GetSkill(ctx, prod.SkillName)
		if err != nil {
			rep.Errors = append(rep.Errors, "get skill "+prod.SkillName+": "+err.Error())
			continue
		}
		if sk.Pinned {
			continue // the floor is never a generation target (REQ-1305)
		}
		stats, err := d.Means.DimensionMeans(ctx, prod.ID, d.Cfg.Window)
		if err != nil {
			rep.Errors = append(rep.Errors, "means "+prod.SkillName+": "+err.Error())
			continue
		}
		dim, ok, skipped := FillableRegression(stats, d.Cfg)
		if !ok {
			if skipped {
				// A real regression exists but only on a dimension whose recent scored-sample supply can't
				// fill a trial (proposer-only-sparse, e.g. falsifiable_prediction after seq C, which only
				// proposing sessions score). Targeting it would arm a trial that can only timeout-abort and
				// burn the skill's single trial slot (TG-67) — so this run leaves the skill for later, when
				// proposal traffic may rise. Visible, not silent.
				d.logf("flywheel generate: %s regressed only on a proposer-only dimension with too little scored traffic to complete a trial — no fillable dimension to target this run (waits for traffic)",
					prod.SkillName)
			}
			continue
		}
		regressed = append(regressed, regression{prod: prod, dim: dim})
	}
	// Rank worst-first: the LOWEST dimension mean is the most severe regression. Deterministic — ties break
	// on skill name so the same means always select the same worst skill (pure/replay-safe).
	sort.Slice(regressed, func(i, j int) bool {
		if regressed[i].dim.Mean != regressed[j].dim.Mean {
			return regressed[i].dim.Mean < regressed[j].dim.Mean
		}
		return regressed[i].prod.SkillName < regressed[j].prod.SkillName
	})
	// Pass 2: draft for at most maxSkills of the worst, honoring the dedup skip.
	drafted := 0
	for _, r := range regressed {
		prod, dim := r.prod, r.dim
		open, err := d.Store.OpenCandidates(ctx, prod.ID)
		if err != nil {
			rep.Errors = append(rep.Errors, "open-candidates "+prod.SkillName+": "+err.Error())
			continue
		}
		if open > 0 {
			// Dedup: a batch is already pending admission/trial for this production version — do not pile
			// another on (idempotent across daily runs). Already-in-flight work, so it takes no draft slot.
			rep.Skipped++
			d.logf("flywheel generate: %s %s below %.2f (mean %.2f/%d) but %d candidate(s) already open — dedup, not regenerating",
				prod.SkillName, dim.Dimension, d.Cfg.Threshold, dim.Mean, dim.Samples, open)
			continue
		}
		if drafted >= maxSkills {
			// TG-63 cap: this skill regressed and needs drafting, but a worse-regressed skill already took
			// the run's draft budget. DEFERRED, not dropped — the next cron run reconsiders it.
			rep.DeferredGen++
			d.logf("flywheel generate: %s %s mean %.2f/%d below %.2f — DEFERRED (drafted the worst %d skill(s) this run; drains next run)",
				prod.SkillName, dim.Dimension, dim.Mean, dim.Samples, d.Cfg.Threshold, maxSkills)
			continue
		}
		trig := GenTrigger{
			SkillName: prod.SkillName, Dimension: dim.Dimension, Mean: dim.Mean,
			Threshold: d.Cfg.Threshold, Window: dim.Samples, Source: GenSource(dim.Dimension, d.RunID),
		}
		drafts, err := GenerateCandidates(ctx, d.Store, d.Model, trig)
		if err != nil {
			if errors.Is(err, ErrPinnedSkill) {
				continue
			}
			rep.Errors = append(rep.Errors, "generate "+prod.SkillName+": "+err.Error())
			continue
		}
		drafted++
		rep.Generated += len(drafts)
		d.logf("flywheel generate: %s %s mean %.2f/%d below %.2f — %d draft candidate(s) (generate-only, no effect until admitted+trialed)",
			prod.SkillName, dim.Dimension, dim.Mean, dim.Samples, d.Cfg.Threshold, len(drafts))
	}
	if rep.DeferredGen > 0 {
		d.logf("flywheel generate: %d skill(s) regressed and need drafting; drafted the worst %d this run, %d deferred to a later run (a global-low dimension must not flood every skill at once — the daily cron drains the backlog worst-first)",
			drafted+rep.DeferredGen, drafted, rep.DeferredGen)
	}
}

// admit is phase 2: run the offline gate on the open flywheel drafts. A pass transitions a draft->trial
// (through the audited AdmitToTrial); a refusal keeps it a draft with the refusal stored on its row. The
// gate never reads the sealed holdout (the OfflineRunner interface cannot express one).
//
// BOUNDED PER RUN (TG-63): each admit offline-scores candidate-vs-production with reasoning-model judge
// calls, so admitting an unbounded backlog in one activity (17 drafts observed live) blows the activity
// budget and the run times out having admitted nothing. FlywheelDrafts returns ascending id == creation
// order, so processing at most MaxAdmitPerRun of them is deterministic OLDEST-first; the rest are DEFERRED
// (not dropped) and drained by later runs. Drafts with no recoverable target dimension are left for an
// operator and take no admit slot (no offline gate can honestly run on them).
//
// BOUNDED PER SKILL (TG-65): admit also SKIPS a draft whose production version already holds
// MaxCandidatesPerTrial admitted-but-not-yet-trialed candidates with no active trial draining them.
// Otherwise the admitted set grows every run, the next trial's arm count grows with it, and StartTrial's
// traffic bar (MinSamplesPerArm × (1+arms)) rises past what bootstrap traffic can ever meet — so the start
// starves forever. The skipped draft stays a draft and is reconsidered once a trial drains the set.
func (d CreationDeps) admit(ctx context.Context, rep *CreationReport) {
	maxAdmit := d.Cfg.MaxAdmitPerRun
	if maxAdmit <= 0 {
		maxAdmit = DefaultMaxAdmitPerRun
	}
	maxCands := d.Cfg.MaxCandidatesPerTrial
	if maxCands <= 0 {
		maxCands = DefaultMaxCandidatesPerTrial
	}
	drafts, err := d.Store.FlywheelDrafts(ctx)
	if err != nil {
		rep.Errors = append(rep.Errors, "list flywheel drafts: "+err.Error())
		return
	}
	// TG-65 (stop over-admitting): the admitted-but-not-yet-trialed set on a production version is bounded
	// to maxCands. Count status='trial' candidates parented on the draft's production version and cache it,
	// incrementing as we admit this run, so a skill never piles up more admitted candidates than one trial
	// will arm. Over-admission is exactly what let the arm count — and thus StartTrial's traffic bar — grow
	// every run until a trial could never start.
	admittedByParent := map[int64]int{}
	countAdmitted := func(parentID int64) (int, error) {
		if n, ok := admittedByParent[parentID]; ok {
			return n, nil
		}
		cs, err := d.Store.AdmittedCandidates(ctx, parentID)
		if err != nil {
			return 0, err
		}
		admittedByParent[parentID] = len(cs)
		return len(cs), nil
	}
	processed := 0
	for _, draft := range drafts {
		dim := dimensionFromSource(draft.Source)
		if dim == "" {
			// No recoverable target dimension — an honest offline gate cannot be run, so this cron leaves
			// the draft for an operator (visible in the console's version history). Not admittable work.
			continue
		}
		admitted, err := countAdmitted(draft.ParentVersionID)
		if err != nil {
			rep.Errors = append(rep.Errors, "admitted-count "+draft.SkillName+": "+err.Error())
			continue
		}
		if admitted >= maxCands {
			// TG-65 cap: this draft's production version already holds a full trial's worth of admitted
			// candidates and no active trial is draining them, so offline-scoring another would only grow a
			// later trial's arm count past what StartTrial can complete at bootstrap traffic. It STAYS a draft
			// (no admit slot consumed) and is reconsidered once a trial completes and drains the admitted set.
			rep.AdmitSkippedAtCap++
			d.logf("flywheel admit: %s v%s held as draft — %s already has %d admitted candidate(s) at the cap of %d with no trial draining them (a skill trials in waves)",
				draft.SkillName, draft.Version, draft.SkillName, admitted, maxCands)
			continue
		}
		if processed >= maxAdmit {
			// TG-63 cap: this admittable draft is DEFERRED (not dropped) to a later run — oldest-first, so
			// the backlog drains deterministically without ever exceeding the activity budget.
			rep.DeferredAdmit++
			continue
		}
		processed++
		_, err = AdmitToTrial(ctx, d.Store, d.Ledger, d.Runner, draft.ID, dim)
		switch {
		case err == nil:
			rep.Admitted++
			admittedByParent[draft.ParentVersionID]++ // this production version now holds one more admitted candidate
			d.logf("flywheel admit: %s v%s passed the offline gate on %s — draft -> trial", draft.SkillName, draft.Version, dim)
		case errors.Is(err, ErrNotAdmitted):
			rep.RefusedAdmit++
			d.logf("flywheel admit: %s v%s refused by the offline gate on %s — stays draft, refusal stored", draft.SkillName, draft.Version, dim)
		default:
			rep.Errors = append(rep.Errors, "admit "+draft.SkillName+" v"+draft.Version+": "+err.Error())
		}
	}
	if rep.DeferredAdmit > 0 {
		d.logf("flywheel admit: %d draft(s) pending admission; processed the oldest %d this run, %d deferred to a later run (bounded so admit-all stays within the activity budget)",
			processed+rep.DeferredAdmit, processed, rep.DeferredAdmit)
	}
	if rep.AdmitSkippedAtCap > 0 {
		d.logf("flywheel admit: %d draft(s) held because their skill is already at the admitted-candidate cap (%d) with no trial draining it — reconsidered once a trial completes",
			rep.AdmitSkippedAtCap, maxCands)
	}
}

// start is phase 3: for each non-pinned production skill with NO active trial, gather its admitted
// (offline-passed) candidates and start ONE trial via StartTrial — which itself refuses (honestly) if
// the observed judged-session rate cannot complete it before its end date (REQ-1309). A refused start
// leaves the candidates admitted; the next run retries once traffic rises.
func (d CreationDeps) start(ctx context.Context, prods []Version, now time.Time, rep *CreationReport) {
	maxCands := d.Cfg.MaxCandidatesPerTrial
	if maxCands <= 0 {
		maxCands = DefaultMaxCandidatesPerTrial
	}
	for _, prod := range prods {
		sk, err := d.Store.GetSkill(ctx, prod.SkillName)
		if err != nil {
			rep.Errors = append(rep.Errors, "get skill "+prod.SkillName+": "+err.Error())
			continue
		}
		if sk.Pinned {
			continue
		}
		if _, active, err := d.Trials.ActiveTrialFor(ctx, prod.SkillName); err != nil {
			rep.Errors = append(rep.Errors, "active-trial "+prod.SkillName+": "+err.Error())
			continue
		} else if active {
			continue // one active trial per skill (REQ-1306) — let the running one finalize first
		}
		cands, err := d.Store.AdmittedCandidates(ctx, prod.ID)
		if err != nil {
			rep.Errors = append(rep.Errors, "admitted-candidates "+prod.SkillName+": "+err.Error())
			continue
		}
		if len(cands) == 0 {
			continue
		}
		// TG-65 (cap ARMS per trial): StartTrial's traffic gate needs MinSamplesPerArm × (1+arms) judged
		// sessions before the end date, so arming a trial with EVERY admitted candidate makes the sample bar
		// climb with the admitted set until it can never be met. Take at most maxCands of them, the TOP-K by
		// offline discovery-delta (higher delta = stronger candidate), with a deterministic ascending-id
		// tie-break. The rest stay admitted and are trialed in a later WAVE once this trial drains.
		taken := topKByOfflineDelta(cands, maxCands)
		dim := dimensionFromSource(taken[0].Source)
		if dim == "" {
			rep.Errors = append(rep.Errors, "start "+prod.SkillName+": admitted candidate carries no target dimension")
			continue
		}
		ids := make([]int64, len(taken))
		for i, c := range taken {
			ids[i] = c.ID
		}
		t := Trial{
			SkillName: prod.SkillName, CandidateIDs: ids, ControlVersionID: prod.ID, Dimension: dim,
			MinSamplesPerArm: d.Cfg.MinSamplesPerArm, MinLift: d.Cfg.MinLift, PThreshold: d.Cfg.PThreshold,
			EndsAt: now.Add(d.Cfg.TrialDuration),
			Note:   fmt.Sprintf("flywheel: top %d of %d admitted candidate(s) vs production on %s", len(ids), len(cands), dim),
		}
		_, err = StartTrial(ctx, d.Trials, t, now)
		switch {
		case err == nil:
			rep.TrialsStarted++
			d.logf("flywheel start: %s trial on %s — armed %d of %d admitted candidate(s) vs control (top-K by offline delta; the rest wait for a later wave; ends %s)",
				prod.SkillName, dim, len(ids), len(cands), t.EndsAt.UTC().Format("2006-01-02"))
		case errors.Is(err, ErrTrialStarvation):
			rep.RefusedStart++
			d.logf("flywheel start: %s trial refused — traffic too low to complete even a %d-arm trial (%v); candidates stay admitted, retried next run",
				prod.SkillName, len(ids)+1, err)
		default:
			rep.Errors = append(rep.Errors, "start "+prod.SkillName+": "+err.Error())
		}
	}
}

// topKByOfflineDelta returns the up-to-k strongest admitted candidates: ranked by offline discovery-delta
// descending (higher = stronger), deterministic ascending-id tie-break. Pure and non-mutating (it copies
// before sorting), so the same admitted set always selects the same wave (replay-safe).
func topKByOfflineDelta(cands []Version, k int) []Version {
	ranked := make([]Version, len(cands))
	copy(ranked, cands)
	sort.SliceStable(ranked, func(i, j int) bool {
		di, dj := offlineDelta(ranked[i]), offlineDelta(ranked[j])
		if di != dj {
			return di > dj
		}
		return ranked[i].ID < ranked[j].ID
	})
	if k > 0 && len(ranked) > k {
		ranked = ranked[:k]
	}
	return ranked
}

// offlineDelta reads the offline discovery-delta an admitted candidate cleared the gate with. AdmitToTrial
// stores the OfflineResult on the version row (eval_offline), whose DiscoveryDelta is the target-dimension
// gain on the discovery set — the "discovery <dim> delta +X.XXX" the offline runner records. Absent or
// unparseable ⇒ 0, which sorts behind any candidate carrying a recorded positive delta; the gate only
// admits DiscoveryDelta > 0, so a real admitted row always has one. Never panics on a malformed blob.
func offlineDelta(v Version) float64 {
	if len(v.OfflineEval) == 0 {
		return 0
	}
	var r OfflineResult
	if err := json.Unmarshal(v.OfflineEval, &r); err != nil {
		return 0
	}
	return r.DiscoveryDelta
}
