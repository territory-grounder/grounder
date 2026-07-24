package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/httpapi"
)

// The skill-store read side (spec/014 REQ-1311/1313): the console's library, history, and trial views.
// Parameterized SQL only; every aggregate is computed by the database, never assembled from strings.

// ListSkills serves the library list: identity + production version + version count + active-trial flag.
func (s *SkillStore) ListSkills(ctx context.Context) ([]httpapi.SkillSummary, error) {
	rows, err := s.p.Query(ctx, `
		SELECT k.name, k.kind, k.pinned, k.position,
		       COALESCE(pv.version, ''), COALESCE(pv.source, ''),
		       (SELECT COUNT(*) FROM skill_version v WHERE v.skill_name = k.name),
		       EXISTS (SELECT 1 FROM skill_trial t WHERE t.skill_name = k.name AND t.status = 'active')
		FROM skill k
		LEFT JOIN skill_version pv ON pv.skill_name = k.name AND pv.status = 'production'
		ORDER BY k.position ASC, k.name ASC`)
	if err != nil {
		return nil, fmt.Errorf("db: list skills: %w", err)
	}
	defer rows.Close()
	var out []httpapi.SkillSummary
	for rows.Next() {
		var r httpapi.SkillSummary
		if err := rows.Scan(&r.Name, &r.Kind, &r.Pinned, &r.Position,
			&r.ProductionVersion, &r.ProductionSource, &r.VersionCount, &r.ActiveTrial); err != nil {
			return nil, fmt.Errorf("db: scan skill summary: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SkillDetail serves one skill's full version history (newest first), rationale logs and eval scores
// included. found=false for an unknown name.
func (s *SkillStore) SkillDetail(ctx context.Context, name string) (httpapi.SkillDetailView, bool, error) {
	var det httpapi.SkillDetailView
	err := s.p.QueryRow(ctx, `
		SELECT k.name, k.kind, k.pinned, k.position,
		       COALESCE(pv.version, ''), COALESCE(pv.source, ''),
		       (SELECT COUNT(*) FROM skill_version v WHERE v.skill_name = k.name),
		       EXISTS (SELECT 1 FROM skill_trial t WHERE t.skill_name = k.name AND t.status = 'active')
		FROM skill k
		LEFT JOIN skill_version pv ON pv.skill_name = k.name AND pv.status = 'production'
		WHERE k.name = $1`, name).
		Scan(&det.Name, &det.Kind, &det.Pinned, &det.Position,
			&det.ProductionVersion, &det.ProductionSource, &det.VersionCount, &det.ActiveTrial)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return httpapi.SkillDetailView{}, false, nil
		}
		return httpapi.SkillDetailView{}, false, fmt.Errorf("db: skill detail %s: %w", name, err)
	}

	rows, err := s.p.Query(ctx, `
		SELECT id, version, status, body, applies_when, content_hash, author, source, rationale,
		       COALESCE(eval_offline, 'null'), COALESCE(eval_online, 'null'), COALESCE(ledger_seq, 0),
		       created_at, status_changed_at
		FROM skill_version WHERE skill_name = $1 ORDER BY id DESC`, name)
	if err != nil {
		return httpapi.SkillDetailView{}, false, fmt.Errorf("db: skill versions %s: %w", name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var v httpapi.SkillVersionView
		var aw, evOff, evOn []byte
		var created, statusAt time.Time
		if err := rows.Scan(&v.ID, &v.Version, &v.Status, &v.Body, &aw, &v.ContentHash, &v.Author,
			&v.Source, &v.Rationale, &evOff, &evOn, &v.LedgerSeq, &created, &statusAt); err != nil {
			return httpapi.SkillDetailView{}, false, fmt.Errorf("db: scan version: %w", err)
		}
		v.AppliesWhen = json.RawMessage(aw)
		// SQL NULL is coalesced to the JSON literal 'null' at the query, then omitted here: an absent
		// eval is an ABSENT field in the view, never a fabricated null object.
		if string(evOff) != "null" {
			v.EvalOffline = json.RawMessage(evOff)
		}
		if string(evOn) != "null" {
			v.EvalOnline = json.RawMessage(evOn)
		}
		v.CreatedAt = created.UTC().Format(time.RFC3339)
		v.StatusAt = statusAt.UTC().Format(time.RFC3339)
		det.Versions = append(det.Versions, v)
	}
	return det, true, rows.Err()
}

// ListTrials serves every trial with its per-arm assignment counts.
func (s *SkillStore) ListTrials(ctx context.Context) ([]httpapi.TrialView, error) {
	rows, err := s.p.Query(ctx, `
		SELECT id, skill_name, dimension, status, candidate_ids, COALESCE(control_version_id, 0),
		       min_samples_per_arm, min_lift, p_threshold, ends_at, COALESCE(winner_version_id, 0),
		       note, created_at, finalized_at
		FROM skill_trial ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("db: list trials: %w", err)
	}
	defer rows.Close()
	var out []httpapi.TrialView
	for rows.Next() {
		var t httpapi.TrialView
		var ends, created time.Time
		var finalized *time.Time
		if err := rows.Scan(&t.ID, &t.SkillName, &t.Dimension, &t.Status, &t.CandidateIDs,
			&t.ControlVersion, &t.MinSamples, &t.MinLift, &t.PThreshold, &ends,
			&t.WinnerVersionID, &t.Note, &created, &finalized); err != nil {
			return nil, fmt.Errorf("db: scan trial: %w", err)
		}
		t.EndsAt = ends.UTC().Format(time.RFC3339)
		t.CreatedAt = created.UTC().Format(time.RFC3339)
		if finalized != nil {
			t.FinalizedAt = finalized.UTC().Format(time.RFC3339)
		}
		t.Assignments = map[string]int{}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Per-arm assignment counts in one aggregate pass (the dashboard's arm gauges).
	arms, err := s.p.Query(ctx, `
		SELECT trial_id, variant_idx, COUNT(*) FROM skill_trial_assignment GROUP BY trial_id, variant_idx`)
	if err != nil {
		return nil, fmt.Errorf("db: trial assignments: %w", err)
	}
	defer arms.Close()
	counts := map[int64]map[string]int{}
	for arms.Next() {
		var trial int64
		var variant, n int
		if err := arms.Scan(&trial, &variant, &n); err != nil {
			return nil, fmt.Errorf("db: scan assignment count: %w", err)
		}
		if counts[trial] == nil {
			counts[trial] = map[string]int{}
		}
		counts[trial][fmt.Sprintf("%d", variant)] = n
	}
	for i := range out {
		if c, ok := counts[out[i].ID]; ok {
			out[i].Assignments = c
		}
	}
	return out, arms.Err()
}
