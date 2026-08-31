// Package judge is the shared LLM-judge semantics for triage-session quality (task #26 / TG-37): the
// five scoring dimensions, the strict-JSON judge prompt, and the defensive verdict parser. It is the
// SINGLE source of these semantics — the eval harness (eval/) and the durable judge cron
// (temporal/skilljudge) both consume this package, so the offline scorecard and the production
// session_judgment rows are scored by the same judge, never two drifting copies.
//
// The judge ADJUDICATES the session record; it never re-runs the triage and it is read-only over the
// record. A grounded decision NOT to act (a stop whose conclusion cites observed evidence) is CORRECT
// triage and scores well — the guidance below carries that verbatim from the eval harness.
package judge

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/proposal"
)

// Dimensions are the five quality axes the judge scores 1..5. correct_diagnosis, evidence_grounded,
// sensible_proposal and appropriate_band mirror the predecessor's LLM-judge dims; falsifiable_prediction
// is TG's own grounding differentiator (was a well-formed, verifiable prediction committed?). Sourced from
// the embedded rubric.json (the ONE rubric source) so eval/, eval/gate and the Python shadowbench judge
// all score the same axes — never a re-declared copy that drifts.
var Dimensions = rubric.Dimensions

// DimFalsifiablePrediction is the grounding-differentiator dimension whose applicability is
// disposition-dependent (see PredictionApplicable) — named so every mean-computing consumer excludes it
// consistently for stand-downs (TG-61 seq C).
const DimFalsifiablePrediction = "falsifiable_prediction"

// DimAppropriateBand is the safety-analog dimension: autonomy-band appropriateness. Named so the eval gate
// (eval/gate.SafetyDim) references the one source rather than re-declaring the literal.
const DimAppropriateBand = "appropriate_band"

// PredictionApplicable reports whether falsifiable_prediction is a meaningful axis for this session.
// Only a session that PROPOSED an action (and so committed a machine prediction at the gate) has an
// action-consequence to predict; a grounded stand-down makes no action, so there is nothing to falsify —
// the dimension is genuinely N/A, not "poor". Scoring a stand-down 1/5 both mis-measures grounding AND
// (because the dimension then floors globally across every skill's mostly-stand-down sessions) is the
// root cause the flywheel's Regressed trigger fired for every skill at once. Every consumer that MEANS
// falsifiable_prediction — the eval scorecard, the durable session_judgment write, and the flywheel's
// DimensionMeans — excludes non-applicable sessions from that dimension only (TG-61 seq C). Proposed and
// Predicted are equal for a terminal record (a gated proposal always commits a prediction); the durable
// session_triage record persists Proposed, so either being set marks the session applicable.
func PredictionApplicable(s Session) bool { return s.Proposed || s.Predicted }

// Session is the judged record's facts — what the judge sees. The eval harness fills every field from
// its captured run; the judge cron fills what the compact TriageRow carries (absent facts stay zero,
// honestly presented as such rather than fabricated).
type Session struct {
	Ref        string
	AlertRule  string
	Host       string
	Severity   string
	Band       string   // AUTO | AUTO_NOTICE | POLL_PAUSE | ""
	Proposed   bool     // did the agent propose an action?
	Op         string   // the proposed op (when known — the compact record carries it)
	ActionID   string   // the sealed action id (if proposed+gated)
	Prediction string   // the committed consequence prediction (grounding signal)
	Predicted  bool     // was a machine prediction committed (falsifiable)?
	Evidence   []string // cited evidence ids (INV-11 silent-cognition guard)
	Conclusion string   // the agent's grounded no-action rationale on a stop (REQ-1008)
	Decisions  []string // governance-ledger decision labels for this session
	Outcome    string   // the RunnerResult outcome string
	Mutated    bool     // MUST be false (mutation OFF)
	// ActorAttribution is the deterministic attribution taxonomy (controlled vocabulary, spec/023) and
	// ActorEvidenceCount how many reader-captured records backed it. Both were invisible to the judge, which
	// therefore scored correct_diagnosis and evidence_grounded without knowing whether an actor was even
	// known for the incident.
	ActorAttribution   string
	ActorEvidenceCount int
	// Diagnosis is the session's typed, source-bound CLAIM (core/proposal, TG-201) as the ORCHESTRATOR bound
	// it — `Cited` here means "this id matched a ToolResult the orchestrator actually captured", never "the
	// model said so". It is scored by ScoreDiagnosis (deterministic, core/judge/diagnosis.go) and is
	// deliberately NOT rendered into Prompt(): the judge model does not grade this axis, and the prompt is
	// byte-pinned by TestPromptMatchesGolden.
	//
	// DiagnosisRecorded says the RECORD carries the field at all — false for every session that ran before
	// migration 0056, where an empty claim means "the column did not exist", not "the agent withheld one".
	// Without that bit the dimension would retroactively grade thousands of historical sessions against a
	// rule they were never offered (the TG-61 global-floor failure, in the other direction).
	Diagnosis         proposal.Diagnosis
	DiagnosisRecorded bool
	// Estate is the MACHINE-COMPUTED topology block (TG-202): what the causal estate graph says about the
	// relationship between the diagnosis's named cause and the alerting host. Filled by GroundInEstate at the
	// judging boundary — it is not carried on the durable record, because a judgement about topology must be
	// read from the estate as it is KNOWN, by traversal, and never from a model's prose about it.
	//
	// Like Diagnosis it is scored deterministically (ScoreEstateGrounded) and is NOT rendered into Prompt():
	// showing a model the graph and asking it to read the edges is the weaker form this dimension exists to
	// replace. Its zero value means "the graph was never consulted" ⇒ the axis is N/A, so a caller that does
	// not wire an estate scores exactly as it did before this shipped.
	Estate EstateFacts
}

// Prompt builds the strict-JSON judge instruction for one session. The judge sees the incident + what
// the Runner did and rates each dimension 1..5. It never re-runs the triage — it adjudicates the record.
func Prompt(s Session) string {
	var b strings.Builder
	// Fixed rubric text comes from the embedded rubric.json (the ONE source, shared with shadowbench); only
	// the session-fact interpolation lives in code. This reproduces the historical prompt byte-for-byte —
	// TestPromptMatchesGolden pins that, so relocating the text into the shared file changed no scoring.
	b.WriteString(rubric.Intro)
	b.WriteString(rubric.ReplyInstruction)
	fmt.Fprintf(&b, "INCIDENT: rule=%q host=%q severity=%q\n", s.AlertRule, s.Host, s.Severity)
	fmt.Fprintf(&b, "TRIAGE RESULT: band=%q proposed=%v op=%q action_id=%q predicted=%v mutated=%v outcome=%q\n", s.Band, s.Proposed, s.Op, s.ActionID, s.Predicted, s.Mutated, s.Outcome)
	fmt.Fprintf(&b, "COMMITTED PREDICTION: %q\n", s.Prediction)
	fmt.Fprintf(&b, "CITED EVIDENCE IDS: %q\n", s.Evidence)
	fmt.Fprintf(&b, "ACTOR ATTRIBUTION: taxonomy=%q evidence_records=%d\n", s.ActorAttribution, s.ActorEvidenceCount)
	fmt.Fprintf(&b, "AGENT CONCLUSION (present when it stopped without proposing): %q\n", s.Conclusion)
	fmt.Fprintf(&b, "LEDGER DECISIONS: %q\n\n", s.Decisions)
	b.WriteString(rubric.Guidance)
	b.WriteString(rubric.HollowProposalRule)
	return b.String()
}

// Score is one judged session: a 1..5 per dimension + a one-line rationale.
type Score struct {
	Ref     string         `json:"ref"`
	Scores  map[string]int `json:"scores"`
	Comment string         `json:"comment"`
}

// ParseScore extracts the judge's JSON verdict defensively (the model may wrap it in prose / fences).
func ParseScore(ref, raw string) (Score, error) {
	sc := Score{Ref: ref, Scores: map[string]int{}}
	i, j := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if i < 0 || j <= i {
		return sc, fmt.Errorf("no json object in judge reply")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw[i:j+1]), &m); err != nil {
		return sc, fmt.Errorf("judge json: %w", err)
	}
	for _, d := range Dimensions {
		if v, ok := m[d]; ok {
			sc.Scores[d] = clampScore(v)
		}
	}
	if c, ok := m["comment"].(string); ok {
		sc.Comment = c
	}
	if len(sc.Scores) == 0 {
		return sc, fmt.Errorf("judge reply had no dimension scores")
	}
	return sc, nil
}

// clampScore coerces a JSON number/string to an int in [1,5].
func clampScore(v any) int {
	var n int
	switch t := v.(type) {
	case float64:
		n = int(t)
	case int:
		n = t
	case string:
		fmt.Sscanf(t, "%d", &n)
	}
	if n < 1 {
		n = 1
	}
	if n > 5 {
		n = 5
	}
	return n
}

// TrajectoryStep is one digested tool invocation of a session's ordered path — the persisted twin of
// agent.TrajectoryStep (tool name + the order-independent ArgsKey digest; never raw arguments). Declared
// here rather than imported so core/judge keeps no dependency edge on the agent loop (TG-527).
type TrajectoryStep struct {
	Tool    string `json:"tool"`
	ArgsKey string `json:"args_key"`
}

// TriageRow is the Runner's compact terminal record (spec/012 REQ-1106) — one session_triage row: the
// facts the asynchronous judge adjudicates. It is written best-effort at the workflow's terminal
// outcome, idempotent on ExternalRef, and read-only thereafter (Judged is the only mutable bit).
type TriageRow struct {
	ExternalRef string
	Host        string
	AlertRule   string
	Band        string
	Outcome     string
	Proposed    bool
	Op          string
	// OpClass is the canonical op-class of the proposed action (Action.OpClass, e.g. "restart-service",
	// "start-guest", "disk-grow") — the correct unit for axis A5 (fault-class breadth). Op alone is the raw
	// verb and is ambiguous ("restart" ⇒ restart-service OR reboot); op_class disambiguates it. Persisted for
	// the live-axis scorer (migration 0036); "" for a no-proposal stop or a pre-migration record. OBSERVABILITY
	// ONLY — a decision record, never a gate input.
	OpClass string
	// ActorAttribution is the deterministic attribution taxonomy for the session (spec/023) — a CONTROLLED
	// vocabulary value such as "authorized-test" / "attributed-authorized" / "unattributable", never free
	// text. ActorEvidenceCount is how many reader-captured actor-evidence records backed it.
	//
	// The judge could not see either. It was shown "CITED EVIDENCE IDS" as OPAQUE IDS, so it could tell that
	// SOME evidence was cited and never whether the conclusion accounted for a KNOWN actor — which is most of
	// what `correct_diagnosis` and `evidence_grounded` are asking. Measured live: 508 of 1228 sessions carry a
	// resolved taxonomy and 465 carry evidence, and none of it reached the scorer.
	//
	// Only the taxonomy and the COUNT cross this boundary — deliberately. The evidence records themselves are
	// external text (actor names, verbs, refs out of other systems' logs), and rendering them into a judge
	// prompt would carry an untrusted payload across an export boundary for no scoring benefit (REQ-2313).
	ActorAttribution   string
	ActorEvidenceCount int
	EvidenceIDs        []string
	Conclusion         string
	// StopReason: WHY a no-proposal session halted, ORCHESTRATOR-COMPUTED (migration 0044). Deliberately
	// separate from Conclusion, which is untrusted agent free-text (INV-08), and deliberately NOT rendered
	// into the judge's fact surface — that prompt is byte-pinned by a golden test and must not move
	// mid-accrual.
	StopReason string
	// Prediction is the committed machine prediction rendered judge-readable, and Predicted whether one was
	// committed (TG-61). Without these the judge cron scored falsifiable_prediction blind — the durable
	// session_judgment rows floored the dimension for want of the prediction the gate actually committed. The
	// eval harness already passes the same rendered line, so carrying it here aligns live scoring with eval.
	Prediction string
	Predicted  bool
	// Confidence is the agent's emitted 0..1 proposal CONFIDENCE scalar (core/proposal), persisted for the
	// decision tracer + calibration measurement (spec/020 REQ-2003, the observability half); 0 for a
	// no-proposal stop. OBSERVABILITY ONLY — this is NOT the actuation-path policy min_confidence clamp input
	// (that clamp reads r.Confidence at the interceptor and is a separate reviewed change).
	Confidence float64
	// Attribution is the actor-attribution taxonomy the attribute step resolved (spec/023 REQ-2311) — the
	// WHO-CAUSED-THIS answer; "" for a pre-feature/pre-deploy record. ActorEvidence is the minimized,
	// redacted reader-captured evidence (actor, verb, timestamp, ref — never raw log lines, REQ-2313).
	// OBSERVABILITY ONLY — the taxonomy was already decided deterministically upstream; neither re-enters it.
	Attribution   string
	ActorEvidence []byte // jsonb blob of []attribution.Evidence, marshaled at the activity boundary
	// Trajectory is the session's ordered, digested tool path (agent.TrajectoryStep — tool + ArgsKey, no
	// raw arguments; TG-525) persisted for the trajectory_grounded axis over HISTORICAL sessions (TG-527,
	// migration 0104). Before this the trajectory existed only inside the eval harness's process, so any
	// DB-replayed re-judge read the axis N/A forever. Typed here (Temporal serializes it); the store
	// marshals to jsonb with the Diagnosis NULL-vs-empty discipline. OBSERVABILITY ONLY — the deterministic
	// TrajectoryVeto already acted at runtime; nothing re-enters the decision path. Mirrors
	// agent.TrajectoryStep field-for-field without importing agent (core/ keeps no edge to the loop).
	Trajectory []TrajectoryStep
	// SkillLoads is the composed-seed provenance verbatim (name@version#id:origin[:arm], spec/014
	// REQ-1303) — the judge cron extracts the store version ids from it for the regression-watch feed.
	SkillLoads []string
	// PromptVersion, SeedHash, ModelTier are the session's prompt/seed/model provenance for the decision
	// tracer (spec/020 REQ-2009): the trusted-preamble template version, the SHA-256 fingerprint of the
	// composed agent seed (the HASH only — never the seed text, which embeds untrusted incident data; INV-13),
	// and the LLM tier the investigation ran on. OBSERVABILITY ONLY — none re-enters the decision path. Empty
	// for a session that composed no seed (a suppressed/early stop).
	PromptVersion string
	SeedHash      string
	ModelTier     string
	// DecisionTier is the tier that produced the TERMINAL proposal or grounded stop — WHICH MODEL DECIDED
	// (TG-198, migration 0057). ModelTier above answers only which model did the READING: the TG-60
	// decide-nudge routinely hands the final cycle from "fast" to "primary", and until this field existed
	// that switch was recorded nowhere, so every one of TG's 537 recorded incidents attributed its decision
	// to the cheap tier. Empty for a session recorded before the column existed — deliberately distinct from
	// a real tier name, so an analysis over the corpus can exclude unattributable rows instead of counting
	// them as "fast". OBSERVABILITY ONLY — it re-enters no gate.
	DecisionTier string
	// StepCount is the agent loop's read-only investigation cycle count (len of the ReAct transcript) — benchmark
	// axis A6a (decision efficiency, the steps half). Persisted (migration 0037) so the live-axis scorer measures
	// A6a off real triages; 0 for a pre-migration record or a session that ran no loop. OBSERVABILITY ONLY.
	StepCount int
	// DecisionMillis is the agent loop's WALL-CLOCK time to the terminal decision — benchmark axis A6b, the
	// time half TG-205 split out of A6. StepCount above answers only how many cycles were spent; A6 is
	// DEFINED as MTTR and no axis surface measured time (the sole clock was tg_agent_run_seconds_total, a
	// cumulative sum over all loops), so a run that is few-stepped and slow was indistinguishable from one
	// that is few-stepped and fast. Persisted (migration 0058); 0 means UNMEASURED
	// — a pre-migration record, or a session that never entered the loop — and the A6b percentiles EXCLUDE it
	// rather than counting an instant decision. OBSERVABILITY ONLY — it re-enters no gate (INV-08).
	DecisionMillis int64
	// Mutated is whether TG actually ACTUATED an estate mutation for this incident (res.Mutated = exec.Executed)
	// — benchmark axis A3 (heal success) DENOMINATOR. False under mutation OFF (the interceptor refuses at
	// GuardMutation) and for a no-proposal / stood-down terminus. ConfirmedClear is the orchestrator-captured
	// post-condition confirmation that the incident's ORIGINAL (host, alert_rule) went quiet after the mutation
	// (TG-124) — the A3 NUMERATOR, and the FAITHFUL heal signal for the live native-ssh path (a match verdict
	// excludes the target's own alert, so action_verdict can never mean the original condition cleared). Both
	// persisted from migration 0039; OBSERVABILITY ONLY, neither re-enters a gate. mutated is set at first
	// insert (known at record time); confirmed_clear is written by a follow-up update once the bounded
	// clear-observe loop resolves, so a mutated row whose clear is not yet (or never) observed reads false.
	Mutated        bool
	ConfirmedClear bool
	Judged         bool
	// UndoSketch is the model's free-text reversal sketch for the proposed action (spec/026 REQ-2604,
	// migration 0046) — the grammar's ONE additive field, screen.Scrub'd at the shadow-record activity
	// before it reaches this row. Untrusted DATA (INV-08). Deliberately NOT rendered by Facts() into the
	// byte-pinned judge prompt in v1 (ADR-0016 OQ-7): the golden test pins that surface, and this field
	// must not move it. Empty for a proposal without a sketch and for every pre-migration record.
	UndoSketch string
	// Diagnosis is the typed, source-bound CLAIM the session's proposal rested on (TG-201, migration 0056:
	// session_triage.diagnosis jsonb) — the evidence FOR it, the evidence AGAINST it, and the alternatives
	// ruled out, each already bound by the orchestrator against its own captured ToolResult ids. It is
	// persisted rather than re-derived because the judge runs HOURS later on the record: the transcript is
	// gone by then, and re-deriving `cited` from the model's own citation is precisely the thing INV-11
	// refuses to do.
	//
	// DiagnosisRecorded is whether the column was non-NULL — the honest difference between "the agent bound
	// no claim" and "this session predates the column". Scored (deterministically) by ScoreDiagnosis; it
	// re-enters no gate and vetoes nothing (INV-08).
	Diagnosis         proposal.Diagnosis
	DiagnosisRecorded bool
	// DegradedCapabilities is the set of TG's OWN dependency capabilities that were degraded when this session
	// ran — a capability (embed / journal-evidence / secrets / tracker / notify) with a backing host that had
	// no fresh edge in the estate graph (TG-394 slice 3, migration 0082). It is stamped so a lexical-only
	// investigation is LEGIBLE AFTERWARDS: when the embed backend is unreachable, retrieval silently degrades
	// to lexical, and the record must carry the reason instead of reading like an ordinary session hours later
	// when the graph has recovered. A CONTROLLED, non-secret vocabulary — capability names, never a host, argv,
	// or credential. NULL column = the session predates the field; an empty (non-nil) set = "checked, nothing
	// degraded" — this build always writes the set explicitly. OBSERVABILITY ONLY — it re-enters no gate (INV-08).
	DegradedCapabilities []string
	CreatedAt            time.Time
}

// Facts renders the compact record as the judge's Session. Fields the compact record does not carry
// (severity, action id, ledger decisions) stay zero — honestly absent, never invented. The committed
// prediction IS carried now (TG-61), so the judge scores falsifiable_prediction over real data.
func (r TriageRow) Facts() Session {
	return Session{
		Ref:        r.ExternalRef,
		AlertRule:  r.AlertRule,
		Host:       r.Host,
		Band:       r.Band,
		Proposed:   r.Proposed,
		Op:         r.Op,
		Prediction: r.Prediction,
		Predicted:  r.Predicted,
		Evidence:   r.EvidenceIDs,
		Conclusion: r.Conclusion,
		Outcome:    r.Outcome,
		// Carried now (P5): without these the judge scored correct_diagnosis and evidence_grounded blind to
		// whether an actor was known for the incident at all.
		ActorAttribution:   r.ActorAttribution,
		ActorEvidenceCount: r.ActorEvidenceCount,
		// TG-201: the typed claim and whether the record carries it. A projection that dropped either would
		// make the diagnosis dimension silently N/A for every session — the way the committed prediction used
		// to arrive blank at the judge.
		Diagnosis:         r.Diagnosis,
		DiagnosisRecorded: r.DiagnosisRecorded,
	}
}

// StoreVersionIDs parses the store version ids out of skill_load provenance strings. A store-origin
// entry is `name@version#<id>:store` optionally suffixed with a trial-arm note (`:trial9/arm0`); every
// other shape (compiled, pinned, fallback markers, id-less legacy store entries) is skipped. The ids
// feed skillstore.ObserveJudgedSession — the regression watch's composed-version match (REQ-1310).
func StoreVersionIDs(loads []string) []int64 {
	var out []int64
	for _, l := range loads {
		h := strings.Index(l, "#")
		if h < 0 {
			continue
		}
		rest := l[h+1:]
		c := strings.Index(rest, ":")
		if c <= 0 {
			continue
		}
		tail := rest[c+1:]
		if tail != "store" && !strings.HasPrefix(tail, "store:") {
			continue
		}
		id, err := strconv.ParseInt(rest[:c], 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		out = append(out, id)
	}
	return out
}
