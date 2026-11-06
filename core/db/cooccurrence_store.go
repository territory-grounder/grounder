package db

import (
	"context"
	"fmt"

	"github.com/territory-grounder/grounder/core/learn"
)

// CoOccurrenceStore is the pgx-backed durable snapshot of the co-occurrence learner (migration 0077, TG-388
// face c). It exists so the self-learning tier survives a worker restart instead of re-learning from zero
// (measured 1,524 learned edges -> 0 on a redeploy). MUTABLE competence-plane cache, not the append-only
// spine: Save REPLACES the whole snapshot atomically, because the learner decays pairs OUT and a stale row
// must not linger to be reloaded.
type CoOccurrenceStore struct{ p *Pool }

// NewCoOccurrenceStore returns a Postgres-backed learner-snapshot store.
func NewCoOccurrenceStore(p *Pool) *CoOccurrenceStore { return &CoOccurrenceStore{p: p} }

// Load reads the persisted learner snapshot; an empty database yields an empty snapshot (a first boot).
func (s *CoOccurrenceStore) Load(ctx context.Context) (learn.Snapshot, error) {
	var snap learn.Snapshot
	prows, err := s.p.Pool.Query(ctx, `SELECT primary_host, dependent_host, count, delay_sum FROM co_occurrence`)
	if err != nil {
		return snap, fmt.Errorf("db: load co_occurrence: %w", err)
	}
	defer prows.Close()
	for prows.Next() {
		var po learn.PairObservation
		if err := prows.Scan(&po.Primary, &po.Dependent, &po.Count, &po.DelaySum); err != nil {
			return snap, fmt.Errorf("db: scan co_occurrence: %w", err)
		}
		snap.Pairs = append(snap.Pairs, po)
	}
	if err := prows.Err(); err != nil {
		return snap, fmt.Errorf("db: co_occurrence rows: %w", err)
	}

	hrows, err := s.p.Pool.Query(ctx, `SELECT host, trials, recovery_sum, recovery_count FROM co_occurrence_host`)
	if err != nil {
		return snap, fmt.Errorf("db: load co_occurrence_host: %w", err)
	}
	defer hrows.Close()
	for hrows.Next() {
		var ht learn.HostTrials
		var recSum, recCount float64
		if err := hrows.Scan(&ht.Host, &ht.Trials, &recSum, &recCount); err != nil {
			return snap, fmt.Errorf("db: scan co_occurrence_host: %w", err)
		}
		snap.Trials = append(snap.Trials, ht)
		if recCount > 0 {
			// 0/0 is "no recovery evidence" (a pre-TG-188 row or a host that never cleared) — restoring it as
			// an entry would fabricate a zero-second MTTR observation.
			snap.Recoveries = append(snap.Recoveries, learn.HostRecovery{Host: ht.Host, Sum: recSum, Count: recCount})
		}
	}
	if err := hrows.Err(); err != nil {
		return snap, fmt.Errorf("db: co_occurrence_host rows: %w", err)
	}
	return snap, nil
}

// Save atomically REPLACES the persisted snapshot with the current learner state — DELETE-all + bulk INSERT in
// one transaction, so a concurrent Load never sees a half-written tier and a pair the learner decayed out is
// really gone. Empty maps clear the tables (the tier faded to nothing) — a faithful save, not a bug.
func (s *CoOccurrenceStore) Save(ctx context.Context, snap learn.Snapshot) error {
	tx, err := s.p.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: co_occurrence save begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after a successful Commit

	if _, err := tx.Exec(ctx, `DELETE FROM co_occurrence`); err != nil {
		return fmt.Errorf("db: co_occurrence clear: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM co_occurrence_host`); err != nil {
		return fmt.Errorf("db: co_occurrence_host clear: %w", err)
	}

	if n := len(snap.Pairs); n > 0 {
		prim, dep := make([]string, n), make([]string, n)
		cnt, dly := make([]float64, n), make([]float64, n)
		for i, p := range snap.Pairs {
			prim[i], dep[i], cnt[i], dly[i] = p.Primary, p.Dependent, p.Count, p.DelaySum
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO co_occurrence (primary_host, dependent_host, count, delay_sum)
			SELECT * FROM unnest($1::text[], $2::text[], $3::double precision[], $4::double precision[])`,
			prim, dep, cnt, dly); err != nil {
			return fmt.Errorf("db: co_occurrence insert: %w", err)
		}
	}
	// The host rows carry BOTH per-host tiers — trial denominators and recovery evidence (TG-188) — so the
	// insert set is the UNION of hosts appearing in either (a host's trials can decay out while its sparser
	// but younger recovery evidence survives, and vice versa).
	type hostRow struct{ trials, recSum, recCount float64 }
	rowsByHost := map[string]*hostRow{}
	for _, h := range snap.Trials {
		rowsByHost[h.Host] = &hostRow{trials: h.Trials}
	}
	for _, r := range snap.Recoveries {
		hr, ok := rowsByHost[r.Host]
		if !ok {
			hr = &hostRow{}
			rowsByHost[r.Host] = hr
		}
		hr.recSum, hr.recCount = r.Sum, r.Count
	}
	if n := len(rowsByHost); n > 0 {
		host := make([]string, 0, n)
		tr, rs, rc := make([]float64, 0, n), make([]float64, 0, n), make([]float64, 0, n)
		for h, r := range rowsByHost {
			host = append(host, h)
			tr = append(tr, r.trials)
			rs = append(rs, r.recSum)
			rc = append(rc, r.recCount)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO co_occurrence_host (host, trials, recovery_sum, recovery_count)
			SELECT * FROM unnest($1::text[], $2::double precision[], $3::double precision[], $4::double precision[])`,
			host, tr, rs, rc); err != nil {
			return fmt.Errorf("db: co_occurrence_host insert: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: co_occurrence save commit: %w", err)
	}
	return nil
}
