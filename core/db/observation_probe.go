package db

import (
	"context"
	"fmt"
	"time"
)

// ObservationProbeStore reads and writes the observation_probe ledger (TG-180 part 2 — the census's null test).
// It is the durable memory of the fault-injection probe: which census-unobservable entities were probed, over
// what window, and with what verdict. The estate-mutating side of a probe is faultinjector's concern (it writes
// injected_fault with its own restore obligation); this store records the OBSERVATION only.
//
// Types are db-native (strings, a PendingProbe struct) rather than the tools/observeprobe domain types, so
// core/db keeps its no-tools-import direction; the worker adapts them to the orchestrator's interfaces.
type ObservationProbeStore struct{ p *Pool }

// NewObservationProbeStore wraps a connected pool.
func NewObservationProbeStore(p *Pool) *ObservationProbeStore { return &ObservationProbeStore{p: p} }

// PendingProbe is one probe awaiting a verdict — its window may or may not have closed yet.
type PendingProbe struct {
	ID         int64
	Host       string
	FaultClass string
	InjectedAt time.Time
	WindowEnd  time.Time
	Ran        bool
}

// RecordProbe appends a probe run and returns its id. The verdict starts 'pending'; ran records whether the
// fault actually committed (false ⇒ the caller decides it inconclusive at once, since nothing was perturbed).
// Bound parameters only, no interpolation.
func (s *ObservationProbeStore) RecordProbe(ctx context.Context, host, faultClass string, injectedAt, windowEnd time.Time, ran bool, note string) (int64, error) {
	var id int64
	err := s.p.QueryRow(ctx, `
		INSERT INTO observation_probe (host, fault_class, injected_at, window_end, ran, note)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id`, host, faultClass, injectedAt, windowEnd, ran, note).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: record observation probe: %w", err)
	}
	return id, nil
}

// PendingProbes returns every probe still awaiting a verdict, soonest window-close first. The caller decides
// each whose window has closed and leaves the rest for a later cycle (the cross-cycle discipline).
func (s *ObservationProbeStore) PendingProbes(ctx context.Context) ([]PendingProbe, error) {
	rows, err := s.p.Query(ctx, `
		SELECT id, host, fault_class, injected_at, window_end, ran
		  FROM observation_probe
		 WHERE verdict = 'pending'
		 ORDER BY window_end`)
	if err != nil {
		return nil, fmt.Errorf("db: pending observation probes: %w", err)
	}
	defer rows.Close()
	var out []PendingProbe
	for rows.Next() {
		var p PendingProbe
		if err := rows.Scan(&p.ID, &p.Host, &p.FaultClass, &p.InjectedAt, &p.WindowEnd, &p.Ran); err != nil {
			return nil, fmt.Errorf("db: scan pending observation probe: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetProbeVerdict writes a probe's terminal (or inconclusive) verdict, but ONLY while it is still pending — the
// WHERE clause makes the transition idempotent and single-writer, so two reconcilers cannot both decide the
// same row and the second's write is a harmless no-op rather than an overwrite. decided_at is set here so the
// migration's (verdict='pending') = (decided_at IS NULL) invariant always holds.
func (s *ObservationProbeStore) SetProbeVerdict(ctx context.Context, id int64, verdict string) error {
	_, err := s.p.Exec(ctx, `
		UPDATE observation_probe
		   SET verdict = $2, decided_at = now()
		 WHERE id = $1 AND verdict = 'pending'`, id, verdict)
	if err != nil {
		return fmt.Errorf("db: set observation-probe verdict id=%d: %w", id, err)
	}
	return nil
}

// ProbeConfirmedHosts is the set of hosts with a TERMINAL verdict (observable OR unobservable_confirmed) — the
// coverage numerator, and (with the pending hosts) the "already probed, do not re-probe" set. An inconclusive
// probe is deliberately EXCLUDED: it never ran, so its host is still an open question and remains re-probeable.
func (s *ObservationProbeStore) ProbeConfirmedHosts(ctx context.Context) (map[string]bool, error) {
	rows, err := s.p.Query(ctx, `
		SELECT DISTINCT host FROM observation_probe
		 WHERE verdict IN ('observable','unobservable_confirmed')`)
	if err != nil {
		return nil, fmt.Errorf("db: probe confirmed hosts: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("db: scan probe confirmed host: %w", err)
		}
		out[h] = true
	}
	return out, rows.Err()
}

// ProbeAlertTimes returns the received-at times of admitted alerts on a host within [since, until] — the
// front-door evidence (ingest_alert = what TG admitted) the probe verdict reads. Bound parameters only; the
// bounds are inclusive so an alert exactly at the injection instant or exactly at the window close counts.
func (s *ObservationProbeStore) ProbeAlertTimes(ctx context.Context, host string, since, until time.Time) ([]time.Time, error) {
	rows, err := s.p.Query(ctx, `
		SELECT received_at FROM ingest_alert
		 WHERE host = $1 AND received_at >= $2 AND received_at <= $3
		 ORDER BY received_at`, host, since, until)
	if err != nil {
		return nil, fmt.Errorf("db: probe alert times for %s: %w", host, err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("db: scan probe alert time: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
