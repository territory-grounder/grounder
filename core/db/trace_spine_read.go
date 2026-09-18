package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/trace"
)

// TraceSpineStore is the pgx-backed READ side of the decision tracer (spec/020 REQ-2011): it loads the
// durable correlation spine for ONE external_ref and returns it as trace.SpineRecords for the pure
// trace.Assemble to stitch. Read-only by construction — four bound SELECTs, no mutation, every parameter
// bound ($1), never string-built. It joins classify (session_risk_audit) → propose (session_triage) →
// predict (infragraph_prediction, by external_ref, migration 0026) → verify (action_verdict, by action_id).
// An external_ref with neither a classification nor a triage row is trace.ErrNotFound (an unknown session),
// never an empty walk.
type TraceSpineStore struct{ p *Pool }

// NewTraceSpineStore returns the Postgres-backed decision-tracer spine reader.
func NewTraceSpineStore(p *Pool) *TraceSpineStore { return &TraceSpineStore{p: p} }

// compile-time proof it satisfies the read seam the detail endpoint depends on.
var _ trace.SpineReader = (*TraceSpineStore)(nil)

// Load assembles the spine for one external_ref. Authority is enforced upstream at the authenticated route;
// this reads the committed spine only. Missing sub-rows leave their Present=false (a queued session has only
// the classification), so the assembler yields the honest partial prefix.
func (s *TraceSpineStore) Load(ctx context.Context, externalRef string) (trace.SpineRecords, error) {
	var rec trace.SpineRecords

	// ingest — the durable front-door alert record (ingest_alert, migration 0033), latest for the ref. Non-secret
	// normalized envelope fields only (INV-13). Absent for a session that bypassed the front door (pull-minted).
	var ingSeverity, ingSummary, ingSite string
	err := s.p.QueryRow(ctx, `
		SELECT source_type, alert_rule, severity, host, site, summary, received_at
		FROM ingest_alert
		WHERE external_ref = $1
		ORDER BY received_at DESC, id DESC
		LIMIT 1`, externalRef).Scan(
		&rec.Ingest.SourceType, &rec.Ingest.AlertRule, &ingSeverity, &rec.Ingest.Host, &ingSite, &ingSummary, &rec.Ingest.ReceivedAt)
	switch {
	case err == nil:
		rec.Ingest.Present = true
		rec.Ingest.Severity, rec.Ingest.Site, rec.Ingest.Summary = ingSeverity, ingSite, ingSummary
	case errors.Is(err, pgx.ErrNoRows):
		// no durable front-door record (pull-minted or pre-0033 session) — the console shows its light scaffold.
	default:
		return rec, fmt.Errorf("db: trace ingest read: %w", err)
	}

	// correlate — the deterministic suppression/correlation decision committed to the governance ledger under
	// action_id "suppress:"+external_ref (core/suppression Chain.Decide, one immutable record per run). decision
	// is "suppress:<outcome>"; the projection strips the "suppress:" prefix to the bare outcome. Non-secret
	// governance projection (INV-13). Absent for a session that predates the suppression-ledger wiring.
	var corrDecision, corrReason string
	err = s.p.QueryRow(ctx, `
		SELECT decision, reason, created_at
		FROM governance_ledger
		WHERE action_id = 'suppress:' || $1
		ORDER BY created_at DESC, seq DESC
		LIMIT 1`, externalRef).Scan(&corrDecision, &corrReason, &rec.Correlate.DecidedAt)
	switch {
	case err == nil:
		rec.Correlate.Present = true
		rec.Correlate.Outcome = strings.TrimPrefix(corrDecision, "suppress:")
		rec.Correlate.Reason = corrReason
	case errors.Is(err, pgx.ErrNoRows):
		// no suppression decision recorded for this ref (pre-wiring session) — the console shows its scaffold.
	default:
		return rec, fmt.Errorf("db: trace correlate read: %w", err)
	}

	// classify — latest classification for the ref. signals_json (migration 0003) carries the admission-time
	// screen signals (poll_reason / never-auto-floor / blast-radius) — read for the screen step.
	var signalsJSON []byte
	err = s.p.QueryRow(ctx, `
		SELECT band::text, risk_level, action_id, COALESCE(plan_hash, ''), auto_approved, created_at, COALESCE(signals_json, '{}'::jsonb)
		FROM session_risk_audit
		WHERE external_ref = $1
		ORDER BY created_at DESC
		LIMIT 1`, externalRef).Scan(
		&rec.Classification.Band, &rec.Classification.RiskLevel, &rec.Classification.ActionID,
		&rec.Classification.PlanHash, &rec.Classification.AutoApproved, &rec.Classification.CreatedAt, &signalsJSON)
	switch {
	case err == nil:
		rec.Classification.Present = true
		rec.Classification.Signals = decodeSignals(signalsJSON) // tolerant: a non-string value degrades that ONE signal, never drops the whole screen
	case errors.Is(err, pgx.ErrNoRows):
		// no classification — may still be a triage-only row; fall through.
	default:
		return rec, fmt.Errorf("db: trace classify read: %w", err)
	}

	// propose — the parsed proposal + stated confidence (migration 0024). skill_loads (migration 0010) carries the
	// composed-seed retrieval provenance — read for the rag step.
	// prompt_version/seed_hash/model_tier (session_identity, migration 0027) are the composed-seed identity — read
	// for the propose step's prompts provenance (REQ-2009/REQ-2000).
	// decision_model_tier (migration 0057, TG-198) is read alongside because the PROPOSE step is the DECISION:
	// labelling it with the investigation tier told the operator the fast model authored a proposal the TG-60
	// nudge had actually handed to the reasoner. COALESCE keeps a pre-0057 row readable — it renders no
	// decision tier at all rather than inventing one.
	var skillLoadsJSON, actorEvidenceJSON []byte
	err = s.p.QueryRow(ctx, `
		SELECT host, alert_rule, band, outcome, proposed, op, conclusion, confidence, created_at, COALESCE(skill_loads, '[]'::jsonb),
		       COALESCE(prompt_version, ''), COALESCE(seed_hash, ''), COALESCE(model_tier, ''), COALESCE(decision_model_tier, ''),
		       -- spec/023 REQ-2311: the attribution the tracer surfaces as its own boundary. COALESCE to '' so a
		       -- pre-attribution session reads as ABSENT (no attribute step) rather than as a fabricated verdict.
		       COALESCE(actor_attribution, ''), COALESCE(actor_evidence, '[]'::jsonb)
		FROM session_triage
		WHERE external_ref = $1`, externalRef).Scan(
		&rec.Triage.Host, &rec.Triage.AlertRule, &rec.Triage.Band, &rec.Triage.Outcome,
		&rec.Triage.Proposed, &rec.Triage.Op, &rec.Triage.Conclusion, &rec.Triage.Confidence,
		&rec.Triage.CreatedAt, &skillLoadsJSON,
		&rec.Triage.PromptVersion, &rec.Triage.SeedHash, &rec.Triage.ModelTier, &rec.Triage.DecisionTier,
		&rec.Triage.Attribution, &actorEvidenceJSON)
	switch {
	case err == nil:
		rec.Triage.Present = true
		if len(skillLoadsJSON) > 0 {
			_ = json.Unmarshal(skillLoadsJSON, &rec.Triage.SkillLoads) // best-effort; rag step omitted if unparseable
		}
		// The attribution's DOMAIN-NATIVE references only — "<domain>:<ref>" — never the actor, never the raw
		// record (INV-13). The taxonomy is the finding; these are the pointers an operator follows to check it.
		rec.Triage.AttributionEvidence = attributionRefs(actorEvidenceJSON)
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return rec, fmt.Errorf("db: trace propose read: %w", err)
	}

	if !rec.Classification.Present && !rec.Triage.Present && !rec.Ingest.Present && !rec.Correlate.Present {
		return rec, trace.ErrNotFound
	}

	// predict — the committed consequence prediction + its verify-time falsify score (tp NULL = unscored).
	// Predictions are keyed by the content-hashed action_id; the external_ref column landed later (migration
	// 0026) so the prediction writer does not stamp it on every row (older/most rows carry an empty
	// external_ref). Join by the classification's action_id (falling back to external_ref) so a committed
	// prediction is never orphaned — the "commit" step showed "no data yet" for every session whose
	// prediction had an empty external_ref, even though the row existed under its action_id.
	var tp, fp, fn *int
	predActionID := rec.Classification.ActionID
	err = s.p.QueryRow(ctx, `
		SELECT action_id, plan_hash, tp, fp, fn, committed_at
		FROM infragraph_prediction
		WHERE (($1 <> '' AND action_id = $1) OR ($2 <> '' AND external_ref = $2)) AND kind = 'action'
		ORDER BY committed_at DESC
		LIMIT 1`, predActionID, externalRef).Scan(
		&rec.Prediction.ActionID, &rec.Prediction.PlanHash, &tp, &fp, &fn, &rec.Prediction.CommittedAt)
	switch {
	case err == nil:
		rec.Prediction.Present = true
		if tp != nil {
			rec.Prediction.Scored = true
			rec.Prediction.TP, rec.Prediction.FP, rec.Prediction.FN = deref(tp), deref(fp), deref(fn)
		}
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return rec, fmt.Errorf("db: trace predict read: %w", err)
	}

	// The content-hashed action id (INV-07). NOTE it is the identity of the ACTION SHAPE, not of one
	// occurrence — the mechanical-verdict read that uses it lives AFTER the gate-chain read below, because it
	// needs this incident's own timeline to tell its verdict from a previous incident's. See there.
	actionID := rec.Classification.ActionID
	if actionID == "" {
		actionID = rec.Prediction.ActionID
	}

	// agent cycles — the scrubbed ReAct investigation cycles (agent_step, T-020-8), cycle-ordered. One row per
	// transcript STEP: a single-tool cycle is one step, and a BATCHED directive (TG-49) is one step per batched
	// call, all sharing the cycle ordinal in directive order (a.id ASC keeps that order within the cycle).
	// LEFT JOIN, never INNER: a cycle whose ground truth was not recorded — every cycle in every session
	// predating migration 0053 — must still appear in the walk. An inner join would silently delete the
	// reasoning history of 3241 sessions from the tracer the day this shipped.
	//
	// THE JOIN BINDS EACH STEP TO ITS OWN EVIDENCE, NOT TO ITS CYCLE'S. (external_ref, cycle) stopped being a
	// per-step key the day TG-49 let one cycle hold N steps and N evidence rows: joined on the pair alone, a
	// 2-call batched cycle came back as the 2×2 cross product — four rendered steps, two of them citing the
	// SIBLING's payload as their "ground truth", on the one surface (TG-272) whose whole premise is that a
	// claim's citation opens the claim's own evidence. The loop stamps the evidence id INTO the step's own
	// observation ("observed <id>", or "observed <id> (tool call FAILED)" — TG-199's two orchestrator-authored
	// shapes, byte-stable since 0031), so matching the observation against the evidence id is an exact
	// per-step correlation with no schema change and no behaviour change for the entire pre-batch corpus
	// (unique (ref, cycle) rows match exactly as before). A step whose observation was pathologically altered
	// by the Scrub pass simply misses the join and renders citation-less — fail-soft, never mispaired.
	cyRows, err := s.p.Query(ctx, `
		SELECT a.cycle, a.thought, a.tool, a.observation, a.outcome, a.created_at,
		       COALESCE(e.evidence_id, '')
		FROM agent_step a
		LEFT JOIN agent_step_evidence e
		       ON e.external_ref = a.external_ref AND e.cycle = a.cycle
		      AND a.observation IN ('observed ' || e.evidence_id,
		                            'observed ' || e.evidence_id || ' (tool call FAILED)')
		WHERE a.external_ref = $1
		ORDER BY a.cycle ASC, a.id ASC`, externalRef)
	if err != nil {
		return rec, fmt.Errorf("db: trace agent-cycle read: %w", err)
	}
	for cyRows.Next() {
		var a trace.AgentCycleRecord
		if err := cyRows.Scan(&a.Cycle, &a.Thought, &a.Tool, &a.Observation, &a.Outcome, &a.CreatedAt, &a.EvidenceID); err != nil {
			cyRows.Close()
			return rec, fmt.Errorf("db: trace agent-cycle scan: %w", err)
		}
		rec.AgentCycles = append(rec.AgentCycles, a)
	}
	cyRows.Close()
	if err := cyRows.Err(); err != nil {
		return rec, fmt.Errorf("db: trace agent-cycle rows: %w", err)
	}

	// credential resolutions — the non-secret machine identities the investigation resolved (credential_resolution,
	// REQ-2015), keyed by the same external_ref (migration 0032). One step per resolution; only the resolved user,
	// connection scheme, and key-reference SCHEME are read — never key material or a full ref value (INV-13).
	crRows, err := s.p.Query(ctx, `
		SELECT target, outcome, resolved_user, scheme, key_ref_scheme, source, created_at
		FROM credential_resolution
		WHERE external_ref = $1 AND plane = 'machine'
		ORDER BY created_at ASC, id ASC`, externalRef)
	if err != nil {
		return rec, fmt.Errorf("db: trace credential read: %w", err)
	}
	for crRows.Next() {
		var c trace.CredentialRecord
		if err := crRows.Scan(&c.Target, &c.Outcome, &c.User, &c.Scheme, &c.KeyRefScheme, &c.Source, &c.CreatedAt); err != nil {
			crRows.Close()
			return rec, fmt.Errorf("db: trace credential scan: %w", err)
		}
		rec.Credentials = append(rec.Credentials, c)
	}
	crRows.Close()
	if err := crRows.Err(); err != nil {
		return rec, fmt.Errorf("db: trace credential rows: %w", err)
	}

	// The execute-path enrichments key by the content-hashed action_id (INV-07), which only exists once the
	// session sealed an action; a queued/stopped session has none, so these contribute no steps.
	if actionID != "" {
		// commit plan ops — the sealed end-state, projected from action_manifest.action (jsonb = manifest.Action,
		// content-hashed by action_id, INV-07). Read-only projection to structured ops; no secret material (the
		// manifest's non-secret target/op-class/params only, INV-13). Absent manifest → no plan ops (honest).
		var actionJSON []byte
		err = s.p.QueryRow(ctx, `SELECT action FROM action_manifest WHERE action_id = $1`, actionID).Scan(&actionJSON)
		switch {
		case err == nil:
			var a manifest.Action
			if len(actionJSON) > 0 {
				if err := json.Unmarshal(actionJSON, &a); err != nil {
					return rec, fmt.Errorf("db: trace commit action unmarshal: %w", err)
				}
				rec.Commit.PlanOps = trace.ProjectPlanOps(a.Target, a.OpClass, a.Op, a.Params)
				rec.Commit.Present = true
			}
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return rec, fmt.Errorf("db: trace commit action read: %w", err)
		}

		// regime-select boundary (TG-412, REQ-2001): the effect lane the actuation regime engine resolved for
		// THIS action (regime_actuation, action_id-keyed, migration 0020). NON-SECRET — lane + op-class only,
		// never the job token or id (INV-13). Absent when the action never actuated via the regime engine.
		var rgLane, rgOpClass string
		var rgAt time.Time
		switch err := s.p.QueryRow(ctx, `
			SELECT lane, op_class, created_at
			FROM regime_actuation
			WHERE action_id = $1
			ORDER BY created_at ASC
			LIMIT 1`, actionID).Scan(&rgLane, &rgOpClass, &rgAt); {
		case err == nil:
			rec.Regime = trace.RegimeRecord{Present: true, Lane: rgLane, OpClass: rgOpClass, DecidedAt: rgAt}
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return rec, fmt.Errorf("db: trace regime read: %w", err)
		}

		// policy authorization — the composed Decide detail (policy_decision, T-020-3): verdict, in-force bundle
		// version, the FULL matched-rule list (jsonb {id,verdict}, projected non-secret), packet-tracer reason, mode.
		var (
			pVerdict, pBundle, pReason, pMode string
			matchedJSON                       []byte
		)
		//
		// SCOPED TO THIS INCIDENT, for the same reason the gate chain below is. action_id is the hash of the
		// action SHAPE, so `WHERE action_id = $1 ORDER BY created_at DESC LIMIT 1` returned the MOST RECENT
		// decision anyone ever made about that shape — measured live 2026-07-29: 17 of 30 consecutive sessions
		// rendered a policy decision stamped before their own proposal, one of them a full day earlier. A
		// governed actuation's authorization is the first thing an auditor reads, and it was showing a verdict,
		// a bundle version, a rule list and a hash from a decision made on other evidence, about another
		// incident. Nothing marked it as such.
		//
		// migration 0028 added external_ref to this table for precisely this correlation and the read never
		// used it. The predicate tolerates '' so rows written before 0028 still resolve (their correlation is
		// simply absent, as it always was), and the ORDER BY prefers an exact ref match over such a legacy row.
		// What it can no longer do is return a row that belongs to a DIFFERENT, KNOWN incident.
		err = s.p.QueryRow(ctx, `
			SELECT verdict, bundle_version, matched_rules, reason, mode, created_at, COALESCE(min_confidence, 0)
			FROM policy_decision
			WHERE action_id = $1 AND external_ref IN ($2, '')
			ORDER BY (external_ref = $2) DESC, created_at DESC
			LIMIT 1`, actionID, externalRef).Scan(&pVerdict, &pBundle, &matchedJSON, &pReason, &pMode, &rec.Policy.CreatedAt, &rec.Policy.MinConfidence)
		switch {
		case err == nil:
			rec.Policy.Present = true
			rec.Policy.Verdict, rec.Policy.BundleVersion = pVerdict, pBundle
			rec.Policy.Reason, rec.Policy.Mode = pReason, pMode
			var mr []PolicyMatchedRule
			if len(matchedJSON) > 0 {
				if err := json.Unmarshal(matchedJSON, &mr); err != nil {
					return rec, fmt.Errorf("db: trace policy matched_rules unmarshal: %w", err)
				}
			}
			for _, m := range mr {
				rec.Policy.MatchedRules = append(rec.Policy.MatchedRules, m.ID+" → "+m.Verdict)
			}
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return rec, fmt.Errorf("db: trace policy read: %w", err)
		}

		// interceptor gate chain — one row per ordered gate a governed actuation traversed
		// (interceptor_gate_verdict, T-020-7), ordinal-ascending, SCOPED TO THIS INCIDENT.
		//
		// THE external_ref PREDICATE IS LOAD-BEARING. action_id is CONTENT-ADDRESSED: the same action shape
		// (same op-class, target and params) hashes to the same id on every incident it is ever proposed for,
		// forever. Keyed on action_id alone this query returned every gate row that action had EVER produced —
		// measured live 2026-07-29: 503 rows across 47 distinct incidents spanning six days, rendered as ONE
		// session's chain. The operator saw "170-gate chain", sixteen copies of each gate, timestamps from
		// three different days interleaved, and — worst — `policy: refuse` sitting among `policy: pass` rows in
		// the same list, which reads as the gate contradicting itself within a single decision. It was two
		// different decisions on two different days.
		//
		// The ref scopes it to the one incident: 503 rows -> 11, the actual chain. Every row in the table
		// carries a ref (6,885 of 6,885 since 2026-07-21), so this narrows nothing that exists; a row written
		// without one (the column defaults to '') would drop out of the walk rather than pollute every other
		// walk, which is the safe direction — an absent gate row is visibly missing, a foreign one is
		// indistinguishable from truth.
		gRows, err := s.p.Query(ctx, `
			SELECT ordinal, gate, verdict, reason, created_at, margin
			FROM interceptor_gate_verdict
			WHERE action_id = $1 AND external_ref = $2
			ORDER BY ordinal ASC`, actionID, externalRef)
		if err != nil {
			return rec, fmt.Errorf("db: trace gate-verdict read: %w", err)
		}
		for gRows.Next() {
			var g trace.GateVerdictRecord
			// margin is nullable (0076): a binary gate and every pre-column row scan to a nil *float64.
			if err := gRows.Scan(&g.Ordinal, &g.Gate, &g.Verdict, &g.Reason, &g.CreatedAt, &g.Margin); err != nil {
				gRows.Close()
				return rec, fmt.Errorf("db: trace gate-verdict scan: %w", err)
			}
			rec.GateVerdicts = append(rec.GateVerdicts, g)
		}
		gRows.Close()
		if err := gRows.Err(); err != nil {
			return rec, fmt.Errorf("db: trace gate-verdict rows: %w", err)
		}

		// verify — the mechanical verdict (action_verdict, INV-10). THIS TABLE CANNOT BE SCOPED BY A PREDICATE:
		// action_id is its PRIMARY KEY and the ledger is append-only first-wins, so ONE row exists per action
		// SHAPE for all time. The second incident to propose the same shape writes nothing, and inherits the
		// first one's verdict on read.
		//
		// Measured live 2026-07-29 over 30 consecutive sessions: 22 carried a verdict, and all 22 predated
		// their own proposal — three distinct sessions all showed the same `match` stamped 2026-07-23T02:20:23,
		// six days earlier. The count is exact, not approximate: every session the console labelled "executed"
		// was labelled so on another incident's evidence, because deriveStatus keys StatusExecuted off
		// Verdict.Present. A verdict is the PROOF that a heal did what it claimed; a borrowed one is a claim of
		// proof, which is worse than no proof, and one of the borrowed verdicts read `deviation`.
		//
		// So it is admitted only when it can be shown to belong here: a genuine verdict is written AFTER this
		// incident executed, so anything stamped before this incident's own latest pre-verdict row is provably
		// somebody else's. The anchor walks down through per-incident (external_ref-keyed) rows: the ref-scoped
		// gate chain, then the committed prediction, then triage. With no anchor at all the verdict is NOT
		// admitted — an unprovable proof surface is exactly the defect being fixed, and an absent verify step
		// is visibly absent while a foreign one is indistinguishable from truth.
		//
		// This narrows honesty, not information: the per-incident execution evidence is untouched, and
		// deriveStatus reads "executed" off this incident's own execute gate (see core/trace/assemble.go).
		//
		// ★ FIRST, THOUGH, ASK THE TABLE THAT ACTUALLY KNOWS. action_execution (migration 0043) records one row
		// per EXECUTION carrying external_ref — it exists precisely because action_verdict collapses shapes,
		// and until now NOTHING read it per-occurrence (only an EXISTS-on-action_id in action_manifest_read).
		// When this incident has such a row it IS the answer, exactly and without inference, and the anchor
		// heuristic below is not consulted at all. A NULL verdict there means executed-but-unverifiable
		// (TG-182 fail-closed: the post-state could not be read) — recorded honestly rather than as a clean
		// result, so it must not be silently dropped into the "no verdict" bucket.
		var verdict string
		var verdictAt time.Time
		var unverifiable bool
		err = s.p.QueryRow(ctx, `
			SELECT COALESCE(verdict::text, ''), unverifiable, executed_at
			FROM action_execution
			WHERE action_id = $1 AND external_ref = $2
			ORDER BY executed_at DESC LIMIT 1`, actionID, externalRef).Scan(&verdict, &unverifiable, &verdictAt)
		haveOwn := false
		switch {
		case err == nil:
			haveOwn = true
			rec.Verdict.Present = true
			rec.Verdict.CreatedAt = verdictAt
			if unverifiable || verdict == "" {
				rec.Verdict.Verdict = "unverifiable"
			} else {
				rec.Verdict.Verdict = verdict
			}
		case errors.Is(err, pgx.ErrNoRows):
			// no per-execution row (this incident did not execute, or it predates migration 0043) — fall
			// through to the shape-wide ledger under the provenance check below
		default:
			return rec, fmt.Errorf("db: trace execution read: %w", err)
		}

		// FALLBACK for pre-0043 history only. Same read as before, still guarded, because the rows it returns
		// are shared across incidents. Deliberately NOT an early return above: a later reader appended to this
		// block must still run.
		err = pgx.ErrNoRows
		if !haveOwn {
			err = s.p.QueryRow(ctx, `
				SELECT verdict::text, created_at FROM action_verdict WHERE action_id = $1 LIMIT 1`, actionID).Scan(&verdict, &verdictAt)
		}
		switch {
		case err == nil:
			if anchor := incidentAnchor(rec); !anchor.IsZero() && !verdictAt.Before(anchor) {
				rec.Verdict.Present = true
				rec.Verdict.Verdict = verdict
				rec.Verdict.CreatedAt = verdictAt
			}
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return rec, fmt.Errorf("db: trace verify read: %w", err)
		}
	}

	return rec, nil
}

// incidentAnchor returns when THIS incident began — the earliest stamp among rows keyed to it by external_ref.
// A verdict written before the incident existed provably belongs to some earlier incident, which is the test
// the caller applies, given that action_verdict itself carries no occurrence key.
//
// ★ IT IS THE INCIDENT'S START, NOT ITS EXECUTION, AND THAT IS DELIBERATE. The first version of this anchored
// on the LATEST gate-chain row, reasoning that a genuine verdict is written after the action executes. That
// reasoning was wrong about its own premise: the chain does not end at `execute`, it ends at `verify`, which
// the interceptor emits AFTER the verifier has already written action_verdict (live 2026-07-29 on session
// librenms-dc1-181284: execute at 00:19:30, verify at 00:19:31). Anchoring on the maximum therefore
// rejected genuine verdicts as foreign. TestTraceSpineRoundTrip and TestTraceCommitPlanOpsAndVerifyTs both
// went red and are the reason this is right — they only run with TG_TEST_POSTGRES_DSN set, so a version of
// this change developed without a live database would have shipped the false negative silently.
//
// The incident's start cannot have that failure: every row of this walk is at or after it, so no true verdict
// is ever rejected. It gives up the theoretical case of a foreign verdict written DURING this incident's own
// lifetime — which needs two incidents of the same action shape overlapping within minutes, with the other one
// being the first ever to claim the first-wins row. All 22 live cases were days old and are caught. Trading an
// impossible false negative for an improbable false positive is the correct direction for a proof surface; the
// case that remains is closed by a per-occurrence identity, not by a cleverer timestamp.
func incidentAnchor(rec trace.SpineRecords) time.Time {
	var at time.Time
	for _, t := range []time.Time{
		rec.Ingest.ReceivedAt,
		rec.Correlate.DecidedAt,
		rec.Classification.CreatedAt,
		rec.Triage.CreatedAt,
		rec.Prediction.CommittedAt,
	} {
		if !t.IsZero() && (at.IsZero() || t.Before(at)) {
			at = t
		}
	}
	return at
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// decodeSignals decodes session_risk_audit.signals_json (migration 0003) into the non-secret string map the
// screen step projects. It is TOLERANT of a non-string value: unmarshaling straight into map[string]string
// FAILS the whole object if any value is a number/bool/object (e.g. {"blast_radius": 3}), silently dropping
// the entire screen. Decoding per-key into json.RawMessage and stringifying each value degrades one signal at
// most. A JSON string is unquoted; any other scalar/shape keeps its compact JSON text. Returns nil on a wholly
// unparseable blob (the screen step is then omitted, never fabricated).
func decodeSignals(signalsJSON []byte) map[string]string {
	if len(signalsJSON) == 0 {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(signalsJSON, &raw); err != nil || len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		s := strings.TrimSpace(string(v))
		if s == "" || s == "null" {
			out[k] = ""
			continue
		}
		if s[0] == '"' {
			var str string
			if json.Unmarshal(v, &str) == nil {
				out[k] = str
				continue
			}
		}
		out[k] = s // number / bool / array / object — keep the compact JSON text
	}
	return out
}

// attributionRefs projects session_triage.actor_evidence into the non-secret "<domain>:<ref>" pointers the
// attribute trace step carries (spec/023 REQ-2311, INV-13).
//
// It deliberately takes ONLY domain and ref. The stored record also holds the actor principal, and an actor
// identity on a rendered trace step is exactly the kind of value that reaches a screenshot, a paste, and a
// ticket. The taxonomy already says what the attribution CONCLUDED; a reference says where to check it.
//
// Unparseable JSON yields no references rather than an error: the attribute step's subject is the taxonomy,
// which scans from its own column, and a malformed evidence blob must not remove the boundary from the walk.
func attributionRefs(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var rows []struct {
		Domain string `json:"domain"`
		Ref    string `json:"ref"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		d, ref := strings.TrimSpace(r.Domain), strings.TrimSpace(r.Ref)
		if d == "" || ref == "" {
			continue
		}
		out = append(out, d+":"+ref)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
