package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ObservationCoverage is one census snapshot (TG-180, migration 0106): how much of the live estate TG can
// see, measured at an instant. Unobservable is the "coverage of the unmeasured" DENOMINATOR, Confirmed the
// probe-verified NUMERATOR (0 until the probe is armed — and ProbeArmed says whether 0 means "nothing
// probed yet" or "the probe is off", two facts the scorecard must keep apart).
type ObservationCoverage struct {
	RecordedAt   time.Time
	Total        int
	Observed     int
	HealthyQuiet int
	Unobservable int
	Confirmed    int
	ProbeArmed   bool
}

// ObservationCoverageStore appends census snapshots (the worker) and reads the latest (the grounder's axis
// scorer). Append-only: a snapshot is a measurement, never edited.
type ObservationCoverageStore struct{ p *Pool }

func NewObservationCoverageStore(p *Pool) *ObservationCoverageStore { return &ObservationCoverageStore{p: p} }

// Record appends one snapshot. Negative counts are refused (a census cannot go negative; a caller that
// produces one is broken, and a broken snapshot must not silently become the scorecard's denominator).
func (s *ObservationCoverageStore) Record(ctx context.Context, c ObservationCoverage) error {
	if c.Total < 0 || c.Observed < 0 || c.HealthyQuiet < 0 || c.Unobservable < 0 || c.Confirmed < 0 {
		return fmt.Errorf("db: observation coverage snapshot with a negative count refused: %+v", c)
	}
	at := c.RecordedAt
	if at.IsZero() {
		at = time.Now()
	}
	_, err := s.p.Exec(ctx, `
		INSERT INTO observation_coverage (recorded_at, total, observed, healthy_quiet, unobservable, confirmed, probe_armed)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		at, c.Total, c.Observed, c.HealthyQuiet, c.Unobservable, c.Confirmed, c.ProbeArmed)
	if err != nil {
		return fmt.Errorf("db: record observation coverage: %w", err)
	}
	return nil
}

// Latest returns the most recent snapshot, or ok=false when none has ever been recorded — the honest
// "not measured" the scorecard renders as a named gap, never as 0/0.
func (s *ObservationCoverageStore) Latest(ctx context.Context) (ObservationCoverage, bool, error) {
	var c ObservationCoverage
	err := s.p.QueryRow(ctx, `
		SELECT recorded_at, total, observed, healthy_quiet, unobservable, confirmed, probe_armed
		FROM observation_coverage ORDER BY recorded_at DESC, id DESC LIMIT 1`).
		Scan(&c.RecordedAt, &c.Total, &c.Observed, &c.HealthyQuiet, &c.Unobservable, &c.Confirmed, &c.ProbeArmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ObservationCoverage{}, false, nil
	}
	if err != nil {
		return ObservationCoverage{}, false, fmt.Errorf("db: latest observation coverage: %w", err)
	}
	return c, true, nil
}
