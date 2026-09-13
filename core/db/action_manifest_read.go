package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/httpapi"
)

// ActionManifestReadStore serves the Actions surface: every sealed ActionManifest as a governed walk from
// classification to verdict.
//
// It exists because that surface rendered FIVE INVENTED INCIDENTS bound to REAL estate hostnames —
// "Repeated auth failures -> ASA dc1fw01", "NAS volume 88% -> cleanup dc2nas01" — while 109 genuine
// manifests sat in this table unread. A fabricated incident on a real firewall is not a placeholder; an
// operator who believes it goes and looks at a device that is fine.
//
// Every field below is a projection of a stored fact. Nothing is inferred, and the five stage flags are
// derived from the presence of the rows that actually record each stage, never from a status guess.
type ActionManifestReadStore struct{ p *Pool }

// NewActionManifestReadStore builds the read store.
func NewActionManifestReadStore(p *Pool) *ActionManifestReadStore {
	return &ActionManifestReadStore{p: p}
}

var _ httpapi.ActionManifestReader = (*ActionManifestReadStore)(nil)

// sealedAction is the shape of action_manifest.action (the sealed manifest, INV-07). Decoded into a typed
// struct with an explicit field set rather than passed through as raw jsonb: the console must never receive
// keys this projection did not deliberately publish.
type sealedAction struct {
	Op         string            `json:"op"`
	OpClass    string            `json:"op_class"`
	Target     string            `json:"target"`
	Reversible bool              `json:"reversible"`
	Params     map[string]string `json:"params"`
}

// Recent returns the newest sealed manifests, newest first.
//
// The five stage booleans are each grounded in a row that exists:
//   - classified — the manifest carries a band (NOT NULL in the table, so always true; published anyway so
//     the surface reads its value rather than assuming it)
//   - predicted  — a prediction was committed and bound before sealing (prediction_hash)
//   - approved   — a human recorded a choice (approval_choice)
//   - executed   — an action_execution row exists for this action_id
//   - verified   — the mechanical verifier wrote a verdict
//
// EXISTS is used for the execution probe rather than a JOIN so a manifest with several execution attempts
// yields ONE ribbon, not one per attempt. An action-identity collapse of exactly that kind has already
// turned 87 executions into 26 rows in this project's own reporting.
func (s *ActionManifestReadStore) Recent(ctx context.Context, _ auth.Principal, limit int) ([]httpapi.ActionRibbon, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// risk_level and confidence are LEFT joins, and their absence is carried as absence rather than as a
	// zero: t.confidence IS NULL means "none recorded", which the DTO reports via HasConfidence. A
	// COALESCE(...,0) here would render an unrecorded value as total no-confidence — a claim the data does
	// not make, and the same "absent is not zero" defect that once made a calibration score unreadable.
	//
	// Both are LATERAL/scalar subqueries rather than plain joins so a session with several risk-audit rows
	// cannot multiply one manifest into several ribbons (the identity-collapse hazard the EXISTS below
	// exists to avoid).
	rows, err := s.p.Query(ctx, `
		SELECT m.action_id, m.action, m.band::text,
		       -- ★ THE GRADUATION SURFACE UNDER-REPORTED DEVIATIONS 24-to-1.
		       -- m.verdict is a BACKFILLED column and the backfill is incomplete: measured live 2026-07-29,
		       -- 61 of 113 manifests had m.verdict NULL, 46 of those DID have a scored row in action_verdict,
		       -- and 23 OF THOSE HAD DEVIATED. So #actions rendered "1 deviation" while 24 actions had
		       -- actually deviated, and #grounding — reading action_verdict directly — said 24 on the same
		       -- console. #actions is the surface an operator reads to judge whether an op-class is safe to
		       -- graduate to unattended AUTO, and a deviation is precisely the clamping event that must
		       -- prevent it. Under-reporting it there is the dangerous direction.
		       -- Coalescing is sound rather than a guess: where BOTH values exist they disagree in 0 of 98
		       -- cases, so action_verdict is the same fact recorded earlier, not a competing one.
		       -- The scalar subquery (not a JOIN) keeps ONE ribbon per manifest even if action_verdict ever
		       -- gains per-occurrence rows — the identity-collapse hazard this file already guards against
		       -- twice. Verified today: action_verdict holds exactly 1 row per action_id, max 1, 0 duplicates.
		       -- THE DEEPER FIX IS THE BACKFILL ITSELF (cmd/worker ManifestBackfiller); this stops the READ
		       -- from publishing a number it can already see is incomplete.
		       COALESCE(NULLIF(m.verdict::text, ''),
		                (SELECT av.verdict::text FROM action_verdict av WHERE av.action_id = m.action_id),
		                ''),
		       COALESCE(m.plan_hash, ''), (m.prediction_hash IS NOT NULL),
		       COALESCE(m.approval_choice, ''),
		       EXISTS (SELECT 1 FROM action_execution e WHERE e.action_id = m.action_id),
		       -- TG-532: whose session each lifecycle label describes, and how many sessions share this
		       -- SHAPE. Scalar subquery (not a JOIN) for the same identity-collapse reason as above.
		       --
		       -- The population is the UNION of both records that bind a session to an action_id, because
		       -- either alone UNDER-counts and under-counting is the dangerous direction here: a ribbon
		       -- reporting "1" invites precisely the "this label must be mine" reading this column exists
		       -- to prevent. Measured live 2026-08-22 over all 139 sealed manifests: session_risk_audit saw
		       -- MORE sessions than infragraph_prediction on 21 shapes, and prediction saw more on ZERO —
		       -- a manifest can be sealed and classified without a committed prediction (prediction_hash is
		       -- nullable), so the prediction bridge alone misses those sessions entirely.
		       m.approval_ref, m.verdict_ref,
		       (SELECT count(*) FROM (
		            SELECT ip.external_ref FROM infragraph_prediction ip
		             WHERE ip.action_id = m.action_id AND ip.kind = 'action' AND ip.external_ref <> ''
		            UNION
		            SELECT ra.external_ref FROM session_risk_audit ra
		             WHERE ra.action_id = m.action_id AND ra.external_ref <> ''
		          ) shared),
		       m.sealed_at,
		       (SELECT r.risk_level FROM session_risk_audit r
		         WHERE r.action_id = m.action_id ORDER BY r.created_at DESC LIMIT 1),
		       (SELECT t.confidence FROM session_risk_audit r
		          JOIN session_triage t ON t.external_ref = r.external_ref
		         WHERE r.action_id = m.action_id ORDER BY r.created_at DESC LIMIT 1)
		FROM action_manifest m
		ORDER BY m.sealed_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("db: action manifests: %w", err)
	}
	defer rows.Close()

	out := make([]httpapi.ActionRibbon, 0, limit)
	for rows.Next() {
		var (
			r        httpapi.ActionRibbon
			raw      []byte
			approval string
			risk     *string
			conf     *float64
		)
		if err := rows.Scan(&r.ActionID, &raw, &r.Band, &r.Verdict, &r.PlanHash,
			&r.Predicted, &approval, &r.Executed,
			&r.ApprovalRef, &r.VerdictRef, &r.SessionsSharing,
			&r.SealedAt, &risk, &conf); err != nil {
			return nil, err
		}
		if risk != nil {
			r.RiskLevel = *risk
		}
		if conf != nil {
			r.Confidence, r.HasConfidence = *conf, true
		}
		var a sealedAction
		if err := json.Unmarshal(raw, &a); err != nil {
			// A manifest we cannot decode is not rendered as a partial row: the surface would show a
			// governed action with no target, which reads as "TG acted on nothing".
			return nil, fmt.Errorf("db: action manifest %s: decode sealed action: %w", r.ActionID, err)
		}
		r.Op, r.OpClass, r.Target, r.Reversible = a.Op, a.OpClass, a.Target, a.Reversible
		r.Params = a.Params
		r.ApprovalChoice = approval
		r.Classified = r.Band != ""
		r.Approved = approval != ""
		r.Verified = r.Verdict != ""
		out = append(out, r)
	}
	return out, rows.Err()
}

// Counts reports the population behind the page, so the surface never presents its page size as a count —
// the defect that pinned the alerts badge at its fetch limit of 50.
func (s *ActionManifestReadStore) Counts(ctx context.Context, _ auth.Principal) (httpapi.ActionCounts, error) {
	var c httpapi.ActionCounts
	// ★ THE SAME COALESCE AS Recent(), AND IT WAS MISSING HERE FOR THE SAME 24-TO-1 REASON.
	//
	// Recent() ninety lines above carries a long note about m.verdict being an INCOMPLETE backfill: 61 of
	// 113 manifests had it NULL, 46 of those had a scored row in action_verdict, and 23 of those had
	// DEVIATED. Recent() was fixed to coalesce. Counts() was not — and both feed the SAME HTTP response.
	//
	// So /v1/actions returned a headline badge reading "1 deviation" directly above a ribbon list showing
	// twenty-four of them, and an operator judging whether an op-class is safe to graduate to unattended
	// AUTO read the badge. Under-reporting a deviation on the graduation surface is the dangerous
	// direction: a deviation is precisely the clamping event that must PREVENT the climb.
	//
	// Sound rather than a guess, on the same evidence Recent() cites: where both values exist they
	// disagree in 0 of 98 cases, so action_verdict is the same fact recorded earlier, not a competing one.
	// The scalar subquery (not a JOIN) keeps the count one-per-manifest.
	//
	// NULLIF('' ) matters as much as the COALESCE: the column is a text-backed enum whose backfill wrote
	// EMPTY STRINGS as well as NULLs, and `verdict IS NOT NULL` counted an empty string as verified.
	err := s.p.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE COALESCE(NULLIF(m.verdict::text, ''),
		                (SELECT av.verdict::text FROM action_verdict av WHERE av.action_id = m.action_id),
		                '') <> ''),
		       count(*) FILTER (WHERE COALESCE(NULLIF(m.verdict::text, ''),
		                (SELECT av.verdict::text FROM action_verdict av WHERE av.action_id = m.action_id),
		                '') = 'deviation')
		FROM action_manifest m`).Scan(&c.Total, &c.Verified, &c.Deviations)
	if err != nil {
		return httpapi.ActionCounts{}, fmt.Errorf("db: action manifest counts: %w", err)
	}
	return c, nil
}
