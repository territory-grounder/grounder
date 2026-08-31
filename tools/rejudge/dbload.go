package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/eval"
)

// DB-REPLAY MODE (TG-527 slice A2). Until this, rejudge replayed only the harness's captured
// sessions.runN.json files — no code anywhere constructed an eval.Session from the durable session_triage
// row, so the trajectory column shipped in 0104 would have been write-only: persisted, never read. This
// loader is the read half: it turns a window of live triage rows into eval.Sessions (trajectory included),
// so the trajectory_grounded axis — and every other row-backed axis — can be scored over HISTORICAL
// sessions with the same Aggregate the harness uses.
//
// Honesty about the mapping: session_triage carries the terminal facts the asynchronous judge adjudicates,
// not the full harness capture. Fields the row has no column for (Severity, the per-step Decisions list)
// stay zero, and the axes that need them read N/A rather than being floored — the same "never invent a
// measurement" rule the Diagnosis and trajectory NULLs follow. A pre-0104 row (trajectory NULL) yields an
// empty Trajectory, which trajectoryScore already treats as N/A.

// loadSessionsFromDB reads session_triage rows created at or after `since`, newest first, capped at limit.
func loadSessionsFromDB(ctx context.Context, dsn string, since time.Time, limit int) ([]eval.Session, error) {
	if limit <= 0 {
		limit = 200
	}
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("rejudge db: %w", err)
	}
	defer pool.Close()
	rows, err := pool.Query(ctx, `
		SELECT external_ref, host, alert_rule, band, outcome, proposed, evidence_ids, conclusion,
		       prediction, predicted, mutated, trajectory
		FROM session_triage
		WHERE created_at >= $1
		ORDER BY created_at DESC
		LIMIT $2`, since, limit)
	if err != nil {
		return nil, fmt.Errorf("rejudge db: query session_triage: %w", err)
	}
	defer rows.Close()
	var out []eval.Session
	for rows.Next() {
		var s eval.Session
		var traj []byte
		if err := rows.Scan(&s.Ref, &s.Host, &s.AlertRule, &s.Band, &s.Outcome, &s.Proposed, &s.Evidence,
			&s.Conclusion, &s.Prediction, &s.Predicted, &s.Mutated, &traj); err != nil {
			return nil, fmt.Errorf("rejudge db: scan %s: %w", s.Ref, err)
		}
		if len(traj) > 0 {
			// NULL scans as nil → stays empty → the axis reads N/A for a pre-0104 row. A present blob
			// (even '[]') is "recorded": decode the persisted twin into the loop's type for the scorer.
			var steps []struct {
				Tool    string `json:"tool"`
				ArgsKey string `json:"args_key"`
			}
			if err := json.Unmarshal(traj, &steps); err != nil {
				return nil, fmt.Errorf("rejudge db: trajectory for %s: %w", s.Ref, err)
			}
			s.Trajectory = make([]agent.TrajectoryStep, 0, len(steps))
			for _, st := range steps {
				s.Trajectory = append(s.Trajectory, agent.TrajectoryStep{Tool: st.Tool, ArgsKey: st.ArgsKey})
			}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
