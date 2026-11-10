package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ActuationStateReadStore records each SSH session the actuation plane opens with TG's actuation key to read a
// target's state before or instead of an effect (TG-709): the independent check's `systemctl is-active` and the
// rollback necessity probe. The SSH-log reader explains a login under TG's key one-to-one by an SSH-lane execution
// or by one of these rows, so a state read no longer reads as TG's key used with nothing behind it. Append-only and
// written by the actuation plane alone (plane: actuation), so the triage plane cannot forge an explanation.
type ActuationStateReadStore struct{ p *Pool }

// NewActuationStateReadStore binds the store to a pool.
func NewActuationStateReadStore(p *Pool) *ActuationStateReadStore {
	return &ActuationStateReadStore{p: p}
}

// Record appends one state read on host for purpose, stamped by the database clock.
func (s *ActuationStateReadStore) Record(ctx context.Context, host, purpose string) error {
	host, purpose = strings.TrimSpace(host), strings.TrimSpace(purpose)
	if host == "" || purpose == "" {
		return fmt.Errorf("db: a state read needs a host and a purpose")
	}
	if _, err := s.p.Exec(ctx, `INSERT INTO actuation_state_read (host, purpose) VALUES ($1, $2)`, host, purpose); err != nil {
		return fmt.Errorf("db: record state read on %s: %w", host, err)
	}
	return nil
}

// TimesOn returns when state reads on host were recorded in [from, to], oldest first.
func (s *ActuationStateReadStore) TimesOn(ctx context.Context, host string, from, to time.Time) ([]time.Time, error) {
	rows, err := s.p.Query(ctx, `
		SELECT read_at FROM actuation_state_read
		WHERE lower(host) = lower($1) AND read_at >= $2 AND read_at <= $3
		ORDER BY read_at`, strings.TrimSpace(host), from, to)
	if err != nil {
		return nil, fmt.Errorf("db: state reads on %s: %w", host, err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
