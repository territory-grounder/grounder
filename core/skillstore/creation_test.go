package skillstore

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

// fakeMeans is the MeansReader over a fixed per-version dimension table (the judge spine's rolling means
// in production).
type fakeMeans struct{ byVersion map[int64][]DimensionStat }

func (f fakeMeans) DimensionMeans(_ context.Context, versionID int64, _ time.Duration) ([]DimensionStat, error) {
	return f.byVersion[versionID], nil
}

func creationCfg() CreationConfig {
	return CreationConfig{
		Threshold: DefaultGenThreshold, MinSamples: DefaultGenMinSamples, Window: 14 * 24 * time.Hour,
		MinSamplesPerArm: 5, MinLift: 0.1, PThreshold: 0.05, TrialDuration: 30 * 24 * time.Hour,
		// Permissive arm cap so the pre-TG-65 scenarios (generation / admission / starvation) keep their
		// original multi-candidate shape; TG-65's small cap is exercised explicitly in its own tests below.
		MaxCandidatesPerTrial: 100,
	}
}

// REQ-1314: the generator fires only on a dimension that regressed below threshold with enough samples,
// and picks the WORST one. Healthy or under-sampled dimensions leave it idle.
func TestRegressed(t *testing.T) {
	stats := []DimensionStat{
		{"correct_diagnosis", 2.8, 12}, // regressed, worst
		{"evidence_grounded", 3.2, 12}, // regressed, but not the worst
		{"sensible_proposal", 4.5, 12}, // healthy
		{"appropriate_band", 1.0, 3},   // very low but UNDER-SAMPLED — not evidence
	}
	got, ok := Regressed(stats, 3.5, 5)
	if !ok || got.Dimension != "correct_diagnosis" {
		t.Fatalf("want worst regressed dimension correct_diagnosis, got %+v ok=%v", got, ok)
	}

	healthy := []DimensionStat{{"correct_diagnosis", 4.1, 30}, {"appropriate_band", 4.9, 30}}
	if _, ok := Regressed(healthy, 3.5, 5); ok {
		t.Fatal("no dimension regressed — generation must stay idle")
	}
	thin := []DimensionStat{{"correct_diagnosis", 1.5, 2}}
	if _, ok := Regressed(thin, 3.5, 5); ok {
		t.Fatal("under-sampled data is noise — generation must stay idle")
	}
}

// fillCfg is a bootstrap-shaped config where a 2-arm trial needs 30 scored samples (MinSamplesPerArm 15),
// the scored-rate window is 14d and a trial runs 30d — so a dimension needs at least ~14 scored samples in
// the trailing window to fill (30 × 336h ÷ 720h). It separates a sparse proposer-only dimension (few
// samples) from a dense dimension (many) for the TG-67 fill-aware selection tests.
func fillCfg() CreationConfig {
	return CreationConfig{
		Threshold: 3.5, MinSamples: 5, Window: 14 * 24 * time.Hour,
		MinSamplesPerArm: 15, MinLift: 0.2, PThreshold: 0.05, TrialDuration: 30 * 24 * time.Hour,
		MaxCandidatesPerTrial: 1,
	}
}

// TG-67: the fill projection is minimal-2-arm and reads the dimension's OWN scored-sample supply. A dense
// dimension with ample recent samples fills; a proposer-only dimension scored by too few sessions does not;
// zero supply or zero window/duration fail closed.
func TestDimensionFillsTrial(t *testing.T) {
	cfg := fillCfg() // need = 15×2 = 30; fillable iff samples ≥ ~14 over the 14d window for a 30d trial
	cases := []struct {
		name string
		stat DimensionStat
		want bool
	}{
		{"dense-dimension fills", DimensionStat{"evidence_grounded", 2.1, 40}, true},
		{"just-enough fills", DimensionStat{"correct_diagnosis", 2.4, 15}, true},
		{"proposer-only-sparse does not fill", DimensionStat{"falsifiable_prediction", 1.1, 12}, false},
		{"zero supply fails closed", DimensionStat{"falsifiable_prediction", 1.1, 0}, false},
	}
	for _, c := range cases {
		if got := dimensionFillsTrial(c.stat, cfg); got != c.want {
			t.Errorf("%s: dimensionFillsTrial(%+v) = %v, want %v", c.name, c.stat, got, c.want)
		}
	}
	// A large operator arm cap must NOT make an otherwise-fillable dimension read as unfillable — the
	// projection is minimal-2-arm regardless (TG-65 caps the actual trial's arms separately).
	big := fillCfg()
	big.MaxCandidatesPerTrial = 100
	if !dimensionFillsTrial(DimensionStat{"evidence_grounded", 2.1, 40}, big) {
		t.Fatal("minimal-2-arm projection must be independent of MaxCandidatesPerTrial")
	}
	// Zero duration / window fail closed.
	z := fillCfg()
	z.TrialDuration = 0
	if dimensionFillsTrial(DimensionStat{"evidence_grounded", 2.1, 40}, z) {
		t.Fatal("zero trial duration must not read as fillable")
	}
}

// TG-67 follow-up: an OVER-LONG TrialDuration (set to clear StartTrial's traffic bar on a data-starved
// estate) must NOT defeat the anti-starvation steering. Without a FillHorizon cap a 50d window makes even a
// proposer-only-sparse dimension read as fillable (the bug that burned 6/7 real trials on the unmovable
// falsifiable_prediction); FillHorizon bounds the STEERING projection so it steers to a dimension a trial can
// PRACTICALLY complete on, while a DENSE dimension still fills within the horizon.
func TestFillHorizonCapsSteering(t *testing.T) {
	sparse := DimensionStat{"falsifiable_prediction", 1.1, 12} // ~840h to fill the minimal trial
	overLong := fillCfg()
	overLong.TrialDuration = 50 * 24 * time.Hour // 1200h — a sparse dimension "fills" over this window

	overLong.FillHorizon = 0 // no cap ⇒ the over-long window makes the sparse dimension read as fillable (the bug)
	if !dimensionFillsTrial(sparse, overLong) {
		t.Fatal("without a FillHorizon cap, a 50d trial window makes even a proposer-only-sparse dimension read as fillable")
	}
	overLong.FillHorizon = DefaultFillHorizon // 14d ⇒ steering caps the projection; the sparse dimension is no longer a target
	if dimensionFillsTrial(sparse, overLong) {
		t.Fatal("FillHorizon must cap the steering projection so a sparse dimension over an over-long window is NOT a steer target")
	}
	// Steering must not exclude EVERYTHING: a dense dimension still fills within the horizon.
	if !dimensionFillsTrial(DimensionStat{"evidence_grounded", 2.1, 40}, overLong) {
		t.Fatal("a dense dimension must still fill within the FillHorizon — steering must still find a completable target")
	}
	// FillHorizon LONGER than the trial never extends it (min semantics): the actual duration still bounds.
	overLong.FillHorizon = 100 * 24 * time.Hour
	if !dimensionFillsTrial(sparse, overLong) {
		t.Fatal("a FillHorizon longer than TrialDuration must not shorten the projection below TrialDuration")
	}
}

// TG-67: FillableRegression targets the worst regressed dimension a trial can actually FILL. When the very
// worst regression is proposer-only-sparse (falsifiable_prediction), it is passed over for the worst DENSE
// dimension the abundant grounded-stand-down traffic fills, and the skip is reported for honest logging.
func TestFillableRegressionPrefersFillableDense(t *testing.T) {
	cfg := fillCfg()
	stats := []DimensionStat{
		{"falsifiable_prediction", 1.1, 12}, // WORST mean, but proposer-only-sparse → cannot fill
		{"evidence_grounded", 2.2, 40},      // regressed + dense → fillable, and the worst that can
		{"correct_diagnosis", 2.6, 40},      // regressed + dense, but less severe than evidence_grounded
		{"appropriate_band", 4.6, 40},       // healthy
	}
	got, ok, skipped := FillableRegression(stats, cfg)
	if !ok {
		t.Fatal("a fillable regressed dimension exists — want ok")
	}
	if got.Dimension != "evidence_grounded" {
		t.Fatalf("want worst FILLABLE dimension evidence_grounded, got %q", got.Dimension)
	}
	if !skipped {
		t.Fatal("the worse falsifiable_prediction regression was passed over — skipped must be reported")
	}
}

// TG-67: when the ONLY regressed dimension is proposer-only-sparse, no trial can fill — the skill is
// deferred (ok=false) rather than armed on an unfillable dimension, and the regression is still reported.
func TestFillableRegressionSkipsWhenNoneFill(t *testing.T) {
	cfg := fillCfg()
	stats := []DimensionStat{
		{"falsifiable_prediction", 1.1, 12}, // regressed but unfillable
		{"evidence_grounded", 4.2, 40},      // healthy
	}
	if got, ok, skipped := FillableRegression(stats, cfg); ok || !skipped {
		t.Fatalf("want ok=false skipped=true (regressed but unfillable), got %+v ok=%v skipped=%v", got, ok, skipped)
	}
	// Nothing regressed at all: ok=false, skipped=false (idle, not a deferred regression).
	healthy := []DimensionStat{{"evidence_grounded", 4.2, 40}, {"correct_diagnosis", 4.5, 40}}
	if _, ok, skipped := FillableRegression(healthy, cfg); ok || skipped {
		t.Fatalf("healthy data must be idle: ok=false skipped=false, got ok=%v skipped=%v", ok, skipped)
	}
}

// TG-67: when every regressed dimension can fill, FillableRegression matches Regressed — it returns the
// worst by mean and reports no skip.
func TestFillableRegressionWorstWhenAllFill(t *testing.T) {
	cfg := fillCfg()
	stats := []DimensionStat{
		{"evidence_grounded", 2.2, 40}, // worst, fillable
		{"correct_diagnosis", 2.6, 40}, // fillable
	}
	got, ok, skipped := FillableRegression(stats, cfg)
	if !ok || got.Dimension != "evidence_grounded" || skipped {
		t.Fatalf("all fillable → worst-by-mean, no skip: got %q ok=%v skipped=%v", got.Dimension, ok, skipped)
	}
}

// REQ-1314 / REQ-1312 / REQ-1307 / REQ-1309: the full happy path — a regressed dimension generates
// drafts (generate-only), the offline gate admits them, and one trial starts. Production is untouched.
func TestRunCreationHalfHappyPath(t *testing.T) {
	m, lg, prod := genStore(t)
	trials := NewMemTrialStore(100) // ample judged-session rate
	deps := CreationDeps{
		Store:  m,
		Means:  fakeMeans{byVersion: map[int64][]DimensionStat{prod.ID: {{"correct_diagnosis", 2.8, 20}, {"evidence_grounded", 4.3, 20}}}},
		Trials: trials,
		Ledger: lg,
		Model:  &scriptedGen{replies: []string{"rewrite A", "rewrite B", "rewrite C"}},
		Runner: fakeRunner{OfflineResult{RunID: "off", RegressionPass: true, DiscoveryDelta: 0.4}},
		Cfg:    creationCfg(),
		RunID:  "run-1",
	}
	rep, err := RunCreationHalf(context.Background(), deps, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Generated != 3 || rep.Admitted != 3 || rep.TrialsStarted != 1 {
		t.Fatalf("want 3 generated, 3 admitted, 1 trial; got %+v", rep)
	}
	if len(rep.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", rep.Errors)
	}
	// Generate-only: composition (the production body) is untouched.
	got, _, _ := m.ProductionVersion(context.Background(), "triage-protocol")
	if got.Body != "current production body" {
		t.Fatal("a generation cycle must not touch production")
	}
	// The trial is active on the regressed dimension with all three candidates.
	tr, ok, _ := trials.ActiveTrialFor(context.Background(), "triage-protocol")
	if !ok || tr.Dimension != "correct_diagnosis" || len(tr.CandidateIDs) != 3 {
		t.Fatalf("want an active trial on correct_diagnosis with 3 candidates, got %+v ok=%v", tr, ok)
	}
	if tr.ControlVersionID != prod.ID {
		t.Fatalf("the production version is control, want %d got %d", prod.ID, tr.ControlVersionID)
	}
	for _, id := range tr.CandidateIDs {
		v, _ := m.GetVersion(context.Background(), id)
		if v.Status != StatusTrial || v.Author != AuthorFlywheel {
			t.Fatalf("candidate %d must be an admitted flywheel version, got %s/%s", id, v.Status, v.Author)
		}
	}

	// Idempotency: a second cycle with the same regression does NOT regenerate (open candidates pending)
	// and does NOT start a second trial (one active per skill).
	deps.Model = &scriptedGen{replies: []string{"must-not-be-used"}}
	rep2, err := RunCreationHalf(context.Background(), deps, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Generated != 0 || rep2.TrialsStarted != 0 || rep2.Skipped == 0 {
		t.Fatalf("second cycle must dedup (0 generated, 0 started, >=1 skipped), got %+v", rep2)
	}
}

// REQ-1307: an offline refusal keeps the candidate a draft with the refusal stored; no trial starts.
func TestRunCreationHalfOfflineRefusalStaysDraft(t *testing.T) {
	m, lg, prod := genStore(t)
	trials := NewMemTrialStore(100)
	deps := CreationDeps{
		Store:  m,
		Means:  fakeMeans{byVersion: map[int64][]DimensionStat{prod.ID: {{"correct_diagnosis", 2.5, 20}}}},
		Trials: trials,
		Ledger: lg,
		Model:  &scriptedGen{replies: []string{"rewrite A", "rewrite B", "rewrite C"}},
		Runner: fakeRunner{OfflineResult{RunID: "off", RegressionPass: false, DiscoveryDelta: 0.4}},
		Cfg:    creationCfg(),
		RunID:  "run-2",
	}
	rep, err := RunCreationHalf(context.Background(), deps, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Generated != 3 || rep.RefusedAdmit != 3 || rep.Admitted != 0 || rep.TrialsStarted != 0 {
		t.Fatalf("want 3 generated, 3 refused-admit, 0 admitted, 0 trials; got %+v", rep)
	}
	if _, ok, _ := trials.ActiveTrialFor(context.Background(), "triage-protocol"); ok {
		t.Fatal("a refused candidate must not start a trial")
	}
	drafts, _ := m.FlywheelDrafts(context.Background())
	if len(drafts) != 3 {
		t.Fatalf("refused candidates must stay drafts, got %d", len(drafts))
	}
	if drafts[0].OfflineEval == nil {
		t.Fatal("the offline refusal must be stored on the version row")
	}
}

// REQ-1309: admitted candidates whose trial cannot complete at the observed session rate are refused at
// start (honest starvation refusal) — they stay admitted and are retried next run.
func TestRunCreationHalfTrafficStarvedRefusesStart(t *testing.T) {
	m, lg, prod := genStore(t)
	trials := NewMemTrialStore(0) // no judged sessions — every start refuses
	deps := CreationDeps{
		Store:  m,
		Means:  fakeMeans{byVersion: map[int64][]DimensionStat{prod.ID: {{"correct_diagnosis", 2.9, 20}}}},
		Trials: trials,
		Ledger: lg,
		Model:  &scriptedGen{replies: []string{"rewrite A", "rewrite B", "rewrite C"}},
		Runner: fakeRunner{OfflineResult{RunID: "off", RegressionPass: true, DiscoveryDelta: 0.5}},
		Cfg:    creationCfg(),
		RunID:  "run-3",
	}
	rep, err := RunCreationHalf(context.Background(), deps, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Admitted != 3 || rep.TrialsStarted != 0 || rep.RefusedStart != 1 {
		t.Fatalf("want 3 admitted, 0 started, 1 refused-start; got %+v", rep)
	}
	if _, ok, _ := trials.ActiveTrialFor(context.Background(), "triage-protocol"); ok {
		t.Fatal("a starved trial must not be created")
	}
	// The candidates are admitted (status trial) and remain available for a later start.
	cands, _ := m.AdmittedCandidates(context.Background(), prod.ID)
	if len(cands) != 3 {
		t.Fatalf("admitted candidates must persist for the next run, got %d", len(cands))
	}
}

// multiProdStore builds n non-pinned production skills (skill-0..skill-(n-1)), each with a production
// version — the multi-skill fixture for the global-regression / worst-first oracles.
func multiProdStore(t *testing.T, n int) (*MemStore, *audit.Ledger, []Version) {
	t.Helper()
	m := NewMemStore()
	lg := audit.NewLedger()
	ctx := context.Background()
	var prods []Version
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("skill-%d", i)
		m.PutSkill(Skill{Name: name, Kind: "behavioral", Position: i + 1})
		v := draft(t, m, name, "1.0.0", fmt.Sprintf("production body %d", i))
		if _, err := Transition(ctx, m, lg, v.ID, StatusTrial, "gate"); err != nil {
			t.Fatal(err)
		}
		v, err := Transition(ctx, m, lg, v.ID, StatusProduction, "initial")
		if err != nil {
			t.Fatal(err)
		}
		prods = append(prods, v)
	}
	return m, lg, prods
}

// seedFlywheelDraft lands one open flywheel DRAFT (author=flywheel, carrying a recoverable target
// dimension in its source) parented on prod — the admit-backlog fixture.
func seedFlywheelDraft(t *testing.T, m *MemStore, prod Version, i int, dim, runID string) Version {
	t.Helper()
	body := fmt.Sprintf("candidate body %d", i)
	aw := AppliesWhen{Phases: []string{"investigate"}, ExecClasses: []string{"STANDARD_AGENT", "DEEP_INVESTIGATION"}}
	v, err := m.CreateVersion(context.Background(), Version{
		SkillName: prod.SkillName, Version: fmt.Sprintf("1.0.0-cand%d", i),
		Body: body, AppliesWhen: aw, ContentHash: ContentHash(body, aw),
		Author: AuthorFlywheel, Source: GenSource(dim, runID),
		Rationale: "[draft] seeded flywheel backlog", ParentVersionID: prod.ID,
	})
	if err != nil {
		t.Fatalf("seed flywheel draft %d: %v", i, err)
	}
	return v
}

// TG-63: a judge dimension that floors GLOBALLY makes every non-pinned skill regress at once. Generation
// must NOT flood every skill in one activity — with MaxGenSkillsPerRun=1 it drafts for exactly the single
// WORST-regressed skill this run and DEFERS the rest (the daily cron drains the ranked backlog worst-first).
func TestGenerateCapsToWorstRegressedSkill(t *testing.T) {
	m, lg, prods := multiProdStore(t, 6)
	// Distinct means on the same globally-low dimension so there is a unique worst; skill-0 is lowest (1.0).
	means := map[int64][]DimensionStat{}
	for i, p := range prods {
		means[p.ID] = []DimensionStat{{"falsifiable_prediction", 1.0 + 0.1*float64(i), 20}}
	}
	var logs []string
	cfg := creationCfg()
	cfg.MaxGenSkillsPerRun = 1
	deps := CreationDeps{
		Store: m, Means: fakeMeans{byVersion: means}, Trials: NewMemTrialStore(100), Ledger: lg,
		Model:  &scriptedGen{replies: []string{"rewrite A", "rewrite B", "rewrite C"}},
		Runner: fakeRunner{OfflineResult{RunID: "off", RegressionPass: true, DiscoveryDelta: 0.4}},
		Cfg:    cfg, RunID: "run-gencap",
		Log: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	rep, err := RunCreationHalf(context.Background(), deps, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the worst skill drafted (3 candidates); the other 5 DEFERRED, not dropped.
	if rep.Generated != 3 || rep.DeferredGen != 5 {
		t.Fatalf("want 3 generated (worst only) + 5 deferred, got %+v", rep)
	}
	worst := prods[0].SkillName
	for _, p := range prods {
		open, _ := m.OpenCandidates(context.Background(), p.ID)
		if p.SkillName == worst && open == 0 {
			t.Fatalf("the worst-regressed skill %s must have drafted candidates", worst)
		}
		if p.SkillName != worst && open != 0 {
			t.Fatalf("non-worst skill %s must NOT draft under the cap (got %d open)", p.SkillName, open)
		}
	}
	// The deferral is logged honestly (not a silent skip).
	if !strings.Contains(strings.Join(logs, "\n"), "deferred") {
		t.Fatalf("the deferred skills must be logged; logs=%v", logs)
	}
}

// TG-63: the offline admission gate offline-scores candidate-vs-production with model calls, so admitting
// an unbounded backlog in one activity blows the budget. With MaxAdmitPerRun=3 admit processes exactly the
// 3 OLDEST drafts and DEFERS the rest; the backlog drains deterministically across successive runs.
func TestAdmitCapsPerRunAndDrainsAcrossRuns(t *testing.T) {
	m, lg, prod := genStore(t)
	ctx := context.Background()
	// Seed 17 open flywheel drafts (the live TG-63 backlog), each with a recoverable target dimension.
	for i := 0; i < 17; i++ {
		seedFlywheelDraft(t, m, prod, i, "correct_diagnosis", "run-seed")
	}
	pre, _ := m.FlywheelDrafts(ctx)
	if len(pre) != 17 {
		t.Fatalf("want 17 seeded drafts, got %d", len(pre))
	}
	var logs []string
	cfg := creationCfg()
	cfg.MaxAdmitPerRun = 3
	deps := CreationDeps{
		Store:  m,
		Means:  fakeMeans{byVersion: map[int64][]DimensionStat{}}, // no regression → generation stays idle
		Trials: NewMemTrialStore(0),                               // starved → keep the test about the admit cap, no trial starts
		Ledger: lg,
		Model:  &scriptedGen{replies: []string{"must-not-be-used"}},
		Runner: fakeRunner{OfflineResult{RunID: "off", RegressionPass: true, DiscoveryDelta: 0.4}},
		Cfg:    cfg, RunID: "run-admitcap",
		Log: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	// Run 1: exactly the 3 OLDEST admitted, 14 deferred; generation idle.
	rep, err := RunCreationHalf(ctx, deps, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Generated != 0 || rep.Admitted != 3 || rep.DeferredAdmit != 14 {
		t.Fatalf("run 1: want 0 generated, 3 admitted, 14 deferred; got %+v", rep)
	}
	remaining, _ := m.FlywheelDrafts(ctx)
	if len(remaining) != 14 {
		t.Fatalf("run 1: want 14 drafts left, got %d", len(remaining))
	}
	// Oldest-first: the 14 left are exactly the higher-id tail (pre[3:]).
	if remaining[0].ID != pre[3].ID {
		t.Fatalf("run 1 must admit the OLDEST 3; lowest remaining id %d != pre[3] id %d", remaining[0].ID, pre[3].ID)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "deferred") {
		t.Fatalf("the deferred drafts must be logged; logs=%v", logs)
	}
	// Run 2: the backlog DRAINS — the next 3 oldest admitted, 11 left.
	rep2, err := RunCreationHalf(ctx, deps, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Admitted != 3 || rep2.DeferredAdmit != 11 {
		t.Fatalf("run 2: want 3 admitted, 11 deferred; got %+v", rep2)
	}
	remaining2, _ := m.FlywheelDrafts(ctx)
	if len(remaining2) != 11 {
		t.Fatalf("run 2: backlog must drain to 11, got %d", len(remaining2))
	}
	if remaining2[0].ID != pre[6].ID {
		t.Fatalf("run 2 must admit the next-oldest 3; lowest remaining id %d != pre[6] id %d", remaining2[0].ID, pre[6].ID)
	}
}

// seedAdmittedCandidate lands one flywheel candidate ADMITTED (status=trial) on prod, carrying the given
// offline discovery-delta in its stored OfflineResult — the top-K ranking's input for the TG-65 arm cap.
// It goes through the real AdmitToTrial so OfflineEval is stored exactly as production would store it.
func seedAdmittedCandidate(t *testing.T, m *MemStore, lg *audit.Ledger, prod Version, i int, dim string, delta float64) Version {
	t.Helper()
	d := seedFlywheelDraft(t, m, prod, i, dim, "run-seed")
	v, err := AdmitToTrial(context.Background(), m, lg,
		fakeRunner{OfflineResult{RunID: fmt.Sprintf("off-%d", i), RegressionPass: true, DiscoveryDelta: delta}}, d.ID, dim)
	if err != nil {
		t.Fatalf("seed admitted candidate %d: %v", i, err)
	}
	return v
}

// TG-65 (cap ARMS per trial): a skill sitting on 4 admitted candidates must start a clean 2-arm trial —
// control vs the single HIGHEST-offline-delta candidate — not a 5-arm trial that StartTrial would starve on
// at bootstrap traffic. The other 3 stay admitted for a later wave. This is the exact live bug: more arms
// raise StartTrial's bar (MinSamplesPerArm × (1+arms)), so an unbounded admitted set never starts.
func TestStartCapsTrialArmsToTopKByOfflineDelta(t *testing.T) {
	m, lg, prod := genStore(t)
	ctx := context.Background()
	// Deltas chosen so the strongest is NOT the oldest id — proves the pick is by delta, not creation order.
	deltas := []float64{0.20, 0.55, 0.30, 0.45}
	var seeded []Version
	for i, dl := range deltas {
		seeded = append(seeded, seedAdmittedCandidate(t, m, lg, prod, i, "correct_diagnosis", dl))
	}
	want := seeded[1] // delta 0.55 is the strongest
	// Bootstrap traffic: 2/day, 15 samples/arm, 30-day trial. A 2-arm trial needs 30 sessions (~15d, OK); the
	// pre-fix 5-arm trial would need 75 (~37.5d > 30d) and StartTrial would refuse — the arm cap is what lets
	// the trial start at all.
	trials := NewMemTrialStore(2)
	cfg := creationCfg()
	cfg.MaxCandidatesPerTrial = 1
	cfg.MinSamplesPerArm = 15
	cfg.TrialDuration = 30 * 24 * time.Hour
	deps := CreationDeps{
		Store: m, Means: fakeMeans{byVersion: map[int64][]DimensionStat{}}, // no regression → generation idle
		Trials: trials, Ledger: lg,
		Model:  &scriptedGen{replies: []string{"must-not-be-used"}},
		Runner: fakeRunner{OfflineResult{RunID: "off", RegressionPass: true, DiscoveryDelta: 0.4}},
		Cfg:    cfg, RunID: "run-armcap",
	}
	rep, err := RunCreationHalf(ctx, deps, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.TrialsStarted != 1 {
		t.Fatalf("want exactly 1 trial started (a 2-arm trial clears the traffic gate), got %+v", rep)
	}
	tr, ok, _ := trials.ActiveTrialFor(ctx, "triage-protocol")
	if !ok {
		t.Fatal("a 2-arm trial must start at bootstrap traffic")
	}
	if len(tr.CandidateIDs) != 1 {
		t.Fatalf("TG-65 arm cap: want 1 candidate (control-vs-candidate 2-arm trial), got %d arms", len(tr.CandidateIDs))
	}
	if tr.CandidateIDs[0] != want.ID {
		t.Fatalf("must arm the HIGHEST-offline-delta candidate (id %d, delta 0.55), got id %d", want.ID, tr.CandidateIDs[0])
	}
	// The other 3 stay admitted (status=trial), available for a later wave.
	cands, _ := m.AdmittedCandidates(ctx, prod.ID)
	if len(cands) != 4 {
		t.Fatalf("all 4 candidates remain admitted (only 1 is armed this trial), got %d", len(cands))
	}
}

// TG-65 (stop over-admitting): admit must SKIP offline-scoring more drafts for a skill that already sits at
// the admitted-candidate cap with no trial draining it, and instead spend the run's budget on a different
// under-cap skill. Over-admission is what grew the arm count run after run until the start starved forever.
func TestAdmitSkipsSkillAtArmCap(t *testing.T) {
	m, lg, prods := multiProdStore(t, 2)
	ctx := context.Background()
	// skill-0: already AT the cap (1 admitted) plus a still-pending draft that must NOT be scored this run.
	seedAdmittedCandidate(t, m, lg, prods[0], 0, "correct_diagnosis", 0.5)
	pendingA := seedFlywheelDraft(t, m, prods[0], 1, "correct_diagnosis", "run-seed")
	// skill-1: under the cap (0 admitted) with a pending draft that SHOULD be admitted.
	pendingB := seedFlywheelDraft(t, m, prods[1], 2, "correct_diagnosis", "run-seed")

	var logs []string
	cfg := creationCfg()
	cfg.MaxCandidatesPerTrial = 1
	deps := CreationDeps{
		Store: m, Means: fakeMeans{byVersion: map[int64][]DimensionStat{}}, // generation idle
		Trials: NewMemTrialStore(0), // starved → keep the test about admit, no trial starts
		Ledger: lg,
		Model:  &scriptedGen{replies: []string{"must-not-be-used"}},
		Runner: fakeRunner{OfflineResult{RunID: "off", RegressionPass: true, DiscoveryDelta: 0.4}},
		Cfg:    cfg, RunID: "run-admit-skip",
		Log: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	rep, err := RunCreationHalf(ctx, deps, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Admitted != 1 || rep.AdmitSkippedAtCap != 1 {
		t.Fatalf("want 1 admitted (the under-cap skill) + 1 skipped-at-cap, got %+v", rep)
	}
	// skill-0's pending draft was NOT offline-scored: still a draft, no eval stored.
	stillDraft, _ := m.GetVersion(ctx, pendingA.ID)
	if stillDraft.Status != StatusDraft || stillDraft.OfflineEval != nil {
		t.Fatalf("the at-cap skill's draft must be left unscored (draft, no eval), got status=%s eval=%s", stillDraft.Status, stillDraft.OfflineEval)
	}
	if got, _ := m.AdmittedCandidates(ctx, prods[0].ID); len(got) != 1 {
		t.Fatalf("skill-0 must stay AT the cap (1 admitted), got %d", len(got))
	}
	// skill-1's draft WAS admitted — the run spent its budget on the under-cap skill.
	admittedB, _ := m.GetVersion(ctx, pendingB.ID)
	if admittedB.Status != StatusTrial {
		t.Fatalf("the under-cap skill's draft must be admitted (trial), got %s", admittedB.Status)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "held as draft") {
		t.Fatalf("the at-cap skip must be logged honestly; logs=%v", logs)
	}
}

// TG-65 (waves): once a trial completes and its arm drains out of the admitted set, the NEXT run trials the
// next wave — the next-strongest admitted candidate — rather than the same one or a swollen arm set.
func TestArmCapTrialsNextWaveAfterDrain(t *testing.T) {
	m, lg, prod := genStore(t)
	ctx := context.Background()
	// Three admitted candidates; strongest is cand0 (0.5), next is cand2 (0.4).
	c0 := seedAdmittedCandidate(t, m, lg, prod, 0, "correct_diagnosis", 0.5)
	_ = seedAdmittedCandidate(t, m, lg, prod, 1, "correct_diagnosis", 0.3)
	c2 := seedAdmittedCandidate(t, m, lg, prod, 2, "correct_diagnosis", 0.4)

	trials := NewMemTrialStore(100) // ample traffic; the point here is wave ordering, not starvation
	cfg := creationCfg()
	cfg.MaxCandidatesPerTrial = 1
	deps := CreationDeps{
		Store: m, Means: fakeMeans{byVersion: map[int64][]DimensionStat{}},
		Trials: trials, Ledger: lg,
		Model:  &scriptedGen{replies: []string{"must-not-be-used"}},
		Runner: fakeRunner{OfflineResult{RunID: "off", RegressionPass: true, DiscoveryDelta: 0.4}},
		Cfg:    cfg, RunID: "run-wave",
	}
	// Run 1: the strongest candidate (cand0, 0.5) is trialed.
	if _, err := RunCreationHalf(ctx, deps, time.Now()); err != nil {
		t.Fatal(err)
	}
	tr1, ok, _ := trials.ActiveTrialFor(ctx, "triage-protocol")
	if !ok || len(tr1.CandidateIDs) != 1 || tr1.CandidateIDs[0] != c0.ID {
		t.Fatalf("run 1 must trial the strongest candidate %d, got %+v ok=%v", c0.ID, tr1, ok)
	}
	// Drain: the trial completes and its arm leaves the admitted set (as the graduation half would do).
	if err := trials.FinalizeTrial(ctx, tr1.ID, "completed", c0.ID, 0.9, 0.01, "test: drained"); err != nil {
		t.Fatal(err)
	}
	if _, err := Transition(ctx, m, lg, c0.ID, StatusRejected, "test: drained after trial"); err != nil {
		t.Fatal(err)
	}
	// Run 2: the next wave — the next-strongest remaining candidate (cand2, 0.4) — is trialed.
	if _, err := RunCreationHalf(ctx, deps, time.Now()); err != nil {
		t.Fatal(err)
	}
	tr2, ok, _ := trials.ActiveTrialFor(ctx, "triage-protocol")
	if !ok || len(tr2.CandidateIDs) != 1 || tr2.CandidateIDs[0] != c2.ID {
		t.Fatalf("run 2 must trial the NEXT wave (candidate %d, delta 0.4), got %+v ok=%v", c2.ID, tr2, ok)
	}
}

// A pinned skill is never a generation target even when a means table would say it regressed (REQ-1305).
// (The compiled body reaches production via the boot importer; here the skill is pinned after it has a
// production row, mirroring a skill an operator pins post-hoc.)
func TestRunCreationHalfSkipsPinned(t *testing.T) {
	m, lg, prod := genStore(t)
	m.PutSkill(Skill{Name: "triage-protocol", Kind: "behavioral", Pinned: true, Position: 5})
	trials := NewMemTrialStore(100)
	deps := CreationDeps{
		Store:  m,
		Means:  fakeMeans{byVersion: map[int64][]DimensionStat{prod.ID: {{"correct_diagnosis", 1.0, 50}}}},
		Trials: trials,
		Ledger: lg,
		Model:  &scriptedGen{replies: []string{"must-not-be-used"}},
		Runner: fakeRunner{OfflineResult{RegressionPass: true, DiscoveryDelta: 1}},
		Cfg:    creationCfg(),
		RunID:  "run-4",
	}
	rep, err := RunCreationHalf(context.Background(), deps, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Generated != 0 || rep.TrialsStarted != 0 {
		t.Fatalf("a pinned skill must never generate or trial, got %+v", rep)
	}
}
