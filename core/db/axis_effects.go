package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/manifest"
)

type ExecutedSessionAction struct {
	ExecutionID int64
	ExternalRef string
	SessionHost string
	Action      manifest.Action
	At          time.Time
}

func (s *AxisReadStore) ExecutedSessionActions(ctx context.Context, since, until time.Time) ([]ExecutedSessionAction, error) {
	rows, err := s.p.Pool.Query(ctx, `
		SELECT e.id, e.external_ref, t.host, e.target_host, m.action, e.executed_at
		  FROM action_execution e
		  JOIN session_triage t ON t.external_ref = e.external_ref
		  LEFT JOIN action_manifest m ON m.action_id = e.action_id
		 WHERE e.executed_at >= $1 AND e.executed_at < $2
		 ORDER BY e.id`, since, until)
	if err != nil {
		return nil, fmt.Errorf("db: executed session actions: %w", err)
	}
	defer rows.Close()
	var out []ExecutedSessionAction
	for rows.Next() {
		var e ExecutedSessionAction
		var target string
		var raw []byte
		if err := rows.Scan(&e.ExecutionID, &e.ExternalRef, &e.SessionHost, &target, &raw, &e.At); err != nil {
			return nil, fmt.Errorf("db: executed session actions: %w", err)
		}
		e.Action = manifest.Action{Target: target}
		if len(raw) > 0 {
			var a manifest.Action
			if err := json.Unmarshal(raw, &a); err == nil {
				e.Action = a
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *AxisReadStore) RefsPresent(ctx context.Context, refs []string, since, until time.Time) ([]string, error) {
	rows, err := s.p.Pool.Query(ctx, `
		SELECT DISTINCT external_ref FROM session_triage
		 WHERE external_ref = ANY($1::text[]) AND created_at >= $2 AND created_at < $3`, nonNilRefs(refs), since, until)
	if err != nil {
		return nil, fmt.Errorf("db: refs present: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("db: refs present: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
