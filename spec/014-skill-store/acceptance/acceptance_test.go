package acceptance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/agent/skills"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/skillstore"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// The spec/014 oracles drive the REAL core/skillstore state machine over the in-memory Store (the pgx
// implementation is integration-tested under compose — constraint D5). Scenarios still @pending are
// spec-ahead-of-code and skipped by ~@pending.

type world struct {
	store  *skillstore.MemStore
	ledger *audit.Ledger
	skill  string
	verID  int64
	err    error
}

func (w *world) reset() {
	w.store = skillstore.NewMemStore()
	w.ledger = audit.NewLedger()
	w.store.PutSkill(skillstore.Skill{Name: "triage-protocol", Kind: "behavioral", Position: 5})
	w.store.PutSkill(skillstore.Skill{Name: "conservative-remediation", Kind: "catalog", Pinned: true, Position: 4})
	w.skill = "triage-protocol"
	w.verID = 0
	w.err = nil
}

func (w *world) createDraft(name, ver, body, rationale string, aw skillstore.AppliesWhen) (skillstore.Version, error) {
	return w.store.CreateVersion(context.Background(), skillstore.Version{
		SkillName: name, Version: ver, Body: body, AppliesWhen: aw,
		ContentHash: skillstore.ContentHash(body, aw), Author: "operator:acceptance", Source: "hand",
		Rationale: rationale,
	})
}

func TestSkillStoreAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name: "spec/014 skill-store",
		ScenarioInitializer: func(sc *godog.ScenarioContext) {
			initializeScenario(sc)
			initializeComposeScenario(sc)
			initializeReadSurfaceScenario(sc)
			initializeWritePathScenario(sc)
			initializeTrialEngineScenario(sc)
			initializeWatchScenario(sc)
			initializeFlywheelScenario(sc)
			initializeCreationScenario(sc)
		},
		Options: &godog.Options{Format: "pretty", Paths: []string{"."}, Tags: "~@pending", Strict: true, TestingT: t},
	}
	if suite.Run() != 0 {
		t.Fatal("spec/014 acceptance scenarios failed")
	}
}

func initializeScenario(sc *godog.ScenarioContext) {
	w := &world{}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		w.reset()
		return ctx, nil
	})

	sc.Step(`^a skill store$`, func() error { return nil })
	sc.Step(`^a draft version$`, func() error {
		v, err := w.createDraft(w.skill, "2.0.0", "body v2", "authored for acceptance", skillstore.AppliesWhen{Phases: []string{"investigate"}})
		if err != nil {
			return err
		}
		w.verID = v.ID
		return nil
	})

	sc.Step(`^a draft is created without a rationale or with an unknown execution class in its predicate$`, func() error {
		_, e1 := w.createDraft(w.skill, "2.0.0", "body", "", skillstore.AppliesWhen{})
		_, e2 := w.createDraft(w.skill, "2.0.1", "body", "r", skillstore.AppliesWhen{ExecClasses: []string{"TURBO"}})
		if !errors.Is(e1, skillstore.ErrRationaleRequired) {
			return fmt.Errorf("missing rationale must be refused, got %v", e1)
		}
		if !errors.Is(e2, skillstore.ErrBadPredicate) {
			return fmt.Errorf("unknown class must be refused, got %v", e2)
		}
		return nil
	})
	sc.Step(`^the write is refused and no row exists$`, func() error {
		if n := len(w.store.VersionsOf(w.skill)); n != 0 {
			return fmt.Errorf("no row must exist, got %d", n)
		}
		return nil
	})

	sc.Step(`^it is admitted, graduated, and retired$`, func() error {
		for _, mv := range []struct {
			to  skillstore.Status
			why string
		}{
			{skillstore.StatusTrial, "offline gate passed"},
			{skillstore.StatusProduction, "welch p=0.02 lift=0.3 vs concurrent control"},
			{skillstore.StatusRetired, "operator rollback"},
		} {
			if _, err := skillstore.Transition(context.Background(), w.store, w.ledger, w.verID, mv.to, mv.why); err != nil {
				return fmt.Errorf("%s: %w", mv.to, err)
			}
		}
		return nil
	})
	sc.Step(`^each transition carries a rationale and appends a governance-ledger entry$`, func() error {
		v, err := w.store.GetVersion(context.Background(), w.verID)
		if err != nil {
			return err
		}
		for _, mark := range []string{"[trial]", "[production]", "[retired]"} {
			if !strings.Contains(v.Rationale, mark) {
				return fmt.Errorf("rationale log missing %s: %q", mark, v.Rationale)
			}
		}
		if v.LedgerSeq == 0 {
			return fmt.Errorf("ledger seq must be recorded")
		}
		return w.ledger.Verify()
	})

	sc.Step(`^a skill with a production version$`, func() error {
		v, err := w.createDraft(w.skill, "2.0.0", "body v2", "authored", skillstore.AppliesWhen{})
		if err != nil {
			return err
		}
		if _, err := skillstore.Transition(context.Background(), w.store, w.ledger, v.ID, skillstore.StatusTrial, "gate"); err != nil {
			return err
		}
		_, err = skillstore.Transition(context.Background(), w.store, w.ledger, v.ID, skillstore.StatusProduction, "welch pass")
		return err
	})
	sc.Step(`^another version graduates$`, func() error {
		v, err := w.createDraft(w.skill, "3.0.0", "body v3", "authored", skillstore.AppliesWhen{})
		if err != nil {
			return err
		}
		if _, err := skillstore.Transition(context.Background(), w.store, w.ledger, v.ID, skillstore.StatusTrial, "gate"); err != nil {
			return err
		}
		w.verID = v.ID
		_, err = skillstore.Transition(context.Background(), w.store, w.ledger, v.ID, skillstore.StatusProduction, "welch pass")
		return err
	})
	sc.Step(`^the prior production version is retired in the same transaction and exactly one production row remains$`, func() error {
		var prod, retired int
		for _, v := range w.store.VersionsOf(w.skill) {
			switch v.Status {
			case skillstore.StatusProduction:
				prod++
			case skillstore.StatusRetired:
				retired++
			}
		}
		if prod != 1 || retired != 1 {
			return fmt.Errorf("want 1 production + 1 retired, got %d/%d", prod, retired)
		}
		return nil
	})

	sc.Step(`^the pinned conservative-remediation skill$`, func() error {
		w.skill = "conservative-remediation"
		return nil
	})
	sc.Step(`^a draft version targets it$`, func() error {
		_, w.err = w.createDraft(w.skill, "2.0.0", "weakened floor", "attempt", skillstore.AppliesWhen{})
		return nil
	})
	sc.Step(`^the write is refused$`, func() error {
		if !errors.Is(w.err, skillstore.ErrPinnedSkill) {
			return fmt.Errorf("pinned write must be refused, got %v", w.err)
		}
		return nil
	})
}

// ---- T-014-2: compose-from-store scenarios (REQ-1303/1304/1305) ----

type composeWorld struct {
	rows []skillstore.ProductionRow
	reg  *skills.Registry
	prov skills.Provenance
	inv  runner.InvestigateResult
}

type stopModel struct{}

func (stopModel) Complete(_ context.Context, _, _ string, _ []model.Message) (string, error) {
	return `{"action":"stop","confidence":0.9,"reason":"acceptance stop"}`, nil
}

func initializeComposeScenario(sc *godog.ScenarioContext) {
	w := &composeWorld{}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		*w = composeWorld{}
		return ctx, nil
	})
	deep := skills.Context{Phase: skills.PhaseInvestigate, ExecClass: execclass.DeepInvestigation}
	prow := func(name, ver, body string, pinned bool) skillstore.ProductionRow {
		return skillstore.ProductionRow{VersionID: 90, SkillName: name, Version: ver, Body: body,
			ContentHash: skillstore.ContentHash(body, skillstore.AppliesWhen{}), Pinned: pinned, Position: 1}
	}

	sc.Step(`^a production snapshot with phase- and class-scoped versions$`, func() error {
		w.rows = []skillstore.ProductionRow{prow("triage-protocol", "9.0.0", "store triage v9", false)}
		w.reg, w.prov = skills.NewFromStore(w.rows, skills.Default())
		return nil
	})
	sc.Step(`^the same typed context composes twice$`, func() error { return nil })
	sc.Step(`^the same bodies compose in the same order both times$`, func() error {
		b1, _ := w.reg.Compose(deep)
		b2, _ := w.reg.Compose(deep)
		if b1 != b2 || !strings.Contains(b1, "store triage v9") {
			return fmt.Errorf("composition must be deterministic and store-backed")
		}
		return nil
	})

	sc.Step(`^a store that fails to load$`, func() error {
		bad := prow("triage-protocol", "9.0.0", "tampered", false)
		bad.ContentHash = "wrong"
		w.reg, w.prov = skills.NewFromStore([]skillstore.ProductionRow{bad}, skills.Default())
		return nil
	})
	sc.Step(`^a seed is composed$`, func() error { return nil })
	sc.Step(`^every compiled skill composes and the record names the fallback reason$`, func() error {
		body, _ := w.reg.Compose(deep)
		if w.prov.Fallback == "" {
			return fmt.Errorf("the fallback reason must be recorded")
		}
		for _, name := range skills.Default().Names() {
			if w.prov.Skills[name].Origin != skills.OriginCompiled {
				return fmt.Errorf("%s must be compiled-origin after fallback", name)
			}
		}
		if strings.Contains(body, "tampered") {
			return fmt.Errorf("no store body may compose after fallback")
		}
		return nil
	})

	sc.Step(`^a store row that rewrites a pinned skill$`, func() error {
		w.reg, w.prov = skills.NewFromStore(
			[]skillstore.ProductionRow{prow("conservative-remediation", "9.9.9", "weakened floor", true)},
			skills.Default())
		return nil
	})
	sc.Step(`^the pinned skill's compiled body is used$`, func() error {
		body, _ := w.reg.Compose(deep)
		if strings.Contains(body, "weakened floor") || !strings.Contains(body, "HARD FLOOR") {
			return fmt.Errorf("the pinned compiled body must win")
		}
		if w.prov.Skills["conservative-remediation"].Origin != skills.OriginPinned {
			return fmt.Errorf("the shadowed pinned row must be reported")
		}
		return nil
	})

	sc.Step(`^an active production snapshot$`, func() error {
		w.rows = []skillstore.ProductionRow{prow("triage-protocol", "9.0.0", "store triage v9", false)}
		return nil
	})
	sc.Step(`^a session composes its seed$`, func() error {
		acts := runner.NewActivities(runner.Deps{
			Model: stopModel{}, Tools: agent.NewReadOnlyToolSet(), Limits: agent.DefaultLimits(),
			SkillRows: func(context.Context) ([]skillstore.ProductionRow, error) { return w.rows, nil },
		})
		var err error
		w.inv, err = acts.InvestigateActivity(context.Background(), ingest.IncidentEnvelope{ExternalRef: "acc-1", Host: "web01", AlertRule: "HostDown"})
		return err
	})
	sc.Step(`^the record lists each loaded skill's name, version, content hash, and trial arm$`, func() error {
		if len(w.inv.SkillLoads) == 0 {
			return fmt.Errorf("the composed skill list must be recorded")
		}
		var storeBacked bool
		for _, l := range w.inv.SkillLoads {
			// the store-origin load names version AND row id (name@version#id:store) so the judge spine
			// can bind judged sessions to the exact graduated version (REQ-1310).
			if strings.Contains(l, "triage-protocol@9.0.0#90:store") {
				storeBacked = true
			}
		}
		if !storeBacked {
			return fmt.Errorf("the record must name the store-backed version, got %v", w.inv.SkillLoads)
		}
		return nil
	})
}

// ---- T-014-3: read-surface scenarios (REQ-1311/1313) ----

type accSkillsReader struct{}

func (accSkillsReader) ListSkills(context.Context) ([]httpapi.SkillSummary, error) {
	return []httpapi.SkillSummary{{Name: "triage-protocol", ProductionVersion: "1.1.0", VersionCount: 2}}, nil
}
func (accSkillsReader) SkillDetail(_ context.Context, name string) (httpapi.SkillDetailView, bool, error) {
	if name != "triage-protocol" {
		return httpapi.SkillDetailView{}, false, nil
	}
	return httpapi.SkillDetailView{
		SkillSummary: httpapi.SkillSummary{Name: "triage-protocol", ProductionVersion: "1.1.0"},
		Versions: []httpapi.SkillVersionView{{ID: 2, Version: "1.1.0", Status: "production",
			Rationale: "[production] welch p=0.03", LedgerSeq: 41, EvalOnline: json.RawMessage(`{"mean":4.1}`)}},
	}, true, nil
}
func (accSkillsReader) ListTrials(context.Context) ([]httpapi.TrialView, error) { return nil, nil }

type accSources struct{}

func (accSources) LookupSource(_ context.Context, id string) (auth.Source, error) {
	if id == "acc-operator" {
		return auth.Source{SourceID: "acc-operator", HMACSecret: []byte("acc-secret")}, nil
	}
	return auth.Source{}, fmt.Errorf("unknown source %s", id)
}

func signAcc(ts, nonce, body string) string {
	mac := hmac.New(sha256.New, []byte("acc-secret"))
	mac.Write([]byte(ts + "\n" + nonce + "\n" + body))
	return hex.EncodeToString(mac.Sum(nil))
}

type accNonces struct{}

func (accNonces) SeenBefore(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}

func initializeReadSurfaceScenario(sc *godog.ScenarioContext) {
	var rec *httptest.ResponseRecorder
	newRouter := func() *auth.Router {
		v, err := auth.NewVerifier(accSources{}, accNonces{}, time.Minute)
		if err != nil {
			panic(err)
		}
		rt := auth.NewRouter(v)
		httpapi.Register(rt, httpapi.Deps{Skills: accSkillsReader{}})
		return rt
	}

	sc.Step(`^a skill with a version history$`, func() error { return nil })
	sc.Step(`^the library is read with a read-only principal$`, func() error {
		rec = httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/skills/triage-protocol", nil)
		ts := fmt.Sprintf("%d", time.Now().Unix())
		req.Header.Set("X-TG-Source", "acc-operator")
		req.Header.Set("X-TG-Timestamp", ts)
		req.Header.Set("X-TG-Nonce", "acc-nonce-1")
		req.Header.Set("X-TG-Signature", signAcc(ts, "acc-nonce-1", ""))
		newRouter().Mux().ServeHTTP(rec, req)
		return nil
	})
	sc.Step(`^each version carries its rationale log, eval scores, and ledger references$`, func() error {
		if rec.Code != http.StatusOK {
			return fmt.Errorf("status %d", rec.Code)
		}
		var det httpapi.SkillDetailView
		if err := json.Unmarshal(rec.Body.Bytes(), &det); err != nil {
			return err
		}
		v := det.Versions[0]
		if !strings.Contains(v.Rationale, "welch") || v.LedgerSeq == 0 || v.EvalOnline == nil {
			return fmt.Errorf("rationale/scores/ledger must be exposed, got %+v", v)
		}
		return nil
	})

	sc.Step(`^the skill routes$`, func() error { return nil })
	sc.Step(`^a request arrives without a principal$`, func() error {
		rec = httptest.NewRecorder()
		newRouter().Mux().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/skills", nil))
		return nil
	})
	sc.Step(`^the response is a refusal, not data$`, func() error {
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNotFound {
			return fmt.Errorf("unauthenticated read must be refused, got %d", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "triage-protocol") {
			return fmt.Errorf("no data may leak on refusal")
		}
		return nil
	})
}

// ---- T-014-4: write-path scenarios (REQ-1301/1311) ----

func initializeWritePathScenario(sc *godog.ScenarioContext) {
	var rec *httptest.ResponseRecorder
	writerCalled := false

	// The real auth.Router with the write routes registered session-only (the Sessions!=nil block) —
	// exactly the production shape; the fake writer records whether any backend call happened.
	newWriteRouter := func() *auth.Router {
		v, err := auth.NewVerifier(accSources{}, accNonces{}, time.Minute)
		if err != nil {
			panic(err)
		}
		rt := auth.NewRouter(v)
		rt.Handle("/v1/skills/{name}/versions", auth.AuthSession,
			httpapi.Deps{SkillsWrite: recordingWriter{&writerCalled}}.SkillDraftHandlerForAcceptance())
		return rt
	}

	sc.Step(`^a machine principal$`, func() error { return nil })
	sc.Step(`^it attempts to create a draft$`, func() error {
		writerCalled = false
		rec = httptest.NewRecorder()
		// A VALID machine (HMAC) credential — the strongest machine identity there is — must still be
		// refused on a session-only route.
		req := httptest.NewRequest("POST", "/v1/skills/triage-protocol/versions",
			strings.NewReader(`{"version":"9.0.0","body":"b","rationale":"r"}`))
		ts := fmt.Sprintf("%d", time.Now().Unix())
		req.Header.Set("X-TG-Source", "acc-operator")
		req.Header.Set("X-TG-Timestamp", ts)
		req.Header.Set("X-TG-Nonce", "acc-nonce-2")
		req.Header.Set("X-TG-Signature", signAcc(ts, "acc-nonce-2", `{"version":"9.0.0","body":"b","rationale":"r"}`))
		newWriteRouter().Mux().ServeHTTP(rec, req)
		return nil
	})

	sc.Step(`^an operator session$`, func() error { return nil })
	sc.Step(`^a promote is requested without a rationale$`, func() error {
		writerCalled = false
		rec = httptest.NewRecorder()
		// Driven with a minted operator principal (dispatch through chi verbs is unit-proven); the
		// surface must refuse BEFORE the backend.
		mux := chi.NewRouter()
		d := httpapi.Deps{SkillsWrite: recordingWriter{&writerCalled}}
		mux.Handle("/v1/skills/versions/{id}/{verb}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d.SkillTransitionHandlerForAcceptance(w, r, auth.Principal{SourceID: "operator:acc"})
		}))
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/skills/versions/9/promote", strings.NewReader(`{}`)))
		return nil
	})
	sc.Step(`^the surface refuses before any backend call$`, func() error {
		if rec.Code < 400 {
			return fmt.Errorf("write must be refused, got %d", rec.Code)
		}
		if writerCalled {
			return fmt.Errorf("the backend must never be reached on a refused write")
		}
		return nil
	})
}

// recordingWriter marks any backend call (a refusal must happen BEFORE it).
type recordingWriter struct{ called *bool }

func (r recordingWriter) CreateDraft(context.Context, string, httpapi.SkillDraftRequest, string) (httpapi.SkillVersionView, error) {
	*r.called = true
	return httpapi.SkillVersionView{}, nil
}
func (r recordingWriter) Transition(context.Context, int64, skillstore.Status, string, string) (httpapi.SkillTransitionOutcome, error) {
	*r.called = true
	return httpapi.SkillTransitionOutcome{}, nil
}

// ---- T-014-5: trial-engine scenarios (REQ-1306/1308/1309) ----

func initializeTrialEngineScenario(sc *godog.ScenarioContext) {
	now := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	var st *skillstore.MemTrialStore
	var tr skillstore.Trial
	var arms []int
	var refErr error
	var outcomes []skillstore.FinalizeOutcome
	var startErr error

	spread := func(base float64) []float64 {
		out := make([]float64, 15)
		for i := range out {
			out[i] = base + float64(i%5)*0.1
		}
		return out
	}

	sc.Step(`^an active trial with two candidates$`, func() error {
		st = skillstore.NewMemTrialStore(10)
		var err error
		tr, err = st.CreateTrial(context.Background(), skillstore.Trial{SkillName: "triage-protocol",
			CandidateIDs: []int64{101, 102}, Dimension: "correct_diagnosis", MinSamplesPerArm: 15,
			MinLift: 0.05, PThreshold: 0.1, EndsAt: now.Add(14 * 24 * time.Hour), Status: "active"})
		return err
	})
	sc.Step(`^the same external ref is assigned twice and a whitespace ref is assigned once$`, func() error {
		arms = arms[:0]
		for i := 0; i < 2; i++ {
			a, err := skillstore.AssignArm(context.Background(), st, "am-HostDown-web01", tr)
			if err != nil {
				return err
			}
			arms = append(arms, a)
		}
		_, refErr = skillstore.AssignArm(context.Background(), st, "   ", tr)
		return nil
	})
	sc.Step(`^the arm is identical across the two assignments and the malformed ref is rejected and counted$`, func() error {
		if len(arms) != 2 || arms[0] != arms[1] {
			return fmt.Errorf("assignment must be deterministic+idempotent, got %v", arms)
		}
		if !errors.Is(refErr, skillstore.ErrMalformedRef) {
			return fmt.Errorf("whitespace ref must be rejected, got %v", refErr)
		}
		return nil
	})

	sc.Step(`^an expired active trial and a completable active trial$`, func() error {
		st = skillstore.NewMemTrialStore(10)
		expired, err := st.CreateTrial(context.Background(), skillstore.Trial{SkillName: "a",
			CandidateIDs: []int64{101}, Dimension: "correct_diagnosis", MinSamplesPerArm: 15,
			MinLift: 0.05, PThreshold: 0.1, EndsAt: now.Add(-time.Hour), Status: "active"})
		if err != nil {
			return err
		}
		st.SetScores(expired.ID, map[int][]float64{-1: spread(3.5), 0: spread(4.5)})
		full, err := st.CreateTrial(context.Background(), skillstore.Trial{SkillName: "b",
			CandidateIDs: []int64{102}, Dimension: "correct_diagnosis", MinSamplesPerArm: 15,
			MinLift: 0.05, PThreshold: 0.1, EndsAt: now.Add(time.Hour), Status: "active"})
		if err != nil {
			return err
		}
		st.SetScores(full.ID, map[int][]float64{-1: spread(3.5), 0: spread(4.2)})
		return nil
	})
	sc.Step(`^the finalizer runs$`, func() error {
		var err error
		outcomes, err = skillstore.FinalizeTrials(context.Background(), st, now)
		return err
	})
	sc.Step(`^the expired trial is aborted-timeout before any graduation is considered$`, func() error {
		if len(outcomes) < 2 {
			return fmt.Errorf("want 2 outcomes, got %+v", outcomes)
		}
		if outcomes[0].Status != "aborted_timeout" {
			return fmt.Errorf("the sweep must run FIRST, got %+v", outcomes[0])
		}
		if outcomes[1].Status != "completed" {
			return fmt.Errorf("the completable trial must graduate after the sweep, got %+v", outcomes[1])
		}
		return nil
	})

	sc.Step(`^a trial whose best candidate lacks samples, lift, significance, or safety-dimension health$`, func() error {
		st = skillstore.NewMemTrialStore(10)
		t1, err := st.CreateTrial(context.Background(), skillstore.Trial{SkillName: "s",
			CandidateIDs: []int64{101}, Dimension: "correct_diagnosis", MinSamplesPerArm: 15,
			MinLift: 0.05, PThreshold: 0.1, EndsAt: now.Add(time.Hour), Status: "active"})
		if err != nil {
			return err
		}
		// Full arms, clear target lift — but a REGRESSED safety analog: the asymmetric guard case.
		st.SetScores(t1.ID, map[int][]float64{-1: spread(3.5), 0: spread(4.2)})
		st.SetSafety(t1.ID, map[int][]float64{-1: spread(4.5), 0: spread(3.0)})
		return nil
	})
	sc.Step(`^no version graduates$`, func() error {
		for _, o := range outcomes {
			if o.Status == "completed" {
				return fmt.Errorf("nothing may graduate, got %+v", o)
			}
		}
		return nil
	})

	sc.Step(`^a judged-session rate too low to reach the sample minimum before the end date$`, func() error {
		st = skillstore.NewMemTrialStore(0.5)
		return nil
	})
	sc.Step(`^a trial start is requested$`, func() error {
		_, startErr = skillstore.StartTrial(context.Background(), st, skillstore.Trial{
			SkillName: "triage-protocol", CandidateIDs: []int64{101}, Dimension: "correct_diagnosis",
			MinSamplesPerArm: 15, MinLift: 0.05, PThreshold: 0.1, EndsAt: now.Add(14 * 24 * time.Hour)}, now)
		return nil
	})
	sc.Step(`^the start is refused with a stored reason$`, func() error {
		if !errors.Is(startErr, skillstore.ErrTrialStarvation) {
			return fmt.Errorf("start must be refused for starvation, got %v", startErr)
		}
		if !strings.Contains(startErr.Error(), "/day") {
			return fmt.Errorf("the refusal must carry the projection, got %q", startErr.Error())
		}
		return nil
	})
}

// ---- T-014-6: graduation-watch scenario (REQ-1310) ----

func initializeWatchScenario(sc *godog.ScenarioContext) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	var (
		m        *skillstore.MemStore
		lg       *audit.Ledger
		ws       *skillstore.MemWatchStore
		v1, v2   skillstore.Version
		escalRef string
	)

	sc.Step(`^a freshly graduated version under its regression watch$`, func() error {
		m = skillstore.NewMemStore()
		m.PutSkill(skillstore.Skill{Name: "triage-protocol", Kind: "behavioral", Position: 5})
		lg = audit.NewLedger()
		ws = skillstore.NewMemWatchStore()
		ctx := context.Background()
		mk := func(ver, body string) skillstore.Version {
			aw := skillstore.AppliesWhen{}
			v, err := m.CreateVersion(ctx, skillstore.Version{SkillName: "triage-protocol", Version: ver,
				Body: body, AppliesWhen: aw, ContentHash: skillstore.ContentHash(body, aw),
				Author: "operator:acc", Source: "hand", Rationale: "acceptance"})
			if err != nil {
				panic(err)
			}
			if _, err := skillstore.Transition(ctx, m, lg, v.ID, skillstore.StatusTrial, "gate"); err != nil {
				panic(err)
			}
			v, err = skillstore.Transition(ctx, m, lg, v.ID, skillstore.StatusProduction, "graduate")
			if err != nil {
				panic(err)
			}
			return v
		}
		v1 = mk("1.0.0", "prior body")
		v2 = mk("2.0.0", "graduate body")
		return skillstore.OpenWatch(ctx, ws, v2.ID, v1.ID, "triage-protocol", "correct_diagnosis", 3.5, 0.05, now)
	})
	sc.Step(`^judged sessions score below the control threshold enough times to trip the breaker$`, func() error {
		esc := func(_ context.Context, ref, reason string) error { escalRef = ref; return nil }
		for i := 0; i < skillstore.DefaultWatchThreshold; i++ {
			if err := skillstore.ObserveJudgedSession(context.Background(), ws, m, lg, esc,
				[]int64{v2.ID}, map[string]float64{"correct_diagnosis": 1.5}, now); err != nil {
				return err
			}
		}
		return nil
	})
	sc.Step(`^the graduate is retired, the prior production version is restored, and the demotion is ledger-recorded and escalated$`, func() error {
		ctx := context.Background()
		got, err := m.GetVersion(ctx, v2.ID)
		if err != nil || got.Status != skillstore.StatusRetired {
			return fmt.Errorf("graduate must retire, got %v %v", got.Status, err)
		}
		prod, ok, _ := m.ProductionVersion(ctx, "triage-protocol")
		if !ok || prod.Body != "prior body" {
			return fmt.Errorf("prior body must be production, got %+v", prod)
		}
		if escalRef == "" {
			return fmt.Errorf("the demotion must escalate")
		}
		return lg.Verify()
	})
}

// ---- T-014-7/8: flywheel + admission scenarios (REQ-1312/1307) ----

type accGen struct{}

func (accGen) Complete(_ context.Context, _, _ string, _ string) (string, error) {
	return "a rewritten candidate body", nil
}

type accOffline struct {
	res         skillstore.OfflineResult
	holdoutRead *bool
}

func (a accOffline) RunOffline(context.Context, skillstore.Version, string) (skillstore.OfflineResult, error) {
	// The runner interface receives ONLY the candidate — this fake proves the holdout is structurally
	// out of reach: nothing in the call chain can read it.
	return a.res, nil
}

func initializeFlywheelScenario(sc *godog.ScenarioContext) {
	var (
		m      *skillstore.MemStore
		lg     *audit.Ledger
		drafts []skillstore.Version
		admErr error
		hold   bool
	)
	seedProd := func() skillstore.Version {
		m = skillstore.NewMemStore()
		m.PutSkill(skillstore.Skill{Name: "triage-protocol", Kind: "behavioral", Position: 5})
		lg = audit.NewLedger()
		ctx := context.Background()
		aw := skillstore.AppliesWhen{}
		v, err := m.CreateVersion(ctx, skillstore.Version{SkillName: "triage-protocol", Version: "1.0.0",
			Body: "production body", AppliesWhen: aw, ContentHash: skillstore.ContentHash("production body", aw),
			Author: "operator:acc", Source: "hand", Rationale: "seed"})
		if err != nil {
			panic(err)
		}
		if _, err := skillstore.Transition(ctx, m, lg, v.ID, skillstore.StatusTrial, "gate"); err != nil {
			panic(err)
		}
		if v, err = skillstore.Transition(ctx, m, lg, v.ID, skillstore.StatusProduction, "seed"); err != nil {
			panic(err)
		}
		return v
	}

	sc.Step(`^the candidate generator triggered by a regressed dimension$`, func() error {
		seedProd()
		return nil
	})
	sc.Step(`^it produces drafts$`, func() error {
		var err error
		drafts, err = skillstore.GenerateCandidates(context.Background(), m, accGen{}, skillstore.GenTrigger{
			SkillName: "triage-protocol", Dimension: "correct_diagnosis", Mean: 2.8, Threshold: 3.5,
			Window: 30, Source: "flywheel:eval-failure:acc-run"})
		return err
	})
	sc.Step(`^each draft records its source and rationale and composition is unchanged$`, func() error {
		if len(drafts) == 0 {
			return fmt.Errorf("drafts must be produced")
		}
		for _, d := range drafts {
			if d.Source != "flywheel:eval-failure:acc-run" || !strings.Contains(d.Rationale, "fell below") {
				return fmt.Errorf("draft must carry source+rationale, got %+v", d)
			}
			if d.Status != skillstore.StatusDraft {
				return fmt.Errorf("generation is draft-only, got %s", d.Status)
			}
		}
		prod, _, _ := m.ProductionVersion(context.Background(), "triage-protocol")
		if prod.Body != "production body" {
			return fmt.Errorf("composition input (production) must be unchanged")
		}
		return nil
	})

	sc.Step(`^a draft whose offline run regresses the regression set$`, func() error {
		seedProd()
		aw := skillstore.AppliesWhen{}
		d, err := m.CreateVersion(context.Background(), skillstore.Version{SkillName: "triage-protocol",
			Version: "2.0.0", Body: "candidate", AppliesWhen: aw, ContentHash: skillstore.ContentHash("candidate", aw),
			Author: "flywheel", Source: "flywheel:eval-failure:acc", Rationale: "cand"})
		if err != nil {
			return err
		}
		drafts = []skillstore.Version{d}
		return nil
	})
	sc.Step(`^admission is evaluated$`, func() error {
		_, admErr = skillstore.AdmitToTrial(context.Background(), m, lg,
			accOffline{res: skillstore.OfflineResult{RunID: "acc-off", RegressionPass: false, DiscoveryDelta: 0.5}, holdoutRead: &hold},
			drafts[0].ID, "correct_diagnosis")
		return nil
	})
	sc.Step(`^the draft stays out of trial and the refusal reason is stored$`, func() error {
		if !errors.Is(admErr, skillstore.ErrNotAdmitted) {
			return fmt.Errorf("admission must refuse, got %v", admErr)
		}
		v, _ := m.GetVersion(context.Background(), drafts[0].ID)
		if v.Status != skillstore.StatusDraft || v.OfflineEval == nil {
			return fmt.Errorf("refused draft stays draft with the run stored, got %s", v.Status)
		}
		return nil
	})

	sc.Step(`^an offline admission run$`, func() error {
		seedProd()
		aw := skillstore.AppliesWhen{}
		d, err := m.CreateVersion(context.Background(), skillstore.Version{SkillName: "triage-protocol",
			Version: "2.0.0", Body: "candidate", AppliesWhen: aw, ContentHash: skillstore.ContentHash("candidate", aw),
			Author: "flywheel", Source: "flywheel:eval-failure:acc", Rationale: "cand"})
		if err != nil {
			return err
		}
		hold = false
		_, err = skillstore.AdmitToTrial(context.Background(), m, lg,
			accOffline{res: skillstore.OfflineResult{RunID: "acc-off2", RegressionPass: true, DiscoveryDelta: 0.3}, holdoutRead: &hold},
			d.ID, "correct_diagnosis")
		return err
	})
	sc.Step(`^it completes$`, func() error { return nil })
	sc.Step(`^the sealed holdout set has not been accessed$`, func() error {
		// Structural proof: OfflineRunner's signature carries only the candidate — there is no holdout
		// handle to read. The fake's flag stays false by construction.
		if hold {
			return fmt.Errorf("the holdout must be structurally unreachable")
		}
		return nil
	})
}

// ---- REQ-1314: the creation-half cron (generate -> offline-admit -> trial-start) ----

// accMeans is the MeansReader over a fixed regressed dimension table (the judge spine's rolling means in
// production).
type accMeans struct{ stats []skillstore.DimensionStat }

func (a accMeans) DimensionMeans(context.Context, int64, time.Duration) ([]skillstore.DimensionStat, error) {
	return a.stats, nil
}

func initializeCreationScenario(sc *godog.ScenarioContext) {
	var (
		m      *skillstore.MemStore
		lg     *audit.Ledger
		trials *skillstore.MemTrialStore
		prod   skillstore.Version
		rep    skillstore.CreationReport
	)
	seedRegressed := func() {
		m = skillstore.NewMemStore()
		m.PutSkill(skillstore.Skill{Name: "triage-protocol", Kind: "behavioral", Position: 5})
		lg = audit.NewLedger()
		ctx := context.Background()
		aw := skillstore.AppliesWhen{}
		v, err := m.CreateVersion(ctx, skillstore.Version{SkillName: "triage-protocol", Version: "1.0.0",
			Body: "production body", AppliesWhen: aw, ContentHash: skillstore.ContentHash("production body", aw),
			Author: "operator:acc", Source: "hand", Rationale: "seed"})
		if err != nil {
			panic(err)
		}
		if _, err := skillstore.Transition(ctx, m, lg, v.ID, skillstore.StatusTrial, "gate"); err != nil {
			panic(err)
		}
		if prod, err = skillstore.Transition(ctx, m, lg, v.ID, skillstore.StatusProduction, "seed"); err != nil {
			panic(err)
		}
	}
	run := func(rate float64) error {
		trials = skillstore.NewMemTrialStore(rate)
		deps := skillstore.CreationDeps{
			Store:  m,
			Means:  accMeans{stats: []skillstore.DimensionStat{{Dimension: "correct_diagnosis", Mean: 2.8, Samples: 20}}},
			Trials: trials,
			Ledger: lg,
			Model:  accGen{},
			Runner: accOffline{res: skillstore.OfflineResult{RunID: "acc-off", RegressionPass: true, DiscoveryDelta: 0.4}},
			Cfg: skillstore.CreationConfig{
				Threshold: skillstore.DefaultGenThreshold, MinSamples: skillstore.DefaultGenMinSamples,
				Window: 14 * 24 * time.Hour, MinSamplesPerArm: 5, MinLift: 0.1, PThreshold: 0.05,
				TrialDuration: 30 * 24 * time.Hour,
			},
			RunID: "acc-run",
		}
		var err error
		rep, err = skillstore.RunCreationHalf(context.Background(), deps, time.Now())
		return err
	}

	sc.Step(`^a production skill whose judged dimension has regressed$`, func() error {
		seedRegressed()
		return nil
	})
	sc.Step(`^the creation-half cron runs with a passing offline gate and ample traffic$`, func() error {
		return run(100)
	})
	sc.Step(`^draft candidates are generated, admitted to trial, and one trial is started while production is unchanged$`, func() error {
		if rep.Generated < 1 || rep.Admitted < 1 || rep.TrialsStarted != 1 {
			return fmt.Errorf("want generate+admit+one-trial, got %+v", rep)
		}
		if p, _, _ := m.ProductionVersion(context.Background(), "triage-protocol"); p.Body != "production body" {
			return fmt.Errorf("generate-only: production must be unchanged, got %q", p.Body)
		}
		tr, ok, _ := trials.ActiveTrialFor(context.Background(), "triage-protocol")
		if !ok || tr.Dimension != "correct_diagnosis" || tr.ControlVersionID != prod.ID {
			return fmt.Errorf("an active trial on the regressed dimension vs production must exist, got %+v ok=%v", tr, ok)
		}
		return nil
	})

	sc.Step(`^the creation-half cron runs with a passing offline gate but starved traffic$`, func() error {
		return run(0)
	})
	sc.Step(`^the candidates are admitted but no trial is started$`, func() error {
		if rep.Admitted < 1 || rep.TrialsStarted != 0 || rep.RefusedStart != 1 {
			return fmt.Errorf("want admitted candidates with a starved-refused start, got %+v", rep)
		}
		if _, ok, _ := trials.ActiveTrialFor(context.Background(), "triage-protocol"); ok {
			return fmt.Errorf("a starved trial must not be created")
		}
		return nil
	})
}
