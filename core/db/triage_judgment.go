package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/trace"
)

// TriageStore is the pgx judge spine over session_triage / session_judgment (migration 0010, spec/012
// REQ-1106): the Runner's compact terminal-record writer plus the judge cron's read/mark/write
// surface. Parameterized SQL only; idempotency is STRUCTURAL (the external_ref PK and the
// (external_ref, dimension) UNIQUE with ON CONFLICT), so an activity retry can never duplicate a
// session record or a judgment row. session_judgment's shape satisfies the queries
// skillstore_trial.go already runs against it (armScoresForDim, JudgedSessionRate) — landing this
// table is what turns those from honest empties into real trial data.
type TriageStore struct{ p *Pool }

// NewTriageStore returns the Postgres-backed judge-spine store.
func NewTriageStore(p *Pool) *TriageStore { return &TriageStore{p: p} }

// RecordTriage persists the compact terminal triage record — idempotent on external_ref (the FIRST
// terminal record for a session wins; a workflow-level retry or duplicate delivery is a no-op).
func (s *TriageStore) RecordTriage(ctx context.Context, row judge.TriageRow) error {
	sv, err := schema.Stamp(schema.TableSessionTriage)
	if err != nil {
		return err
	}
	if row.ExternalRef == "" {
		return fmt.Errorf("db: triage record with empty external_ref refused")
	}
	loads := row.SkillLoads
	if loads == nil {
		loads = []string{}
	}
	loadsJSON, err := json.Marshal(loads)
	if err != nil {
		return fmt.Errorf("db: marshal skill_loads: %w", err)
	}
	evidence := row.EvidenceIDs
	if evidence == nil {
		evidence = []string{}
	}
	// TG-394 slice 3: the degraded-capability set (migration 0082), ALWAYS written non-null by this build —
	// including as an empty array when nothing was degraded. Same backward-compat mechanism as diagnosis: a
	// NULL column means "this session predates the field", while an explicit '{}' means "checked, nothing
	// degraded" — two distinct facts a later reader must be able to tell apart, so this build never writes NULL.
	degraded := row.DegradedCapabilities
	if degraded == nil {
		degraded = []string{}
	}
	// TG-201: the typed claim, ALWAYS written non-null by this build — including as an empty object when the
	// agent bound no diagnosis. That is the whole backward-compatibility mechanism: a NULL column means "this
	// session ran before the field existed" and the judge scores the dimension N/A, while an explicit empty
	// object means "the field was there and nothing was bound", which is a gradeable fact about the agent.
	// Collapsing the two would either retro-grade every historical session or excuse every future one.
	diagnosisJSON, err := json.Marshal(row.Diagnosis)
	if err != nil {
		return fmt.Errorf("db: marshal diagnosis for %s: %w", row.ExternalRef, err)
	}
	// TG-527: the ordered tool path (migration 0104), ALWAYS written non-null by this build — '[]' when the
	// loop took no tool steps. NULL is reserved for pre-0104 rows, which the axis scores N/A; an explicit
	// empty path is a gradeable fact about the session.
	traj := row.Trajectory
	if traj == nil {
		traj = []judge.TrajectoryStep{}
	}
	trajectoryJSON, err := json.Marshal(traj)
	if err != nil {
		return fmt.Errorf("db: marshal trajectory for %s: %w", row.ExternalRef, err)
	}
	// TG-398: step_count (axis A6a) is set only on the investigation SUCCESS path — the activity carries
	// `StepCount: len(res.Steps)` out of InvestigateActivity, and the workflow copies it onto the result
	// AFTER the error branch (temporal/runner/workflow.go:368). A session whose InvestigateActivity returned
	// an ERROR therefore records step_count at its zero value even though the loop DID run — and already
	// persisted — real ReAct cycles to agent_step BEFORE it failed (the emit at activities.go:751 precedes the
	// error return at :826). Measured on prod: all 135 failed:investigate rows read 0 while carrying 321
	// agent_step rows, which turned the pve03 outage's headline "134 sessions did ZERO steps, mean 0.60" into
	// a measurement artifact (the true DEEP_INVESTIGATION mean was 2.79) and is the severity basis two other
	// tickets cite.
	//
	// So when the incoming count is zero, derive it from the durable transcript the session already wrote,
	// keyed by the per-session external_ref — NOT the content-hashed action_id, which collapses across
	// sibling sessions proposing the identical action (TG-142). This makes 0 mean exactly "no cycle ran"
	// (a genuine stand-down writes no agent_step rows, so the derivation returns 0 and the row is unchanged),
	// the same discipline decision_ms already follows, instead of the two-meanings zero it means today.
	//
	// FAIL-SOFT: a count-query error leaves the incoming zero and still records the row. RecordTriage's
	// caller treats the whole record as best-effort, and a durable triage row (the load-bearing fact) must
	// never be dropped for a missing observability field — 0-as-unmeasured is the safe direction when the
	// transcript cannot be read, exactly as decision_ms floors to 0.
	stepCount := row.StepCount
	if stepCount == 0 {
		var n int
		if qerr := s.p.QueryRow(ctx,
			`SELECT count(*) FROM agent_step WHERE external_ref = $1`, row.ExternalRef).Scan(&n); qerr != nil {
			log.Printf("db: derive step_count for %s failed, recording unmeasured 0 (session unaffected): %v", row.ExternalRef, qerr)
		} else {
			stepCount = n
		}
	}
	_, err = s.p.Exec(ctx, `
		INSERT INTO session_triage
			(external_ref, host, alert_rule, band, outcome, proposed, op, op_class, step_count, decision_ms, evidence_ids, conclusion, stop_reason, skill_loads, prediction, predicted, confidence, actor_attribution, actor_evidence, prompt_version, seed_hash, model_tier, decision_model_tier, mutated, undo_sketch, diagnosis, degraded_capabilities, trajectory, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29)
		ON CONFLICT (external_ref) DO NOTHING`,
		row.ExternalRef, row.Host, row.AlertRule, row.Band, row.Outcome, row.Proposed, row.Op, row.OpClass, stepCount,
		// TG-205: step_count is axis A6a (how many cycles), decision_ms axis A6b (how LONG) — two different
		// questions about the same decision, and only the second is the MTTR the axis is defined as.
		row.DecisionMillis,
		evidence, row.Conclusion, row.StopReason, loadsJSON, row.Prediction, row.Predicted, row.Confidence,
		attributionOrEmpty(row.Attribution), evidenceOrEmpty(row.ActorEvidence),
		// TG-198: model_tier is the tier that INVESTIGATED, decision_model_tier the tier that DECIDED. Two
		// columns because a nudged session did both on different models (migration 0057).
		// TG-394 slice 3: degraded_capabilities is the self-dependency degraded set at session time (migration 0082).
		row.PromptVersion, row.SeedHash, row.ModelTier, row.DecisionTier, row.Mutated, row.UndoSketch, diagnosisJSON, degraded, trajectoryJSON, int(sv))
	if err != nil {
		return fmt.Errorf("db: record triage %s: %w", row.ExternalRef, err)
	}
	return nil
}

// MarkCleared records the orchestrator-confirmed clear on an already-recorded triage (benchmark axis A3
// heal-success numerator, migration 0039). It is a FOLLOW-UP to RecordTriage: ConfirmedClear is only known
// after the workflow's bounded clear-observe loop resolves (TG-124), well after the triage row is first
// inserted, so it cannot ride the insert. Keyed on external_ref (the triage PK); a missing row updates zero
// rows (best-effort, exactly like the rest of the compact triage record). confirmed_clear only ever moves
// false->true here (a mutated heal that later confirms clear); the workflow never re-marks it false.
// OBSERVABILITY ONLY — confirmed_clear re-enters no gate.
func (s *TriageStore) MarkCleared(ctx context.Context, externalRef string, cleared bool) error {
	if externalRef == "" {
		return fmt.Errorf("db: mark-cleared with empty external_ref refused")
	}
	if _, err := s.p.Exec(ctx,
		`UPDATE session_triage SET confirmed_clear = $2 WHERE external_ref = $1`,
		externalRef, cleared); err != nil {
		return fmt.Errorf("db: mark triage cleared %s: %w", externalRef, err)
	}
	return nil
}

// MarkMutated back-fills `mutated` at the session terminus. It exists because the insert above is
// ON CONFLICT (external_ref) DO NOTHING — FIRST-WRITE-WINS — and a session that pauses for a human vote
// records its triage row BEFORE it executes. The post-execute write then silently does nothing, so the row
// keeps the propose-time value of `mutated`: FALSE, for an action that demonstrably ran.
//
// That is not a cosmetic gap. `mutated` is the A3 heal-rate DENOMINATOR (axis_read.go: MutatedCount) and half
// of the A7 ineffective-actuation bound, so the flag decides which incidents enter the published numbers.
// Measured on the day this was found: 94 real executions in action_execution against 91 triage rows marked
// mutated — and the three missing were not random. They were exactly the three SSH-lane heals
// (restart-container x2, start-service x1), because those pause for a vote, while all 91 Proxmox-lane
// start-guest heals execute straight through and record correctly. The bias is OP-CLASS CORRELATED: the
// benchmark counted the lane that never waits and silently dropped every lane that does — which is precisely
// the breadth evidence the roadmap's P4 exists to accrue.
//
// Monotonic by design: it only ever sets TRUE. An execution cannot be un-run, so a later false must never
// erase a recorded mutation. OBSERVABILITY ONLY — `mutated` re-enters no gate.
func (s *TriageStore) MarkMutated(ctx context.Context, externalRef string) error {
	if externalRef == "" {
		return fmt.Errorf("db: mark-mutated with empty external_ref refused")
	}
	if _, err := s.p.Exec(ctx,
		`UPDATE session_triage SET mutated = true WHERE external_ref = $1 AND NOT mutated`,
		externalRef); err != nil {
		return fmt.Errorf("db: mark triage mutated %s: %w", externalRef, err)
	}
	return nil
}

// attributionOrEmpty normalizes a pre-feature attribution to the zero/unknown convention (”).
func attributionOrEmpty(s string) string { return s }

// evidenceOrEmpty normalizes a nil actor-evidence blob to an empty JSON array (the '[]' default).
func evidenceOrEmpty(b []byte) []byte {
	if len(b) == 0 {
		return []byte("[]")
	}
	return b
}

// ErrDiagnosisNotFound is trace.ErrDiagnosisNotFound, aliased so db-side callers need not reach across
// packages for it. It is a REAL and expected answer, not a fault (see Diagnosis below).
var ErrDiagnosisNotFound = trace.ErrDiagnosisNotFound

var _ trace.DiagnosisReader = (*TriageStore)(nil)

// Diagnosis serves ONE session's typed claim to the operator console (TG-201) — root cause, mechanism, the
// evidence that SUPPORTS it, the evidence that CONTRADICTS it, and the alternatives ruled out.
//
// ★ IT READS THE COLUMN THE JUDGE SCORES, AND THAT IS THE WHOLE POINT. `session_triage.diagnosis` (migration
// 0056) is written once, by the terminal triage record, and read by two consumers: the asynchronous judge
// (UnjudgedSince, above) and this method. An earlier cut of the console surface came with its OWN
// `session_diagnosis` table and its own writer beside the investigate activity, which would have given one
// fact two stores — the judge grading a row written on one path while the operator read a row written on
// another, with nothing to say which had drifted. That is this codebase's documented pathology, so the
// surface was rebased onto the column instead. The claim an operator audits is now, by construction, the
// exact bytes the dimension was scored from.
//
// It lives on TriageStore, beside UnjudgedSince, deliberately: the decode is the SAME `proposal.Diagnosis`
// unmarshal, so a third accessor with its own copy of that logic is the next drift waiting to happen.
//
// ErrDiagnosisNotFound IS AN ORDINARY ANSWER, NOT A FAULT — the console renders "no typed claim was recorded"
// for it, which is true, rather than an error banner or (worse) an empty claim that reads as "the agent
// asserted nothing". Three genuinely different rows land there:
//   - no such session at all (pgx.ErrNoRows);
//   - the column is NULL — the session ran before migration 0056 and the field did not exist;
//   - the column holds a claim that is not Present() — the agent bound nothing, e.g. a stand-down.
//
// A MALFORMED BLOB IS A LOUD ERROR (503), never a partially-decoded claim. The obvious shortcut — return the
// lanes that did decode — would drop the CONTRADICTING lane on a damaged row and serve a claim that looks
// unopposed. On this surface that is the single worst outcome available. Note the judge's own read makes the
// OPPOSITE choice on purpose: one bad row must not stop a batch cron, and an ungradeable claim honestly
// scores N/A, whereas an operator asking about ONE session must be told the read failed.
//
// The served claim is BOUNDED (trace.Bound) and says so when it clipped. Read-only; it reaches no actuator
// and re-enters no gate (INV-08).
func (s *TriageStore) Diagnosis(ctx context.Context, externalRef string) (trace.SessionDiagnosis, error) {
	if externalRef == "" {
		return trace.SessionDiagnosis{}, ErrDiagnosisNotFound
	}
	var raw []byte
	err := s.p.QueryRow(ctx,
		`SELECT diagnosis FROM session_triage WHERE external_ref = $1`, externalRef).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return trace.SessionDiagnosis{}, ErrDiagnosisNotFound
	}
	if err != nil {
		return trace.SessionDiagnosis{}, fmt.Errorf("db: read diagnosis %s: %w", externalRef, err)
	}
	if len(raw) == 0 {
		return trace.SessionDiagnosis{}, ErrDiagnosisNotFound
	}
	var d proposal.Diagnosis
	if err := json.Unmarshal(raw, &d); err != nil {
		return trace.SessionDiagnosis{}, fmt.Errorf("db: decode diagnosis %s: %w", externalRef, err)
	}
	if !d.Present() {
		return trace.SessionDiagnosis{}, ErrDiagnosisNotFound
	}
	return diagnosisRead(externalRef, d), nil
}

// diagnosisRead projects the stored domain claim onto the dependency-free read seam.
//
// PURE, AND SPLIT OUT SO AN ORACLE CAN HOLD IT WITHOUT A DATABASE. This projection is where the load-bearing
// property of the whole feature can be lost in silence: drop the Contradicting lane, or re-derive Cited from
// "the id is non-empty", and every downstream surface still renders, still looks right, and quietly stops
// telling an operator the one thing the type exists to tell them.
//
// CITED IS COPIED, NEVER RECOMPUTED. agent/loop.go decided it by matching each id against the ToolResults the
// ORCHESTRATOR captured, and it was persisted with the claim precisely because nothing downstream holds that
// set. Anything this function "derived" would be the model's own word about its own citation — the
// fabricated-citation failure INV-11 exists for.
//
// It does NOT scrub: the screen runs before persist (temporal/runner, REQ-2606), so a secret that reached
// this row already escaped the process that could have caught it, and scrubbing here would hide that.
func diagnosisRead(externalRef string, d proposal.Diagnosis) trace.SessionDiagnosis {
	refs := func(in []proposal.EvidenceRef) []trace.DiagnosisRef {
		if len(in) == 0 {
			return nil
		}
		out := make([]trace.DiagnosisRef, 0, len(in))
		for _, r := range in {
			out = append(out, trace.DiagnosisRef{ID: r.ID, Claim: r.Claim, Cited: r.Cited})
		}
		return out
	}
	rec := trace.SessionDiagnosis{
		ExternalRef:   externalRef,
		RootCause:     d.RootCause,
		Mechanism:     d.Mechanism,
		Supporting:    refs(d.Supporting),
		Contradicting: refs(d.Contradicting),
	}
	for _, a := range d.RuledOut {
		rec.RuledOut = append(rec.RuledOut, trace.DiagnosisAlternative{
			Cause: a.Cause, Reason: a.Reason, ID: a.ID, Cited: a.Cited,
		})
	}
	return rec.Bound()
}

// UnjudgedSince returns the unjudged sessions recorded inside the trailing window, oldest first — the
// judge cron's batch read. Sessions older than the window stay unjudged forever (honest: judging a
// stale record against a moved estate scores noise, not quality).
func (s *TriageStore) UnjudgedSince(ctx context.Context, window time.Duration, limit int) ([]judge.TriageRow, error) {
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, host, alert_rule, band, outcome, proposed, op, evidence_ids, conclusion, skill_loads, prediction, predicted, confidence, judged, created_at, actor_attribution, actor_evidence, undo_sketch, diagnosis
		FROM session_triage
		WHERE NOT judged AND created_at > now() - $1::interval
		ORDER BY created_at ASC
		LIMIT $2`,
		fmt.Sprintf("%d seconds", int(window.Seconds())), limit)
	if err != nil {
		return nil, fmt.Errorf("db: unjudged sessions: %w", err)
	}
	defer rows.Close()
	var out []judge.TriageRow
	for rows.Next() {
		var r judge.TriageRow
		var loads []byte
		var actorEv []byte
		var diagnosis []byte
		if err := rows.Scan(&r.ExternalRef, &r.Host, &r.AlertRule, &r.Band, &r.Outcome, &r.Proposed,
			&r.Op, &r.EvidenceIDs, &r.Conclusion, &loads, &r.Prediction, &r.Predicted, &r.Confidence, &r.Judged, &r.CreatedAt,
			&r.ActorAttribution, &actorEv, &r.UndoSketch, &diagnosis); err != nil {
			return nil, fmt.Errorf("db: scan triage row: %w", err)
		}
		// TG-201: the typed claim the diagnosis_grounded dimension scores. A NULL column is a session from
		// BEFORE migration 0056 — the field did not exist, so DiagnosisRecorded stays false and the judge
		// scores that session N/A rather than floored. An UNPARSEABLE blob is treated the same way (and the
		// batch continues): the honest reading of a claim we cannot decode is "no gradeable claim here", not
		// a fabricated score, and one bad row must never stop the judge cron.
		if len(diagnosis) > 0 {
			if err := json.Unmarshal(diagnosis, &r.Diagnosis); err == nil {
				r.DiagnosisRecorded = true
			} else {
				r.Diagnosis = proposal.Diagnosis{}
			}
		}
		if len(loads) > 0 {
			if err := json.Unmarshal(loads, &r.SkillLoads); err != nil {
				return nil, fmt.Errorf("db: skill_loads for %s: %w", r.ExternalRef, err)
			}
		}
		// Only the COUNT crosses into the judge — the records themselves are external text (actor names,
		// verbs and refs out of other systems' logs) and carry no scoring value the count does not. A malformed
		// blob yields 0 rather than failing the batch: the judge cron must not stop because one row's evidence
		// column is unparseable, and 0 is the honest reading of "no admissible records here".
		if len(actorEv) > 0 {
			var recs []struct{}
			if err := json.Unmarshal(actorEv, &recs); err == nil {
				r.ActorEvidenceCount = len(recs)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecentJudgedByVersion returns the judged sessions that composed a given skill version within the
// trailing window, newest first, capped at limit — the offline admission gate's discovery set
// (spec/014 REQ-1307). Read-only; a session appears only if it was judged (session_triage.judged). The
// composing version is matched by the `#<id>:store` anchor in the skill_load provenance (the bound LIKE
// value is built in Go, never SQL assembled from strings). By construction this yields only the skill's
// OWN judged sessions — never the sealed holdout, which lives outside session_triage.
func (s *TriageStore) RecentJudgedByVersion(ctx context.Context, versionID int64, window time.Duration, limit int) ([]judge.TriageRow, error) {
	loadPattern := fmt.Sprintf("%%#%d:store%%", versionID)
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, host, alert_rule, band, outcome, proposed, op, evidence_ids, conclusion, skill_loads, prediction, predicted, confidence, judged, created_at
		FROM session_triage t
		WHERE t.judged AND t.created_at > now() - $2::interval
		  AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(t.skill_loads) e WHERE e LIKE $1)
		ORDER BY t.created_at DESC
		LIMIT $3`,
		loadPattern, fmt.Sprintf("%d seconds", int(window.Seconds())), limit)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: recent judged sessions for version %d: %w", versionID, err)
	}
	defer rows.Close()
	var out []judge.TriageRow
	for rows.Next() {
		var r judge.TriageRow
		var loads []byte
		if err := rows.Scan(&r.ExternalRef, &r.Host, &r.AlertRule, &r.Band, &r.Outcome, &r.Proposed,
			&r.Op, &r.EvidenceIDs, &r.Conclusion, &loads, &r.Prediction, &r.Predicted, &r.Confidence, &r.Judged, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan judged session: %w", err)
		}
		if len(loads) > 0 {
			if err := json.Unmarshal(loads, &r.SkillLoads); err != nil {
				return nil, fmt.Errorf("db: skill_loads for %s: %w", r.ExternalRef, err)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkJudged flags the session as judged so the next batch read skips it.
func (s *TriageStore) MarkJudged(ctx context.Context, externalRef string) error {
	if _, err := s.p.Exec(ctx,
		`UPDATE session_triage SET judged = true WHERE external_ref = $1`, externalRef); err != nil {
		return fmt.Errorf("db: mark judged %s: %w", externalRef, err)
	}
	return nil
}

// WriteJudgment upserts one (session, dimension) verdict — a re-judge overwrites, never duplicates,
// so armScoresForDim's join sees exactly one score per session per dimension.
//
// rubricVersion is the declared identity of the rubric that produced the score (TG-194) — callers pass
// judge.RubricVersion(); an empty value is refused because a new row that cannot say which rubric graded
// it recreates the un-attributable pool this column exists to end.
//
// action_id (TG-195) is DERIVED in the statement, not passed: the ONE action the session sealed, from
// session_risk_audit, ” when it sealed zero or several — the same exactly-one rule migration 0052's
// backfill applied, so historical and new rows carry one semantics. A re-judge refreshes it (ON
// CONFLICT), so a session judged before its audit rows landed converges on the next pass.
func (s *TriageStore) WriteJudgment(ctx context.Context, externalRef, dimension string, score float64, comment, rubricVersion string) error {
	if rubricVersion == "" {
		return fmt.Errorf("db: write judgment %s/%s: rubric version is empty — a new judgment must be attributable to the rubric that produced it (TG-194)", externalRef, dimension)
	}
	sv, err := schema.Stamp(schema.TableSessionJudgment)
	if err != nil {
		return err
	}
	_, err = s.p.Exec(ctx, `
		INSERT INTO session_judgment (external_ref, dimension, score, comment, schema_version, rubric_version, action_id)
		VALUES ($1, $2, $3, $4, $5, $6, COALESCE((
			SELECT min(action_id) FROM session_risk_audit
			WHERE external_ref = $1 AND action_id <> ''
			HAVING count(DISTINCT action_id) = 1), ''))
		ON CONFLICT (external_ref, dimension)
		DO UPDATE SET score = EXCLUDED.score, comment = EXCLUDED.comment, judged_at = now(),
		              rubric_version = EXCLUDED.rubric_version, action_id = EXCLUDED.action_id`,
		externalRef, dimension, score, comment, int(sv), rubricVersion)
	if err != nil {
		return fmt.Errorf("db: write judgment %s/%s: %w", externalRef, dimension, err)
	}
	return nil
}

// ShadowProposalRow is one shadow (free-form, never-executable) proposal for the read-only console lane
// (spec/026 REQ-2607). Every field is the already-screened persisted record — the reader adds nothing.
type ShadowProposalRow struct {
	ExternalRef string `json:"external_ref"`
	Host        string `json:"host"`
	AlertRule   string `json:"alert_rule"`
	Op          string `json:"op"`
	OpClass     string `json:"op_class"`
	// Band is why an un-executed proposal stopped where it did: POLL_PAUSE waited on a human vote that
	// never came, AUTO/AUTO_NOTICE stopped elsewhere. Carried so the console can state the REAL reason per
	// shape instead of one hardcoded sentence that is only true for the rows lacking an op-class.
	Band        string    `json:"band"`
	Rationale   string    `json:"rationale"` // the persisted conclusion (the proposal's rationale)
	UndoSketch  string    `json:"undo_sketch"`
	Confidence  float64   `json:"confidence"`
	Attribution string    `json:"attribution"`
	CreatedAt   time.Time `json:"created_at"`
	// TG-307: the derived diagnosis SIGNAL for the operator's proposal-review lane — never the claim TEXT.
	// DiagnosisRecorded is Present() (the agent bound a typed claim at all); DiagnosisContradicted is the
	// recorded A2 failure made visible (the agent cited GROUNDED evidence AGAINST its own root cause);
	// DiagnosisUncited counts assertions bound to no gathered observation. Computed off the SAME
	// session_triage.diagnosis column the judge scores and the #reasoning walk renders, through the SAME
	// core/proposal.Diagnosis decode, so a second definition of "grounded contradiction" cannot drift in here.
	// The claim's own text (root cause, mechanism, the screened evidence quotes) is deliberately absent — it
	// is trace-read-gated on /v1/sessions/{ref}/diagnosis (AuthTraceRead), and this lane is AuthReadOnly.
	DiagnosisRecorded     bool `json:"diagnosis_recorded"`
	DiagnosisContradicted bool `json:"diagnosis_contradicted"`
	DiagnosisUncited      int  `json:"diagnosis_uncited"`
}

// shadowDiagnosisSignal derives the read-only proposal-lane SIGNAL from a stored session_triage.diagnosis
// blob. It reuses the ONE decode (core/proposal.Diagnosis) and the domain type's OWN predicates, so the
// "grounded contradiction" the operator sees on the proposals lane is by construction the same fact the
// diagnosis_grounded dimension scored and the #reasoning walk renders — not a second, quietly-laxer copy.
//
// IT RETURNS THE SIGNAL, NEVER THE CLAIM TEXT. Root cause, mechanism and the screened evidence quotes are a
// detail OF the walk and are served only behind the elevated AuthTraceRead gate (/v1/sessions/{ref}/diagnosis);
// the proposals lane is AuthReadOnly, so surfacing that text here would be a way around the tracer's own
// authority. A boolean "this proposal recorded evidence against its own root cause" is a trust signal of the
// same class as the rationale and confidence this lane already serves — and the one an operator most needs at
// the moment of review.
//
// A NULL/empty column (a pre-migration-0056 session, or an agent that bound no claim) is recorded=false: the
// honest "no typed claim", which the console renders as silence rather than a false all-clear. A MALFORMED
// blob is read the SAME batch-safe way UnjudgedSince reads it — recorded=false, never an error that would
// blank the whole list for one bad row. The single-session Diagnosis read makes the opposite, louder choice
// (503) on purpose: an operator asking about ONE session must be told the read failed, but the list must not.
func shadowDiagnosisSignal(raw []byte) (recorded, contradicted bool, uncited int) {
	if len(raw) == 0 {
		return false, false, 0
	}
	var d proposal.Diagnosis
	if err := json.Unmarshal(raw, &d); err != nil {
		return false, false, 0
	}
	if !d.Present() {
		return false, false, 0
	}
	return true, d.HasContradiction(), d.UncitedAssertions()
}

// shadowProposalWhere selects what the console's own contract calls a shadow proposal: "named, recorded,
// NEVER EXECUTED".
//
// ★ THIS PREDICATE USED TO BE `outcome = 'proposed:shadow'` AND IT HID THE ENTIRE PLANE. Measured against
// production on 2026-08-01: the spine held 1,990 rows with outcome 'proposed' and exactly ONE with
// 'proposed:shadow', so the surface the board calls "on day zero the entire product" rendered a single row
// over a store holding 1,484 un-executed proposals in 18 recurring shapes. The two outcomes are written by
// different branches of the runner and mean different things — 'proposed:shadow' is the narrow case where
// TG proposed something it has NO REGISTERED OP-CLASS for (temporal/runner/workflow.go:310, the
// earned-catalog feed), while plain 'proposed' is a remedy for a verb TG already owns
// (workflow.go:495,763) — but BOTH are proposals, and neither executed unless `mutated` says so.
//
// `NOT mutated` is what carries the "never executed" half, and it is load-bearing now that mutation is ON:
// 506 of those 1,990 rows DID actuate, and listing an executed action on a plane captioned "never
// executes" would be the same class of lie in the opposite direction. The old predicate was accidentally
// safe here only because it matched almost nothing.
// The NOT EXISTS is not belt-and-braces: `mutated` has a KNOWN, DOCUMENTED, OP-CLASS-CORRELATED gap.
// RecordTriage is first-write-wins, and a session that pauses for a human vote writes its row BEFORE it
// executes, so the post-execute write no-ops and the row keeps mutated=false for an action that
// demonstrably ran (see MarkMutated above, which exists to back-fill exactly this). Measured on
// production 2026-08-01: THREE rows are marked not-mutated while carrying a real action_execution, and all
// three are POLL_PAUSE — the same vote-waiting lane MarkMutated's comment names. Trusting the flag alone
// would put three genuinely executed actions on a plane captioned "never executes", so the execution
// table is consulted directly and the caption becomes true rather than approximately true.
const shadowProposalWhere = `outcome LIKE 'proposed%' AND NOT mutated
	AND NOT EXISTS (SELECT 1 FROM action_execution e WHERE e.external_ref = session_triage.external_ref)`

// ListShadowProposals returns the newest un-executed proposals, newest first, bounded.
// Read-only; an empty spine returns an empty slice, never fabricated rows (INV-15).
func (s *TriageStore) ListShadowProposals(ctx context.Context, limit int) ([]ShadowProposalRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// TG-307: `diagnosis` rides the SAME row — a shadow proposal IS a session_triage record, and the typed
	// claim is a column on it. Reading it here (rather than fanning out to /v1/sessions/{ref}/diagnosis
	// per row) keeps the signal one query and one authority with the list it annotates.
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, host, alert_rule, op, op_class, band, conclusion, undo_sketch, confidence,
		       actor_attribution, created_at, diagnosis
		FROM session_triage
		WHERE `+shadowProposalWhere+`
		ORDER BY created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list shadow proposals: %w", err)
	}
	defer rows.Close()
	out := []ShadowProposalRow{}
	for rows.Next() {
		var r ShadowProposalRow
		var diagnosis []byte
		if err := rows.Scan(&r.ExternalRef, &r.Host, &r.AlertRule, &r.Op, &r.OpClass, &r.Band, &r.Rationale,
			&r.UndoSketch, &r.Confidence, &r.Attribution, &r.CreatedAt, &diagnosis); err != nil {
			return nil, fmt.Errorf("db: scan shadow proposal: %w", err)
		}
		r.DiagnosisRecorded, r.DiagnosisContradicted, r.DiagnosisUncited = shadowDiagnosisSignal(diagnosis)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountShadowProposals returns the REAL number of shadow proposals in the store — the console badge's
// honest total (spec/026 REQ-2607). Listing is page-limited; the count is not, so the badge can never
// silently degrade into "page size" once the store outgrows the limit.
func (s *TriageStore) CountShadowProposals(ctx context.Context) (int, error) {
	var n int
	if err := s.p.QueryRow(ctx,
		`SELECT count(*) FROM session_triage WHERE `+shadowProposalWhere).Scan(&n); err != nil {
		return 0, fmt.Errorf("db: count shadow proposals: %w", err)
	}
	return n, nil
}

// CounterfactualSince answers the one number that makes a shadow plane legible to an operator: over the
// window, how many incidents did TG see, and for how many of them did it have an answer it was not
// allowed to apply (spec/026, TG-236 oracle 4).
//
// It returns (incidents, addressed). ADDRESSED counts incidents whose session ended in a shadow proposal
// — TG named a fix and could not execute it. INCIDENTS is every triage session in the window, including
// the ones TG correctly stood down on, because a denominator that quietly excluded them would inflate
// the ratio into a slogan. "TG would have addressed 14 of 17" is only worth printing if the 17 is real.
//
// One query, not two: a separate count and sub-count taken at different instants can disagree under
// concurrent writes and produce a ratio above 1.0, which reads as a bug in the number rather than in the
// query.
// ★ THE NUMERATOR WAS `outcome = 'proposed:shadow'` AND IT UNDERSTATED TG BY THREE ORDERS OF MAGNITUDE.
// Run against production on 2026-08-01 this headline would have read "This week TG would have addressed
// 1 of 2,699 incidents". The honest figure is 1,748 — 'proposed:shadow' is the rare branch where TG had no
// registered op-class (one row in 3,202), not the set of incidents it produced a remedy for.
//
// `executed` is reported alongside because mutation is ON in this deployment: of those 1,748, TG was
// allowed to carry out 506. A single "would have addressed" figure that silently mixes what TG DID with
// what it was BLOCKED from doing is a worse number than either — it invites an operator to grant a
// capability that is, in part, already granted. Three counts from ONE scan so they cannot disagree.
func (s *TriageStore) CounterfactualSince(ctx context.Context, since time.Time) (incidents, addressed, executed int, err error) {
	if err := s.p.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE outcome LIKE 'proposed%'),
		       count(*) FILTER (WHERE outcome LIKE 'proposed%' AND mutated)
		FROM session_triage
		WHERE created_at >= $1`, since).Scan(&incidents, &addressed, &executed); err != nil {
		return 0, 0, 0, fmt.Errorf("db: counterfactual since %s: %w", since.UTC().Format(time.RFC3339), err)
	}
	return incidents, addressed, executed, nil
}
