// Package eval is TG's grounding/quality evaluation harness (task #26, first iteration: MEASUREMENT).
//
// It runs a corpus of realistic incidents through the REAL Runner (read-only, mutation OFF) and scores each
// resulting triage session with an LLM-as-judge on five quality dimensions — the same shape the predecessor
// (claude-gateway) uses, plus TG's own grounding signals (a committed, falsifiable prediction). The pure
// logic here (corpus load, judge-response parsing, aggregation) is unit-tested in CI; the actual run against
// the live model gateway is a build-gated integration test (eval_integration_test.go) executed ON the box.
//
// The next iteration turns this measurement into the auto-patching flywheel (3 prompt variants + a control,
// deterministic assignment, Welch t-test, promote the winner) — see README.md.
package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/proposal"
	retrievaleval "github.com/territory-grounder/grounder/eval/retrieval"
)

// isTransientModelErr reports whether a gateway error is worth retrying: a transport drop (EOF), a timeout, a
// rate-limit, a provider 5xx, or an empty (thinking-only) completion. A bad_request/auth is PERMANENT (retrying
// cannot help). An unrecognized error shape is treated as transient — the on-box eval must survive gateway
// blips under the full-corpus judge-call burst rather than silently pool a short (degraded) scorecard.
func isTransientModelErr(err error) bool {
	if err == nil {
		return false
	}
	var me *model.ModelError
	if errors.As(err, &me) {
		switch me.Class {
		case "bad_request", "auth":
			return false
		}
	}
	return true
}

// retryComplete calls fn up to maxTries, backing off between attempts ONLY on a transient model error; it
// returns on the first success or permanent error. backoff(attempt) is the injectable inter-attempt wait — a
// real sleep in the harness, a no-op in tests. This is the R1 fix for the full-corpus judge phase, which
// previously dropped a session's judgment on any single transient EOF (the burst of 20 rapid judge calls
// degraded the whole run to "0 sessions judged").
func retryComplete(maxTries int, backoff func(attempt int), fn func() (string, error)) (string, error) {
	var raw string
	var err error
	for attempt := 1; attempt <= maxTries; attempt++ {
		if raw, err = fn(); err == nil || !isTransientModelErr(err) {
			return raw, err
		}
		if attempt < maxTries && backoff != nil {
			backoff(attempt)
		}
	}
	return raw, err
}

// Incident is one corpus entry — an IncidentEnvelope-shaped realistic alert (real NL host + rule).
type Incident struct {
	ExternalRef string `json:"external_ref"`
	SourceID    string `json:"source_id"`
	AlertRule   string `json:"alert_rule"`
	Host        string `json:"host"`
	Severity    string `json:"severity"`
	Site        string `json:"site"`
	Summary     string `json:"summary"`
	// Expected labels the outcome a correct agent produces on THIS incident: "propose" (a remediation is
	// warranted), "stand-down" (no action is the right call), or "escalate". Without labels a raw
	// proposal_rate cannot distinguish "correctly stood down on a stale corpus" from "cannot propose" —
	// the ambiguity that let the 2026-07-25 collapse to 0% proposals read as a quality IMPROVEMENT.
	Expected string `json:"expected,omitempty"`
	// ExpectedOpClass optionally names the op-class a correct proposal uses (breadth/aim signal).
	ExpectedOpClass string `json:"expected_op_class,omitempty"`
	// ToolFixtures arms this incident with CAPTURED tool outputs (the B4a fixture arm, fixtures.go): keyed
	// by FixtureKey(tool, host), each entry is served verbatim when the agent invokes that tool on that
	// host; any uncaptured (tool, host) gets the family's faithful "no data" shape. A fixture-armed
	// incident makes NO live network calls and SKIPS the corpus-freshness pass — it is stale-proof by
	// construction, which is the point: the live estate healing can never again zero the propose supply
	// (the 2026-07-30 trend failure). ONLY expected-propose incidents may carry fixtures (LoadCorpus fails
	// closed otherwise): stand-down/escalate correctness IS live-groundedness, so those stay live-armed.
	ToolFixtures map[string]FixtureResult `json:"tool_fixtures,omitempty"`
	// ConfighashChanged arms the TG-466 slice-2 confighash seam for THIS incident with a deterministic
	// in-process answer (TG-533: the gate was structurally blind to the armed intrusion-suspicious path —
	// no incident produced a confirmed observed mutation, so no run at any count could tell whether the
	// escalation misfires). nil/absent leaves the attribution seams UNWIRED for the session — ship-dark
	// parity, byte-identical behavior for every existing incident. true: the harness supplies a
	// covered-but-empty actor reading plus a host-scoped changed=true answer, and the CORRECT outcome is
	// attributed-suspicious → POLL_PAUSE (must-fire). false: same covered-but-empty reading, changed=false —
	// the escalation must NOT fire (the spurious-suspicion control; the check is two-sided by design).
	// The eval never touches guest_config_baseline/Postgres or a live PVE: the gate stays hermetic.
	ConfighashChanged *bool `json:"confighash_changed,omitempty"`
}

// SecurityCheck grades the mechanical TG-466 escalation expectation over a run's sessions (TG-533). It is
// deliberately OUTSIDE the judged dims and the overall formula (widening a judged denominator for a
// taxonomy check is the diagnosis_grounded trap): a session carrying SecurityExpected must have
// Security == *SecurityExpected — must-fire on the confirmed-observed-mutation incident AND must-NOT-fire
// on its changed=false control, so the check cannot be satisfied by escalating more. The checked count is
// returned so a run where no incident opted in reads "0 checked", never a vacuous pass ("found nothing"
// and "found nothing wrong" are different claims). An arm built before the opt-in field existed parses
// the corpus without it, carries no SecurityExpected, and is honestly out of scope (checked=0).
// UNREACHED (2026-08-25, learned from the check's first two on-box contacts): TG-466's escalation is
// attribution OF A PROPOSAL — a session that never proposed never reaches the attribute step, in the eval
// and in production alike. A must-fire session with Proposed=false is therefore reported as UNREACHED
// (its own counted state), never a violation: the first two full-gate runs turned exactly that state into
// t.Errorf on BOTH arms, which the harness read as degraded arms and ABORTED — a gate-DoS riding the
// corpus-wide proposal collapse that the trend-watch already owns (base arm proposal_rate 0.13 < 0.25).
// The propose-supply failure stays gated where it belongs (proposal_recall / the rate floors); THIS check
// grades only what it can reach, and says so with a denominator.
func SecurityCheck(sessions []Session) (checked int, unreached []string, violations []string) {
	for _, s := range sessions {
		if s.SecurityExpected == nil {
			continue
		}
		checked++
		if *s.SecurityExpected && !s.Proposed {
			unreached = append(unreached,
				s.Ref+": no proposal this run — the attribute step was never reached (outcome="+s.Outcome+")")
			continue
		}
		if s.Security != *s.SecurityExpected {
			want := "must NOT escalate (changed=false control)"
			if *s.SecurityExpected {
				want = "must escalate attributed-suspicious/POLL_PAUSE (confirmed observed mutation)"
			}
			violations = append(violations,
				s.Ref+": "+want+"; got attribution="+s.Attribution+" band="+s.Band+" err="+s.Err)
		}
	}
	return checked, unreached, violations
}

// Session is the captured outcome of running one incident through the Runner (read-only).
type Session struct {
	Ref        string   `json:"ref"`
	AlertRule  string   `json:"alert_rule"`
	Host       string   `json:"host"`
	Severity   string   `json:"severity"`
	Band       string   `json:"band"`               // AUTO | AUTO_NOTICE | POLL_PAUSE | ""
	Proposed   bool     `json:"proposed"`           // did the agent propose an action?
	ActionID   string   `json:"action_id"`          // the sealed action id (if proposed+gated)
	OpClass    string   `json:"op_class"`           // the proposed action's op-class (fault-class breadth signal, A5)
	StepCount  int      `json:"step_count"`         // the agent loop's investigation-cycle count (decision-steps signal, A6a)
	Prediction string   `json:"prediction"`         // the committed consequence prediction (grounding signal)
	Predicted  bool     `json:"predicted"`          // was a machine prediction committed (falsifiable) ?
	Evidence   []string `json:"evidence"`           // cited evidence ids (INV-11 silent-cognition guard)
	Conclusion string   `json:"conclusion"`         // the agent's grounded no-action rationale on a stop (REQ-1008)
	Decisions  []string `json:"decisions"`          // governance-ledger decision labels for this session
	Outcome    string   `json:"outcome"`            // the RunnerResult outcome string
	Mutated    bool     `json:"mutated"`            // MUST be false (mutation OFF)
	Expected   string   `json:"expected,omitempty"` // the corpus label (Incident.Expected), carried for recall scoring
	// StaleVsLive marks a labeled incident whose LIVE estate evidence contradicted it at run time (device
	// disabled/missing, no alert firing). The session still runs and is judged (a grounded stand-down on a
	// stale incident is CORRECT and earns its judged scores), but it is excluded from the label-aware
	// recall/precision denominators — scoring it as an agent miss is how corpus drift once read as a
	// capability collapse.
	StaleVsLive bool `json:"stale_vs_live,omitempty"`
	// FixtureArmed marks a session served from the incident's captured fixtures (the deterministic B4a
	// arm, fixtures.go) instead of the live estate tools. Carried onto the scorecard (FixtureArmed count)
	// so recall always discloses how much of its supply came from the deterministic arm.
	FixtureArmed bool `json:"fixture_armed,omitempty"`
	// Attribution is the actor-attribution taxonomy the attribute step resolved (RunnerResult.Attribution).
	// Security is the REAL security-disposition bit (RunnerResult.SecurityEscalated — never a taxonomy+band
	// proxy, which a generic-escalate fallback could satisfy for the wrong reason). SecurityExpected carries
	// the incident's ConfighashChanged opt-in so the mechanical SecurityCheck can grade must-fire AND
	// must-not-fire without re-reading the corpus (TG-533).
	Attribution      string `json:"attribution,omitempty"`
	Security         bool   `json:"security,omitempty"`
	SecurityExpected *bool  `json:"security_expected,omitempty"`
	Err              string `json:"err,omitempty"`
	// Diagnosis is the session's typed, source-bound CLAIM (core/proposal, TG-201) exactly as the agent loop
	// bound it against the orchestrator's captured ToolResult ids. Carried so the offline scorecard reports
	// the same deterministic diagnosis_grounded axis the live judge cron writes — one measurement of the
	// claim, in both planes, never a plane where it silently goes ungraded. DiagnosisRecorded says the run
	// produced the field at all (always true for a session this harness ran; false for a replayed capture
	// from before TG-201, which must read N/A rather than 1).
	Diagnosis         proposal.Diagnosis `json:"diagnosis,omitzero"`
	DiagnosisRecorded bool               `json:"diagnosis_recorded,omitempty"`
	// Trajectory is the ordered tool path (agent.TrajectoryStep, ArgsKey already digested by the runner) the
	// session's agent loop walked. Carried so the deterministic trajectory_grounded axis (TG-525) grades the
	// ordered path with the SAME veto the runtime uses to halt a stuck agent. Empty for a pre-feature or
	// DB-replayed capture (tools/rejudge), which reads N/A on the axis — never floored, like diagnosis_grounded.
	Trajectory []agent.TrajectoryStep `json:"trajectory,omitempty"`
	// TrajectoryJudgeScore is the LLM ordered-path judge's 1..5 grade of the trajectory SHAPE (TG-525 slice 2),
	// computed by the harness via the same model channel as the main judge. 0 when no grade was produced (no
	// gateway, or a pre-feature / DB-replayed capture). It is the value the deterministic agent.TrajectoryVeto
	// OVERRIDES in trajectoryScore — the judge is not the authority. Read by no gate.
	TrajectoryJudgeScore  int    `json:"trajectory_judge_score,omitempty"`
	TrajectoryJudgeReason string `json:"trajectory_judge_reason,omitempty"`
}

// Dimensions are the five quality axes the judge scores 1..5 — the canonical list lives in core/judge
// (the durable judge cron scores the same axes; ONE judge, never two drifting copies).
var Dimensions = judge.Dimensions

// Score is one judged session: a 1..5 per dimension + a one-line rationale.
type Score struct {
	Ref     string         `json:"ref"`
	Scores  map[string]int `json:"scores"`
	Comment string         `json:"comment"`
}

// Scorecard is the aggregate over a run.
type Scorecard struct {
	// RubricVersion — see gate.Scorecard: the rubric identity these sessions were judged under (TG-194).
	RubricVersion string `json:"rubric_version,omitempty"`
	N             int    `json:"n"`
	Judged        int    `json:"judged"` // sessions the judge actually scored — < N means a DEGRADED run (integrity signal)
	Errors        int    `json:"errors"` // sessions whose triage workflow errored — > 0 means a DEGRADED run (integrity signal)
	// FirstErr is the FIRST session error string (TG-493): the integrity abort printed only "N errored", and the
	// aggregate carried no per-session CAUSE (the 844-byte-scorecard trap) — a wedge read like contention when it
	// was really a dropped tunnel. Surfacing one concrete cause makes the next wedge name itself.
	FirstErr       string         `json:"first_err,omitempty"`
	Bands          map[string]int `json:"bands"`
	ProposalRate   float64        `json:"proposal_rate"`
	PredictionRate float64        `json:"prediction_rate"` // fraction with a committed falsifiable prediction
	// AutonomyRate is benchmark axis A4 (autonomy rate, docs/BENCHMARK-AXES.md): the fraction of the corpus the
	// agent would resolve WITHOUT a human — i.e. it lands an auto-actuating band (AUTO | AUTO_NOTICE), not a
	// human-gated POLL_PAUSE nor a no-action stop. Reported (rides along for the change-gate diff), not a hard
	// gate bar: a change that is correctly MORE conservative lowers autonomy legitimately, so it must not fail a
	// merge — the number is a trend signal, the safety dims are the bars.
	AutonomyRate float64 `json:"autonomy_rate"`
	// FaultClasses / FaultClassBreadth are benchmark axis A5 (fault-class breadth, docs/BENCHMARK-AXES.md): the
	// set of DISTINCT op-classes the agent proposed a fix for across the corpus, and its size. Reported (rides
	// along for the change-gate diff), not a hard bar — a change that correctly narrows to fewer, safer op-classes
	// lowers breadth legitimately. A change that teaches the agent a NEW fault-class raises it (+A5).
	FaultClasses      []string `json:"fault_classes"`
	FaultClassBreadth int      `json:"fault_class_breadth"`
	// MeanDecisionSteps is benchmark axis A6a (decision STEPS): the mean number of read-only investigation
	// cycles the agent loop ran per session. Reported (rides along for the change-gate diff), not a hard bar —
	// FEWER steps is the win, so this is the one metric where a DROP is good; the change-gate prints the Δ.
	// NOTE: wall-clock latency is model-gateway-dominated and noisy, so it is deliberately NOT gated here; the
	// cycle count is the deterministic, agent-controlled signal. TG-205 split the axis rather than conflating
	// them: fewer steps does NOT mean faster (the same two cycles cost seconds on the fast tier and minutes on
	// the reasoning tier), and the wall-clock half is measured off session_triage.decision_ms and REPORTED by
	// the live scorer (cmd/axisscore, A6b).
	MeanDecisionSteps float64 `json:"mean_decision_steps"`
	// ResolutionRecall / ResolutionRecallCeiling / ResolutionRecallOfFindable are TG-491's leave-one-out
	// retrieval-recall over the SHIPPED seed corpus (deploy/knowledge/corpus.seed.json), surfaced onto the
	// scorecard by TG-507 so retrieval quality is visible in the deploy-gating change-gate diff (beside
	// A4/A5/A6a). Each incident's OWN recorded human Resolution is the un-invented, non-circular ground truth
	// (the retriever never scores on it) — no operator labels, no gateway, no judge. Reported (rides along for
	// the change-gate diff), NEVER a hard bar: the eval gate does not consult these, and the retriever's own CI
	// ratchet (eval/retrieval/resolution_recall_test.go, floor 0.90) is where a real regression FAILs. OfFindable
	// — of the incidents whose fix a peer makes recoverable, the fraction the retriever surfaced — is the clean
	// quality number; Ceiling = the best any retriever could do over this corpus (context, not a score). All
	// three stay zero when the shipped seed is not locatable from the caller's CWD (a reported metric degrades
	// to "not reported", never to a failure).
	ResolutionRecall           float64 `json:"resolution_recall"`
	ResolutionRecallCeiling    float64 `json:"resolution_recall_ceiling"`
	ResolutionRecallOfFindable float64 `json:"resolution_recall_of_findable"`
	MutationCount              int     `json:"mutation_count"` // MUST be 0 (mutation OFF)
	// ProposalRecall / StanddownPrecision score against the corpus's Expected labels: recall = proposals on
	// expected-propose incidents over the expected-propose count (did the agent act when action was
	// warranted?); precision = of the sessions the agent stood down, the fraction whose label agrees
	// (did it only stand down when that was right?). The *N fields are the denominators — a gate floor is
	// meaningful only when its denominator is > 0, so both are published, never inferred.
	ExpectedProposeN   int     `json:"expected_propose_n"`
	ProposalRecall     float64 `json:"proposal_recall"`
	LabeledStanddownN  int     `json:"labeled_standdown_n"` // labeled sessions the agent stood down on
	StanddownPrecision float64 `json:"standdown_precision"`
	StaleExcluded      int     `json:"stale_excluded"` // labeled sessions excluded because live evidence contradicted the corpus
	// FixtureArmed is the number of sessions served from captured fixtures (the deterministic B4a arm) —
	// published alongside StaleExcluded so a scorecard always discloses how much of its proposal_recall
	// came from the deterministic arm vs the live estate. Fixture-armed sessions COUNT in recall (they are
	// the measurable propose supply; that is the arm's purpose), so the disclosure, not an exclusion, is
	// the honesty mechanism here.
	FixtureArmed int                `json:"fixture_armed"`
	DimMeans     map[string]float64 `json:"dim_means"`
	// DimSamples is the number of judged samples behind each dimension mean. 0 means the mean is the
	// imputed AbstentionFloor, not a measurement — published so a floored dimension can never masquerade
	// as a measured one.
	DimSamples map[string]int `json:"dim_samples,omitempty"`
	Overall    float64        `json:"overall"`
	// OverallFormula stamps which aggregation rule produced Overall, so cards computed under different
	// denominators are never compared as if they were the same number (the 2026-07 baseline said 3.077
	// over 5 dims while the 07-25 card said 4.14 over 4 — a +0.4 artifact of the shrinking denominator).
	OverallFormula string `json:"overall_formula,omitempty"`
}

// AbstentionFloor is the rubric floor a dimension contributes to Overall when the run produced ZERO
// scoreable samples for it. Abstention is failure, not absence: the pre-v2 mean-over-present-dims let a
// run that proposed nothing DROP falsifiable_prediction (baseline mean 1.483) from the denominator and
// RISE ~+0.4 overall for producing nothing. The per-session N/A rule (TG-61) still applies — only the
// corpus-level zero-coverage case is floored, because producing zero predictions across a whole corpus is
// a system failure, not an N/A.
const AbstentionFloor = 1.0

// OverallFormulaV2 marks a scorecard whose Overall uses the fixed denominator: every canonical dimension
// always enters the mean, floored at AbstentionFloor when unmeasured.
const OverallFormulaV2 = "fixed-denominator-v2"

// LoadCorpus reads the incident corpus and validates its expected labels against the CLOSED enum. A typo'd
// label ("Propose", "propose ") would otherwise silently drop out of ExpectedProposeN and disarm the recall
// floor with no red anywhere — the label plane must fail closed like every other gate input.
func LoadCorpus(path string) ([]Incident, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c []Incident
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("corpus %s: %w", path, err)
	}
	for _, inc := range c {
		switch inc.Expected {
		case "", "propose", "stand-down", "escalate":
		default:
			return nil, fmt.Errorf("corpus %s: %s carries unknown expected label %q (closed set: propose|stand-down|escalate|absent)",
				path, inc.ExternalRef, inc.Expected)
		}
		if inc.ExpectedOpClass != "" && inc.Expected != "propose" {
			return nil, fmt.Errorf("corpus %s: %s declares expected_op_class %q without expected=propose",
				path, inc.ExternalRef, inc.ExpectedOpClass)
		}
		// Fixture-arm validation (B4a) — every failure mode here is a SILENT one without it: fixtures on a
		// non-propose incident would quietly convert a live-groundedness measurement into a replay; a typo'd
		// tool name or an un-normalized host key would never match at Invoke time and the arm would degrade
		// to all-miss shapes with no red anywhere. Fail closed, like every other gate input.
		if inc.FixtureArmed() && inc.Expected != "propose" {
			return nil, fmt.Errorf("corpus %s: %s carries tool fixtures with expected=%q — only expected-propose incidents may be fixture-armed (stand-down/escalate correctness IS live-groundedness)",
				path, inc.ExternalRef, inc.Expected)
		}
		for key, f := range inc.ToolFixtures {
			tool, host, ok := strings.Cut(key, "|")
			if !ok || strings.TrimSpace(tool) == "" || strings.TrimSpace(host) == "" {
				return nil, fmt.Errorf("corpus %s: %s fixture key %q is not \"tool|host\"", path, inc.ExternalRef, key)
			}
			if !fixtureServable(tool) {
				return nil, fmt.Errorf("corpus %s: %s fixture names unknown tool %q (served set: %s) — a typo'd name would silently never match",
					path, inc.ExternalRef, tool, strings.Join(fixtureServedTools, ", "))
			}
			if key != FixtureKey(tool, host) {
				return nil, fmt.Errorf("corpus %s: %s fixture key %q is not normalized (want %q)",
					path, inc.ExternalRef, key, FixtureKey(tool, host))
			}
			if strings.TrimSpace(f.Output) == "" {
				return nil, fmt.Errorf("corpus %s: %s fixture %q has an empty output — an empty observation is a capture error, not evidence", path, inc.ExternalRef, key)
			}
		}
	}
	return c, nil
}

// judgePrompt builds the strict-JSON judge instruction for one session — the shared core/judge prompt
// over this session's facts (the durable judge cron builds the SAME prompt from the compact record).
func judgePrompt(s Session) string {
	return judge.Prompt(judge.Session{
		Ref:        s.Ref,
		AlertRule:  s.AlertRule,
		Host:       s.Host,
		Severity:   s.Severity,
		Band:       s.Band,
		Proposed:   s.Proposed,
		ActionID:   s.ActionID,
		Prediction: s.Prediction,
		Predicted:  s.Predicted,
		Evidence:   s.Evidence,
		Conclusion: s.Conclusion,
		Decisions:  s.Decisions,
		Outcome:    s.Outcome,
		Mutated:    s.Mutated,
	})
}

// ParseScore extracts the judge's JSON verdict defensively (the model may wrap it in prose / fences).
// It delegates to the shared core/judge parser — ONE parser for the eval harness and the judge cron.
func ParseScore(ref, raw string) (Score, error) {
	js, err := judge.ParseScore(ref, raw)
	return Score{Ref: js.Ref, Scores: js.Scores, Comment: js.Comment}, err
}

// sortedStringSet returns a set's keys in sorted order — a stable, diffable op-class list for the A5 breadth
// signal (distinct from the test-only sortedKeys, which takes a map[string]struct{}).
func sortedStringSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// seedResolutionRecall caches TG-491's leave-one-out resolution-recall over the SHIPPED seed corpus. The
// seed (deploy/knowledge/corpus.seed.json) is a compile-time-shipped constant and ResolutionRecall is pure +
// deterministic, so the O(n²) leave-one-out is computed at most once per process, race-safe via sync.Once.
var (
	seedResolutionRecallOnce   sync.Once
	seedResolutionRecallResult retrievaleval.ResolutionRecallResult
)

// resolutionRecallOverSeed returns TG-491's leave-one-out retrieval-recall measured over the shipped seed
// corpus — the same deploy/knowledge/corpus.seed.json the worker serves and resolution_recall_test.go
// measures, loaded the same way (os.ReadFile + json.Unmarshal into []knowledge.Incident). It is BEST-EFFORT
// on purpose: TG-507 surfaces this as a REPORTED-only scorecard axis, never a gate input, so if the shipped
// seed cannot be located from the caller's CWD (e.g. a non-eval-package binary such as tools/rejudge) the
// result stays zero — a reported metric degrades to "not reported", never to a failure. The relative path is
// resolved from the eval package's directory (CWD when the eval-package tests and the on-box harness run),
// so ../deploy/... reaches the repo root; this is resolution_recall_test.go:17's ../../ hop minus one level,
// because eval/ is one directory shallower than eval/retrieval/.
func resolutionRecallOverSeed() retrievaleval.ResolutionRecallResult {
	seedResolutionRecallOnce.Do(func() {
		raw, err := os.ReadFile(filepath.Join("..", "deploy", "knowledge", "corpus.seed.json"))
		if err != nil {
			return
		}
		var corpus []knowledge.Incident
		if err := json.Unmarshal(raw, &corpus); err != nil {
			return
		}
		// Production retrieves top-k=3; the resolution-match cutoff is 0.5 shared tokens over the shorter fix
		// — identical parameters to resolution_recall_test.go, so the reported number tracks the CI ratchet.
		seedResolutionRecallResult = retrievaleval.ResolutionRecall(corpus, 3, 0.5)
	})
	return seedResolutionRecallResult
}

// Aggregate builds the scorecard from the sessions + their scores. An optional estate snapshot enables the
// second deterministic axis (estate_grounded, TG-314); passing none leaves that axis honestly N/A, exactly as
// before — every existing caller compiles unchanged.
func Aggregate(sessions []Session, scores []Score, estateGraph ...*estate.Graph) Scorecard {
	sc := Scorecard{N: len(sessions), Judged: len(scores), Bands: map[string]int{}, DimMeans: map[string]float64{}, RubricVersion: judge.RubricVersion()}
	proposed, predicted, stepSum := 0, 0, 0
	classes := map[string]bool{} // distinct proposed op-classes (A5 fault-class breadth)
	for _, s := range sessions {
		band := s.Band
		if band == "" {
			band = "none"
		}
		sc.Bands[band]++
		if s.Proposed {
			proposed++
		}
		if s.Predicted {
			predicted++
		}
		if s.OpClass != "" {
			classes[s.OpClass] = true
		}
		stepSum += s.StepCount
		if s.FixtureArmed {
			sc.FixtureArmed++ // deterministic-arm disclosure: how much of the propose supply was fixture-served
		}
		if s.Mutated {
			sc.MutationCount++
		}
		if s.Err != "" {
			sc.Errors++ // a triage workflow that errored (e.g. a 429-contended arm) — the gate must not silently pool it
			if sc.FirstErr == "" {
				sc.FirstErr = s.Err // TG-493: keep the first concrete cause so the integrity abort can name it
			}
		}
	}
	sc.FaultClasses = sortedStringSet(classes)
	sc.FaultClassBreadth = len(sc.FaultClasses)
	if sc.N > 0 {
		sc.ProposalRate = float64(proposed) / float64(sc.N)
		sc.PredictionRate = float64(predicted) / float64(sc.N)
		// A4 autonomy rate: the auto-actuating bands (resolve without a human) over the whole corpus.
		sc.AutonomyRate = float64(sc.Bands["AUTO"]+sc.Bands["AUTO_NOTICE"]) / float64(sc.N)
		// A6a decision steps: mean investigation cycles per session (fewer ⇒ cheaper, NOT necessarily faster —
		// the wall-clock half is A6b, measured live off session_triage.decision_ms).
		sc.MeanDecisionSteps = float64(stepSum) / float64(sc.N)
	}
	// Label-aware rates: only labeled sessions enter either metric, so an unlabeled corpus degrades to
	// published-zero denominators (the gate floors are keyed on the denominators, never on bare zeros).
	proposedOnExpected, stooddownLabeled, stooddownCorrect := 0, 0, 0
	for _, s := range sessions {
		if s.StaleVsLive && s.Expected != "" {
			sc.StaleExcluded++ // counted, never silently dropped — the exclusion is part of the record
			continue
		}
		switch s.Expected {
		case "propose":
			sc.ExpectedProposeN++
			if s.Proposed {
				proposedOnExpected++
			}
		}
		if s.Expected != "" && !s.Proposed {
			stooddownLabeled++
			if s.Expected == "stand-down" || s.Expected == "escalate" {
				stooddownCorrect++
			}
		}
	}
	sc.LabeledStanddownN = stooddownLabeled
	if sc.ExpectedProposeN > 0 {
		sc.ProposalRecall = round2(float64(proposedOnExpected) / float64(sc.ExpectedProposeN))
	}
	if stooddownLabeled > 0 {
		sc.StanddownPrecision = round2(float64(stooddownCorrect) / float64(stooddownLabeled))
	}
	// falsifiable_prediction is N/A for a grounded stand-down (no action ⇒ no prediction to falsify) — a
	// category error to score, and its floor otherwise drags the dimension mean down. Exclude non-applicable
	// sessions from THAT dimension only, so it measures real proposer prediction quality (TG-61 seq C). One
	// rule, judge.PredictionApplicable, shared with the durable judge cron + flywheel DimensionMeans.
	applicable := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		applicable[s.Ref] = judge.PredictionApplicable(judge.Session{Proposed: s.Proposed, Predicted: s.Predicted})
	}
	sums := map[string]int{}
	counts := map[string]int{}
	for _, s := range scores {
		for d, v := range s.Scores {
			if d == judge.DimFalsifiablePrediction && !applicable[s.Ref] {
				continue // N/A for a stand-down — omitted, not floored
			}
			sums[d] += v
			counts[d]++
		}
	}
	// Fixed denominator (v2): every canonical dimension ALWAYS enters the mean. A dimension with zero
	// scored samples across the whole corpus contributes AbstentionFloor — abstention scores as failure,
	// never as absence. Overall stays 0 on a fully-degraded run (nothing judged): that zero is the
	// integrity signal the gate refuses to pool, not a floored score.
	sc.DimSamples = map[string]int{}
	var overallSum float64
	for _, d := range Dimensions {
		m := AbstentionFloor
		if counts[d] > 0 {
			m = float64(sums[d]) / float64(counts[d])
		}
		sc.DimMeans[d] = round2(m)
		sc.DimSamples[d] = counts[d]
		overallSum += m
	}
	if sc.Judged > 0 {
		sc.Overall = round2(overallSum / float64(len(Dimensions)))
		sc.OverallFormula = OverallFormulaV2
	}
	// THE DETERMINISTIC AXIS (TG-201): diagnosis_grounded is computed from the SESSIONS, not from the judge
	// model's verdicts — every input is a fact the orchestrator bound, so there is nothing for a model to
	// author. It rides the scorecard beside the five so the offline plane measures the typed claim the live
	// judge cron already scores; one claim, one measurement, in both planes.
	//
	// It stays OUT of Overall and out of gate.Dimensions on purpose. Overall is a FIXED-denominator mean
	// (OverallFormulaV2) and every committed card — the baseline included — was computed over five axes:
	// widening the denominator now would move every historical Overall by ~0.6 for a reason unrelated to
	// agent quality, which is precisely the artifact OverallFormula exists to prevent (the 3.077-over-5 vs
	// 4.14-over-4 incident). N/A sessions are omitted, never floored (judge.DiagnosisApplicable).
	var diagSum, diagN int
	for _, s := range sessions {
		if v, _, ok := judge.ScoreDiagnosis(judge.Session{
			Proposed: s.Proposed, Diagnosis: s.Diagnosis, DiagnosisRecorded: s.DiagnosisRecorded,
		}); ok {
			diagSum += v
			diagN++
		}
	}
	if diagN > 0 {
		sc.DimMeans[judge.DimDiagnosisGrounded] = round2(float64(diagSum) / float64(diagN))
		sc.DimSamples[judge.DimDiagnosisGrounded] = diagN
	}
	// THE SECOND DETERMINISTIC AXIS (TG-314): estate_grounded, measured in the OFFLINE plane too — so the
	// flywheel's pre-filter and the committed baseline see the same axis the live judge cron already scores,
	// "one claim, one measurement, in both planes" (TG-201/TG-202). Unlike diagnosis_grounded — a property of
	// the session alone — this one needs the causal GRAPH, so it is computed only when the harness passes a
	// snapshot; without one the axis stays honestly N/A (unchanged behaviour). The estate is held CONSTANT
	// across a scorecard (one fixture), which is exactly what isolates a skill's effect from estate drift when
	// comparing versions. Like diagnosis_grounded it stays OUT of Overall's fixed denominator and out of
	// gate.Dimensions — widening the denominator would move every historical Overall for a reason unrelated to
	// agent quality (the artifact OverallFormula exists to prevent). N/A sessions are omitted, never floored.
	if len(estateGraph) > 0 && estateGraph[0] != nil {
		g := estateGraph[0]
		var estSum, estN int
		for _, s := range sessions {
			js := judge.Session{Host: s.Host, Proposed: s.Proposed, Diagnosis: s.Diagnosis, DiagnosisRecorded: s.DiagnosisRecorded}
			js.Estate = judge.GroundInEstate(g, js)
			if v, _, ok := judge.ScoreEstateGrounded(js); ok {
				estSum += v
				estN++
			}
		}
		if estN > 0 {
			sc.DimMeans[judge.DimEstateGrounded] = round2(float64(estSum) / float64(estN))
			sc.DimSamples[judge.DimEstateGrounded] = estN
		}
	}
	// THE THIRD DETERMINISTIC AXIS (TG-525): trajectory_grounded — the ordered tool path graded by the SAME
	// deterministic veto that halts a stuck agent at runtime (agent.TrajectoryVeto), so an inefficient/looping
	// trajectory is VISIBLE on the scorecard beside the judged dims. Report-only: like diagnosis_grounded /
	// estate_grounded it stays OUT of Overall's fixed denominator AND out of gate.Dimensions (an axis outside
	// gate.Compare's set cannot bar a merge). N/A sessions (no recorded trajectory) are omitted, never floored.
	var trajSum, trajN int
	for _, s := range sessions {
		if v, ok := trajectoryScore(s); ok {
			trajSum += v
			trajN++
		}
	}
	if trajN > 0 {
		sc.DimMeans[dimTrajectoryGrounded] = round2(float64(trajSum) / float64(trajN))
		sc.DimSamples[dimTrajectoryGrounded] = trajN
	}
	// TG-507: surface TG-491's leave-one-out RESOLUTION-RECALL over the shipped seed corpus as a REPORTED
	// (never-gated) retrieval-quality axis, so a retriever regression is VISIBLE in the deploy-gating
	// change-gate diff beside A4/A5/A6a. It is measured over the shipped corpus (NOT these sessions) using each
	// incident's own recorded Resolution as un-invented, non-circular ground truth — no gateway, no judge, no
	// operator labels. The eval gate never bars on it (proven in eval/gate); the retriever's own CI ratchet
	// (eval/retrieval/resolution_recall_test.go, floor 0.90) owns that red.
	rr := resolutionRecallOverSeed()
	sc.ResolutionRecall = rr.Recall
	sc.ResolutionRecallCeiling = rr.Ceiling
	sc.ResolutionRecallOfFindable = rr.OfFindable
	return sc
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// ScorecardJSON serializes the scorecard deterministically.
func ScorecardJSON(sc Scorecard) []byte {
	b, _ := json.MarshalIndent(sc, "", "  ")
	return b
}

// SortSessions orders sessions by ref for deterministic reporting.
func SortSessions(ss []Session) { sort.Slice(ss, func(i, j int) bool { return ss[i].Ref < ss[j].Ref }) }
