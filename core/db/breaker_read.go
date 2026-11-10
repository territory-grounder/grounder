package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/breaker"
)

// Load reads the current durable record for a named breaker across the process boundary. ok=false (with a nil
// error) means the breaker has never been saved — the breaker treats that as a fresh CLOSED breaker (allow),
// which for the SAFETY mutation breaker is the correct never-yet-tripped default. A query error is returned so
// the caller fails CLOSED: MutationBreaker.State reports an unreadable breaker as OPEN (a safety breaker we
// cannot observe is treated as tripped), so a store error can never let a sibling worker actuate.
func (s *BreakerStore) Load(ctx context.Context, name string) (breaker.Record, bool, error) {
	var (
		rec        breaker.Record
		state      string
		opened     *time.Time
		transition *time.Time
		att        breakerAttribution
	)
	err := s.p.Pool.QueryRow(ctx, `
		SELECT name, state, failure_count, opened_at, half_open_successes, last_transition_at, last_updated_at,
		       tripped_by_action_id, trip_cause, compensation_action_id, compensation_admitted_at
		FROM mutation_breaker_state
		WHERE name = $1`, name).
		Scan(&rec.Name, &state, &rec.FailureCount, &opened, &rec.HalfOpenSuccesses, &transition, &rec.LastUpdatedAt,
			&att.trippedBy, &att.cause, &att.compensation, &att.compensationAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return breaker.Record{}, false, nil
	}
	if err != nil {
		return breaker.Record{}, false, fmt.Errorf("db: mutation_breaker_state load (%q): %w", name, err)
	}
	rec.State = breaker.State(state)
	if opened != nil {
		rec.OpenedAt = *opened
	}
	if transition != nil {
		rec.LastTransitionAt = *transition
	}
	att.apply(&rec)
	return rec, true, nil
}

// breakerAttribution scans the nullable TG-665 attribution columns (NULL on closed / pre-0124 rows).
type breakerAttribution struct {
	trippedBy, cause, compensation *string
	compensationAt                 *time.Time
}

func (a breakerAttribution) apply(rec *breaker.Record) {
	if a.trippedBy != nil {
		rec.TrippedBy = *a.trippedBy
	}
	if a.cause != nil {
		rec.Cause = *a.cause
	}
	if a.compensation != nil {
		rec.CompensationActionID = *a.compensation
	}
	if a.compensationAt != nil {
		rec.CompensationAt = *a.compensationAt
	}
}

// List returns every durable breaker record ordered by name (for the metrics exporter — one gauge per name).
func (s *BreakerStore) List(ctx context.Context) ([]breaker.Record, error) {
	rows, err := s.p.Pool.Query(ctx, `
		SELECT name, state, failure_count, opened_at, half_open_successes, last_transition_at, last_updated_at,
		       tripped_by_action_id, trip_cause, compensation_action_id, compensation_admitted_at
		FROM mutation_breaker_state
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("db: mutation_breaker_state list: %w", err)
	}
	defer rows.Close()
	var out []breaker.Record
	for rows.Next() {
		var (
			rec        breaker.Record
			state      string
			opened     *time.Time
			transition *time.Time
			att        breakerAttribution
		)
		if err := rows.Scan(&rec.Name, &state, &rec.FailureCount, &opened, &rec.HalfOpenSuccesses, &transition, &rec.LastUpdatedAt,
			&att.trippedBy, &att.cause, &att.compensation, &att.compensationAt); err != nil {
			return nil, fmt.Errorf("db: mutation_breaker_state scan: %w", err)
		}
		rec.State = breaker.State(state)
		if opened != nil {
			rec.OpenedAt = *opened
		}
		if transition != nil {
			rec.LastTransitionAt = *transition
		}
		att.apply(&rec)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: mutation_breaker_state rows: %w", err)
	}
	return out, nil
}
