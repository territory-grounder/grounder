package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	var skillLoadsJSON []byte
	err = s.p.QueryRow(ctx, `
		SELECT host, alert_rule, band, outcome, proposed, op, conclusion, confidence, created_at, COALESCE(skill_loads, '[]'::jsonb),
		       COALESCE(prompt_version, ''), COALESCE(seed_hash, ''), COALESCE(model_tier, '')
		FROM session_triage
		WHERE external_ref = $1`, externalRef).Scan(
		&rec.Triage.Host, &rec.Triage.AlertRule, &rec.Triage.Band, &rec.Triage.Outcome,
		&rec.Triage.Proposed, &rec.Triage.Op, &rec.Triage.Conclusion, &rec.Triage.Confidence,
		&rec.Triage.CreatedAt, &skillLoadsJSON,
		&rec.Triage.PromptVersion, &rec.Triage.SeedHash, &rec.Triage.ModelTier)
	switch {
	case err == nil:
		rec.Triage.Present = true
		if len(skillLoadsJSON) > 0 {
			_ = json.Unmarshal(skillLoadsJSON, &rec.Triage.SkillLoads) // best-effort; rag step omitted if unparseable
		}
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

	// verify — the mechanical verdict keyed by the content-hashed action id.
	actionID := rec.Classification.ActionID
	if actionID == "" {
		actionID = rec.Prediction.ActionID
	}
	if actionID != "" {
		var verdict string
		err = s.p.QueryRow(ctx, `
			SELECT verdict::text, created_at FROM action_verdict WHERE action_id = $1 LIMIT 1`, actionID).Scan(&verdict, &rec.Verdict.CreatedAt)
		switch {
		case err == nil:
			rec.Verdict.Present = true
			rec.Verdict.Verdict = verdict
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return rec, fmt.Errorf("db: trace verify read: %w", err)
		}
	}

	// agent cycles — the scrubbed ReAct investigation cycles (agent_step, T-020-8), cycle-ordered. One step per
	// captured cycle; a session with none contributes none (never a fabricated cycle).
	cyRows, err := s.p.Query(ctx, `
		SELECT cycle, thought, tool, observation, outcome, created_at
		FROM agent_step
		WHERE external_ref = $1
		ORDER BY cycle ASC`, externalRef)
	if err != nil {
		return rec, fmt.Errorf("db: trace agent-cycle read: %w", err)
	}
	for cyRows.Next() {
		var a trace.AgentCycleRecord
		if err := cyRows.Scan(&a.Cycle, &a.Thought, &a.Tool, &a.Observation, &a.Outcome, &a.CreatedAt); err != nil {
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

		// policy authorization — the composed Decide detail (policy_decision, T-020-3): verdict, in-force bundle
		// version, the FULL matched-rule list (jsonb {id,verdict}, projected non-secret), packet-tracer reason, mode.
		var (
			pVerdict, pBundle, pReason, pMode string
			matchedJSON                       []byte
		)
		err = s.p.QueryRow(ctx, `
			SELECT verdict, bundle_version, matched_rules, reason, mode, created_at, COALESCE(min_confidence, 0)
			FROM policy_decision
			WHERE action_id = $1
			ORDER BY created_at DESC
			LIMIT 1`, actionID).Scan(&pVerdict, &pBundle, &matchedJSON, &pReason, &pMode, &rec.Policy.CreatedAt, &rec.Policy.MinConfidence)
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
		// (interceptor_gate_verdict, T-020-7), ordinal-ascending.
		gRows, err := s.p.Query(ctx, `
			SELECT ordinal, gate, verdict, reason, created_at
			FROM interceptor_gate_verdict
			WHERE action_id = $1
			ORDER BY ordinal ASC`, actionID)
		if err != nil {
			return rec, fmt.Errorf("db: trace gate-verdict read: %w", err)
		}
		for gRows.Next() {
			var g trace.GateVerdictRecord
			if err := gRows.Scan(&g.Ordinal, &g.Gate, &g.Verdict, &g.Reason, &g.CreatedAt); err != nil {
				gRows.Close()
				return rec, fmt.Errorf("db: trace gate-verdict scan: %w", err)
			}
			rec.GateVerdicts = append(rec.GateVerdicts, g)
		}
		gRows.Close()
		if err := gRows.Err(); err != nil {
			return rec, fmt.Errorf("db: trace gate-verdict rows: %w", err)
		}
	}

	return rec, nil
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
