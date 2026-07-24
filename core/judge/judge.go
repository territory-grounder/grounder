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
	EvidenceIDs []string
	Conclusion  string
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
	Judged        bool
	CreatedAt     time.Time
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
