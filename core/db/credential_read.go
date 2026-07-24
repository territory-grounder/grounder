package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CredentialReadStore is the pgx-backed READ side of the credential-engine console surface (spec/006
// REQ-526): three read-only projections over the worker-written credential state.
//   - Sources: the latest credential_sync_run per source (0017) LEFT JOINed to the current coverage count.
//   - Resolutions: the recent credential_resolution audit tail (0018), optionally filtered to one target.
//   - Coverage: outcome tallies per plane and per source over a recent window, plus each target's most
//     recent resolved/refused outcome — the coverage frontier DERIVED from real resolutions.
//
// Every query is parameterized ($1/$2) — no string-built SQL — and selects ONLY the non-secret columns the
// 0017/0018 tables hold (which are secret-free by construction, INV-13): counts, planes, outcomes, login
// users, connection schemes, and the SCHEME of a key reference. No column here can carry key material, a
// SecretRef value/path, or a token, so no read can leak one.
type CredentialReadStore struct{ p *Pool }

// NewCredentialReadStore returns the Postgres-backed credential read projections.
func NewCredentialReadStore(p *Pool) *CredentialReadStore { return &CredentialReadStore{p: p} }

// credentialCoverageWindowDays bounds the coverage aggregation to a recent window (real, not all-time).
const credentialCoverageWindowDays = 30

// credentialCoverageFrontierCap bounds each of the recent-resolved / recent-refused target lists.
const credentialCoverageFrontierCap = 100

// CredentialSourceRow is one source's latest sync run plus its current coverage count (non-secret).
type CredentialSourceRow struct {
	SourceID       string
	Plane          string
	LastSyncedAt   *time.Time // last SUCCESSFUL sync; nil when never
	Added          int
	Changed        int
	Removed        int
	Outcome        string
	Err            string
	CoveredTargets int
}

// Sources returns the latest credential_sync_run per source_id, LEFT JOINed to the current coverage count,
// ordered plane then source_id. An empty projection yields an empty slice (never a fabricated row).
func (s *CredentialReadStore) Sources(ctx context.Context) ([]CredentialSourceRow, error) {
	rows, err := s.p.Pool.Query(ctx, `
		SELECT source_id, plane, last_synced_at, added, changed, removed, outcome, err, covered_targets
		FROM (
			SELECT DISTINCT ON (r.source_id)
				r.source_id, r.plane, r.last_synced_at, r.added, r.changed, r.removed, r.outcome, r.err,
				COALESCE(c.targets, 0) AS covered_targets
			FROM credential_sync_run r
			LEFT JOIN credential_coverage c ON c.source_id = r.source_id
			ORDER BY r.source_id, r.started_at DESC, r.id DESC
		) t
		ORDER BY plane, source_id`)
	if err != nil {
		return nil, fmt.Errorf("db: credential sources: %w", err)
	}
	defer rows.Close()
	var out []CredentialSourceRow
	for rows.Next() {
		var r CredentialSourceRow
		if err := rows.Scan(&r.SourceID, &r.Plane, &r.LastSyncedAt, &r.Added, &r.Changed, &r.Removed,
			&r.Outcome, &r.Err, &r.CoveredTargets); err != nil {
			return nil, fmt.Errorf("db: credential source scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CredentialResolutionRow is one credential_resolution audit row (non-secret identity metadata only).
type CredentialResolutionRow struct {
	Target       string
	Plane        string
	Outcome      string
	Source       string
	Native       bool
	RuleID       string
	ResolvedUser string
	Scheme       string
	KeyRefScheme string
	Shadowed     []string
	Err          string
	CreatedAt    time.Time
}

// Resolutions returns the recent resolution tail newest-first, capped at limit; a non-empty target filters
// to that target (parameterized — the empty string matches all). Only non-secret columns are selected.
func (s *CredentialReadStore) Resolutions(ctx context.Context, target string, limit int) ([]CredentialResolutionRow, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.p.Pool.Query(ctx, `
		SELECT target, plane, outcome, source, native, rule_id, resolved_user, scheme, key_ref_scheme,
			shadowed, err, created_at
		FROM credential_resolution
		WHERE ($1 = '' OR target = $1)
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, target, limit)
	if err != nil {
		return nil, fmt.Errorf("db: credential resolutions: %w", err)
	}
	defer rows.Close()
	var out []CredentialResolutionRow
	for rows.Next() {
		var r CredentialResolutionRow
		var shadowed string
		if err := rows.Scan(&r.Target, &r.Plane, &r.Outcome, &r.Source, &r.Native, &r.RuleID,
			&r.ResolvedUser, &r.Scheme, &r.KeyRefScheme, &shadowed, &r.Err, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: credential resolution scan: %w", err)
		}
		r.Shadowed = splitShadowed(shadowed)
		out = append(out, r)
	}
	return out, rows.Err()
}

// splitShadowed unpacks the comma-joined non-secret shadowed-source list; empty text yields nil.
func splitShadowed(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// CredentialOutcomeTally is the resolved/unresolved/ambiguous counts for one grouping key.
type CredentialOutcomeTally struct {
	Resolved   int
	Unresolved int
	Ambiguous  int
}

// Total sums the three outcome buckets.
func (t CredentialOutcomeTally) Total() int { return t.Resolved + t.Unresolved + t.Ambiguous }

// CredentialTargetRow is one target's most-recent outcome in the window (coverage frontier).
type CredentialTargetRow struct {
	Target    string
	Outcome   string
	Source    string
	CreatedAt time.Time
}

// CredentialCoverageAgg is the raw coverage aggregation the adapter maps to the console DTO.
type CredentialCoverageAgg struct {
	WindowDays     int
	ByPlane        map[string]CredentialOutcomeTally
	BySource       map[string]CredentialOutcomeTally
	RecentResolved []CredentialTargetRow // newest-first, capped
	RecentRefused  []CredentialTargetRow // newest-first, capped (unresolved + ambiguous)
}

// Coverage aggregates the recent credential_resolution window: per-plane and per-source outcome tallies plus
// each distinct target's most-recent outcome, split into resolved vs refused. The window and the per-list cap
// are bound constants. All four queries are parameterized and select only non-secret columns.
func (s *CredentialReadStore) Coverage(ctx context.Context) (CredentialCoverageAgg, error) {
	out := CredentialCoverageAgg{
		WindowDays: credentialCoverageWindowDays,
		ByPlane:    map[string]CredentialOutcomeTally{},
		BySource:   map[string]CredentialOutcomeTally{},
	}
	days := credentialCoverageWindowDays

	// per-plane outcome tallies.
	if err := s.tally(ctx, `
		SELECT plane, outcome, count(*)
		FROM credential_resolution
		WHERE created_at > now() - make_interval(days => $1)
		GROUP BY plane, outcome`, days, out.ByPlane); err != nil {
		return CredentialCoverageAgg{}, fmt.Errorf("db: credential coverage by plane: %w", err)
	}
	// per-source outcome tallies; '' (native fallback / unresolved) is relabelled for a meaningful key.
	if err := s.tally(ctx, `
		SELECT CASE WHEN source <> '' THEN source WHEN native THEN 'native' ELSE 'unresolved' END AS skey,
			outcome, count(*)
		FROM credential_resolution
		WHERE created_at > now() - make_interval(days => $1)
		GROUP BY skey, outcome`, days, out.BySource); err != nil {
		return CredentialCoverageAgg{}, fmt.Errorf("db: credential coverage by source: %w", err)
	}

	// coverage frontier: each distinct target's most-recent outcome in the window, newest-first.
	frows, err := s.p.Pool.Query(ctx, `
		SELECT target, outcome, source, created_at FROM (
			SELECT DISTINCT ON (target) target, outcome, source, created_at
			FROM credential_resolution
			WHERE created_at > now() - make_interval(days => $1)
			ORDER BY target, created_at DESC, id DESC
		) t
		ORDER BY created_at DESC`, days)
	if err != nil {
		return CredentialCoverageAgg{}, fmt.Errorf("db: credential coverage frontier: %w", err)
	}
	defer frows.Close()
	for frows.Next() {
		var t CredentialTargetRow
		if err := frows.Scan(&t.Target, &t.Outcome, &t.Source, &t.CreatedAt); err != nil {
			return CredentialCoverageAgg{}, fmt.Errorf("db: credential coverage frontier scan: %w", err)
		}
		if t.Outcome == "resolved" {
			if len(out.RecentResolved) < credentialCoverageFrontierCap {
				out.RecentResolved = append(out.RecentResolved, t)
			}
		} else if len(out.RecentRefused) < credentialCoverageFrontierCap {
			out.RecentRefused = append(out.RecentRefused, t)
		}
	}
	return out, frows.Err()
}

// tally runs a (key, outcome, count) GROUP BY and folds it into the destination map.
func (s *CredentialReadStore) tally(ctx context.Context, sql string, days int, dst map[string]CredentialOutcomeTally) error {
	rows, err := s.p.Pool.Query(ctx, sql, days)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, outcome string
		var n int
		if err := rows.Scan(&key, &outcome, &n); err != nil {
			return err
		}
		t := dst[key]
		switch outcome {
		case "resolved":
			t.Resolved += n
		case "unresolved":
			t.Unresolved += n
		case "ambiguous":
			t.Ambiguous += n
		}
		dst[key] = t
	}
	return rows.Err()
}
