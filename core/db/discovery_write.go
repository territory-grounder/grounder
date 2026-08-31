package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/falsify"
	"github.com/territory-grounder/grounder/core/safety"
)

// DiscoveryStore is the pgx-backed, DURABLE core/falsify.DiscoveryWriter (migration 0072, TG-206). The
// verify-time Scorer captures every scored deviation — a committed prediction reality falsified — keyed by
// the deviation SIGNATURE (target, site, sorted surprise-hosts), counting reproductions across incidents as
// the promotion-gating "reproduces >= N" signal. MemDiscoveryCorpus held that only in the worker's memory, so
// a restart dropped the whole corpus and reset the reproduction counts to zero. This persists each capture to
// discovery_deviation so the signal survives a restart and every sibling worker adds to the same corpus. It
// is the same seam MemDiscoveryCorpus satisfies (the in-memory oracle twin); Capture keeps the identical
// first-wins-per-signature, reproduction-increments semantics.
//
// Parameters are always bound ($1) — no string-built SQL (INV-03). NON-SECRET by construction: only host /
// rule / site slugs and hashes ever cross over, exactly as the DiscoveryRecord guarantees.
type DiscoveryStore struct{ p *Pool }

// NewDiscoveryStore returns the Postgres-backed durable discovery corpus.
func NewDiscoveryStore(p *Pool) *DiscoveryStore { return &DiscoveryStore{p: p} }

// compile-time proof the durable store satisfies the seam the Scorer captures through.
var _ falsify.DiscoveryWriter = (*DiscoveryStore)(nil)

// Capture persists a scored deviation, keyed by its signature. A FIRST sighting inserts (reproductions=1);
// a reproduction of an existing signature increments the count and advances last_seen. Returns whether the
// record was newly captured (true) vs a reproduction (false) — the same contract as MemDiscoveryCorpus,
// detected with `RETURNING (xmax = 0)` (true only for a freshly-inserted tuple). A capture error is returned
// (never fatal to the caller): the Scorer counts it exactly like a verdict-write blip.
func (s *DiscoveryStore) Capture(ctx context.Context, rec falsify.DiscoveryRecord) (bool, error) {
	sh, err := json.Marshal(rec.SurpriseHosts)
	if err != nil {
		return false, fmt.Errorf("db: discovery capture marshal surprise_hosts: %w", err)
	}
	mm, err := json.Marshal(rec.Mismatches)
	if err != nil {
		return false, fmt.Errorf("db: discovery capture marshal mismatches: %w", err)
	}
	ob, err := json.Marshal(rec.Observed)
	if err != nil {
		return false, fmt.Errorf("db: discovery capture marshal observed: %w", err)
	}
	sc, err := json.Marshal(rec.Score)
	if err != nil {
		return false, fmt.Errorf("db: discovery capture marshal score: %w", err)
	}
	var inserted bool
	err = s.p.Pool.QueryRow(ctx, `
		INSERT INTO discovery_deviation
		  (deviation_key, action_id, plan_hash, prediction_hash, target_host, site, verdict,
		   surprise_hosts, mismatches, observed, score, committed_at, observed_at,
		   reproductions, first_seen, last_seen)
		VALUES ($1,$2,$3,$4,$5,$6,$7, $8::jsonb,$9::jsonb,$10::jsonb,$11::jsonb, $12,$13, 1, $13,$13)
		ON CONFLICT (deviation_key) DO UPDATE
		  SET reproductions = discovery_deviation.reproductions + 1,
		      last_seen = GREATEST(discovery_deviation.last_seen, EXCLUDED.last_seen)
		RETURNING (xmax = 0)`,
		rec.DeviationKey(), rec.ActionID, rec.PlanHash, rec.PredictionHash, rec.TargetHost, rec.Site, string(rec.Verdict),
		string(sh), string(mm), string(ob), string(sc), rec.CommittedAt, rec.ObservedAt,
	).Scan(&inserted)
	if err != nil {
		return false, fmt.Errorf("db: discovery_deviation capture: %w", err)
	}
	return inserted, nil
}

// Deviations reads the durable corpus back — every signature reproduced at least minReproductions times,
// most-reproduced first — the promotion candidates the eval side drains into the sealed regression suite.
// A minReproductions <= 0 returns everything.
func (s *DiscoveryStore) Deviations(ctx context.Context, minReproductions int) ([]falsify.CapturedDeviation, error) {
	if minReproductions < 1 {
		minReproductions = 1
	}
	rows, err := s.p.Pool.Query(ctx, `
		SELECT action_id, plan_hash, prediction_hash, target_host, site, verdict,
		       surprise_hosts, mismatches, observed, score, committed_at, observed_at,
		       reproductions, first_seen, last_seen
		FROM discovery_deviation
		WHERE reproductions >= $1
		ORDER BY reproductions DESC, last_seen DESC`, minReproductions)
	if err != nil {
		return nil, fmt.Errorf("db: discovery_deviation read: %w", err)
	}
	defer rows.Close()
	var out []falsify.CapturedDeviation
	for rows.Next() {
		var (
			rec                     falsify.DiscoveryRecord
			verdict                 string
			sh, mm, ob, sc          []byte
			repro                   int
			firstSeen, lastSeen     time.Time
			committedAt, observedAt time.Time
		)
		if err := rows.Scan(&rec.ActionID, &rec.PlanHash, &rec.PredictionHash, &rec.TargetHost, &rec.Site, &verdict,
			&sh, &mm, &ob, &sc, &committedAt, &observedAt, &repro, &firstSeen, &lastSeen); err != nil {
			return nil, fmt.Errorf("db: discovery_deviation scan: %w", err)
		}
		rec.Verdict = safety.Verdict(verdict)
		rec.CommittedAt, rec.ObservedAt = committedAt, observedAt
		_ = json.Unmarshal(sh, &rec.SurpriseHosts)
		_ = json.Unmarshal(mm, &rec.Mismatches)
		_ = json.Unmarshal(ob, &rec.Observed)
		_ = json.Unmarshal(sc, &rec.Score)
		out = append(out, falsify.CapturedDeviation{Record: rec, Reproductions: repro, FirstSeen: firstSeen, LastSeen: lastSeen})
	}
	return out, rows.Err()
}
