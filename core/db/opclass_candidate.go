package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/territory-grounder/grounder/core/opclasscat"
)

// OpClassCandidateStore is the pgx surface over opclass_candidate + opclass_candidate_occurrence
// (migration 0048, spec/028 REQ-2801/REQ-2802). Parameterized SQL only; the exactly-once evidence
// property is STRUCTURAL — the occurrence PRIMARY KEY (candidate_key, external_ref) plus
// ON CONFLICT DO NOTHING, so a re-proposal of the same incident is a no-op at the storage layer rather
// than a fact some later query has to remember to de-duplicate.
type OpClassCandidateStore struct{ p *Pool }

// NewOpClassCandidateStore returns the Postgres-backed candidacy store.
func NewOpClassCandidateStore(p *Pool) *OpClassCandidateStore { return &OpClassCandidateStore{p: p} }

// Compile-time proof that the store satisfies the lifecycle contract. Without this, a signature drift
// would only surface at the composition root — the exact class of defect the Stage-1 review caught
// (a fully-tested surface that was never actually wired).
var _ opclasscat.Store = (*OpClassCandidateStore)(nil)

// RecordOccurrence appends one screened observation. FIRST write wins per (key, ref): the earliest
// observation is the honest one, and a re-proposal is not new evidence.
func (s *OpClassCandidateStore) RecordOccurrence(ctx context.Context, occ opclasscat.Occurrence) error {
	actor := occ.ActorEvidence
	if len(actor) == 0 {
		actor = []byte("[]")
	}
	_, err := s.p.Exec(ctx, `
		INSERT INTO opclass_candidate_occurrence
		  (candidate_key, external_ref, host, target, op, op_class, rationale, undo_sketch,
		   confidence, evidence_ids, actor_evidence, band, outcome, observed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12,$13,COALESCE($14, now()))
		ON CONFLICT (candidate_key, external_ref) DO NOTHING`,
		occ.CandidateKey, occ.ExternalRef, occ.Host, occ.Target, occ.Op, occ.OpClass,
		occ.Rationale, occ.UndoSketch, occ.Confidence, occ.EvidenceIDs, string(actor),
		occ.Band, occ.Outcome, nullTime(occ.ObservedAt))
	if err != nil {
		return fmt.Errorf("record opclass occurrence %s/%s: %w", occ.ExternalRef, occ.OpClass, err)
	}
	return nil
}

// UpsertObserving creates the live observing row for a key if none exists and refreshes last_seen_at.
// The partial unique index (live statuses only) makes "one live candidacy per key" structural, so a
// concurrent pass cannot open a second candidacy for the same remedy.
func (s *OpClassCandidateStore) UpsertObserving(ctx context.Context, key string, occ opclasscat.Occurrence) error {
	seen := occ.ObservedAt
	if seen.IsZero() {
		seen = time.Now().UTC()
	}
	_, err := s.p.Exec(ctx, `
		INSERT INTO opclass_candidate (candidate_key, op_class, op, status, first_seen_at, last_seen_at)
		VALUES ($1,$2,$3,'observing',$4,$4)
		ON CONFLICT (candidate_key) WHERE status NOT IN ('dismissed','expired')
		DO UPDATE SET last_seen_at = GREATEST(opclass_candidate.last_seen_at, EXCLUDED.last_seen_at)`,
		key, occ.OpClass, occ.Op, seen)
	if err != nil {
		return fmt.Errorf("upsert opclass candidate %s: %w", occ.OpClass, err)
	}
	return nil
}

// LiveCandidates returns every non-terminal row — the clustering pass's work list.
func (s *OpClassCandidateStore) LiveCandidates(ctx context.Context) ([]opclasscat.Candidate, error) {
	rows, err := s.p.Query(ctx, `
		SELECT id, candidate_key, op_class, op, param_names, status, auto_barred, family, tier,
		       dossier_hash, dismissed_at, dismiss_until, rationale, ledger_seq,
		       first_seen_at, last_seen_at, status_changed_at
		FROM opclass_candidate
		WHERE status IN ('observing','candidate','ratify_ready')
		ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list opclass candidates: %w", err)
	}
	defer rows.Close()

	var out []opclasscat.Candidate
	for rows.Next() {
		var c opclasscat.Candidate
		var status string
		if err := rows.Scan(&c.ID, &c.CandidateKey, &c.OpClass, &c.Op, &c.ParamNames, &status,
			&c.AutoBarred, &c.Family, &c.Tier, &c.DossierHash, &c.DismissedAt, &c.DismissUntil,
			&c.Rationale, &c.LedgerSeq, &c.FirstSeenAt, &c.LastSeenAt, &c.StatusChangedAt); err != nil {
			return nil, fmt.Errorf("scan opclass candidate: %w", err)
		}
		c.Status = opclasscat.Status(status)
		out = append(out, c)
	}
	return out, rows.Err()
}

// CandidateByKey loads ONE candidate for the operator lane (spec/028 T-028-7).
//
// Unlike LiveCandidates it does not filter by status, and that is deliberate: a verb aimed at a decided key
// must be refused by the STATE MACHINE, with its "ratified -> ratified is not a legal transition" message,
// rather than by a query that returns nothing and makes a decided candidate look like a missing one. An
// operator who clicks ratify twice deserves "already ratified", not "no such candidate".
func (s *OpClassCandidateStore) CandidateByKey(ctx context.Context, key string) (opclasscat.Candidate, bool, error) {
	var c opclasscat.Candidate
	var status string
	err := s.p.QueryRow(ctx, `
		SELECT id, candidate_key, op_class, op, param_names, status, auto_barred, family, tier,
		       dossier_hash, dismissed_at, dismiss_until, rationale, ledger_seq,
		       first_seen_at, last_seen_at, status_changed_at
		FROM opclass_candidate
		WHERE candidate_key = $1`, key).
		Scan(&c.ID, &c.CandidateKey, &c.OpClass, &c.Op, &c.ParamNames, &status,
			&c.AutoBarred, &c.Family, &c.Tier, &c.DossierHash, &c.DismissedAt, &c.DismissUntil,
			&c.Rationale, &c.LedgerSeq, &c.FirstSeenAt, &c.LastSeenAt, &c.StatusChangedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return opclasscat.Candidate{}, false, nil
	}
	if err != nil {
		return opclasscat.Candidate{}, false, fmt.Errorf("load opclass candidate: %w", err)
	}
	c.Status = opclasscat.Status(status)
	return c, true, nil
}

// Occurrences returns a key's journal inside the window, newest first.
func (s *OpClassCandidateStore) Occurrences(ctx context.Context, key string, since time.Time) ([]opclasscat.Occurrence, error) {
	rows, err := s.p.Query(ctx, `
		SELECT candidate_key, external_ref, host, target, op, op_class, rationale, undo_sketch,
		       confidence, evidence_ids, actor_evidence::text, band, outcome, observed_at
		FROM opclass_candidate_occurrence
		WHERE candidate_key = $1 AND observed_at > $2
		ORDER BY observed_at DESC`, key, since)
	if err != nil {
		return nil, fmt.Errorf("list opclass occurrences: %w", err)
	}
	defer rows.Close()

	var out []opclasscat.Occurrence
	for rows.Next() {
		var o opclasscat.Occurrence
		var actor string
		if err := rows.Scan(&o.CandidateKey, &o.ExternalRef, &o.Host, &o.Target, &o.Op, &o.OpClass,
			&o.Rationale, &o.UndoSketch, &o.Confidence, &o.EvidenceIDs, &actor,
			&o.Band, &o.Outcome, &o.ObservedAt); err != nil {
			return nil, fmt.Errorf("scan opclass occurrence: %w", err)
		}
		o.ActorEvidence = []byte(actor)
		out = append(out, o)
	}
	return out, rows.Err()
}

// CandidateTallies returns the per-shape recurrence counts for the WHOLE live journal, keyed by
// candidate_key (TG-236 oracles 2 and 3).
//
// One aggregate read, not one read per row: the queue renders many shapes at once, and the alternative —
// calling Occurrences per candidate — is an N+1 that grows with the very recurrence the surface exists to
// celebrate. The dossier still reads the full journal for the single shape under an operator's eye.
//
// The counts are exact rather than approximate BY CONSTRUCTION: the journal's primary key is
// (candidate_key, external_ref), so there is exactly one row per distinct incident. count(*) is therefore
// the distinct-ref count the thresholds are defined over, and avg(confidence) is the mean across distinct
// refs — the same quantity Summarize computes from the full journal. Were the PK ever widened, these two
// equalities would silently break, which is why the reasoning is recorded here and asserted in the store's
// migration test rather than left to a reader to reconstruct.
func (s *OpClassCandidateStore) CandidateTallies(ctx context.Context) (map[string]opclasscat.Tally, error) {
	rows, err := s.p.Query(ctx, `
		SELECT candidate_key,
		       count(*)                                        AS occurrences,
		       count(DISTINCT host) FILTER (WHERE host <> '')  AS hosts,
		       -- seconds, not an interval: pgx has no scan path from interval to time.Duration, and a
		       -- float of seconds converts unambiguously at the one call site below.
		       EXTRACT(EPOCH FROM (max(observed_at) - min(observed_at))) AS span_seconds,
		       avg(confidence)                                 AS mean_confidence
		FROM opclass_candidate_occurrence
		GROUP BY candidate_key`)
	if err != nil {
		return nil, fmt.Errorf("tally opclass occurrences: %w", err)
	}
	defer rows.Close()

	out := make(map[string]opclasscat.Tally)
	for rows.Next() {
		var (
			key  string
			t    opclasscat.Tally
			span time.Duration
			mean *float64
		)
		if err := rows.Scan(&key, &t.Occurrences, &t.Hosts, &span, &mean); err != nil {
			return nil, fmt.Errorf("scan opclass tally: %w", err)
		}
		t.Span = span
		if mean != nil {
			t.MeanConfidence = *mean
		}
		out[key] = t
	}
	return out, rows.Err()
}

// UpdateCandidate persists a transition's result. Called ONLY by opclasscat.Transition, which has
// already appended the chain entry — never call it directly, or a status change lands unrecorded.
func (s *OpClassCandidateStore) UpdateCandidate(ctx context.Context, c opclasscat.Candidate) error {
	_, err := s.p.Exec(ctx, `
		UPDATE opclass_candidate
		SET status = $2, auto_barred = $3, family = $4, tier = $5, dossier_hash = $6,
		    dismissed_at = $7, dismiss_until = $8, rationale = $9, ledger_seq = $10,
		    status_changed_at = $11
		WHERE candidate_key = $1 AND status IN ('observing','candidate','ratify_ready')`,
		c.CandidateKey, string(c.Status), c.AutoBarred, c.Family, c.Tier, c.DossierHash,
		c.DismissedAt, c.DismissUntil, c.Rationale, c.LedgerSeq, c.StatusChangedAt)
	if err != nil {
		return fmt.Errorf("update opclass candidate %s: %w", c.OpClass, err)
	}
	return nil
}

// Liveness supplies the clustering cron's DEAD-MAN inputs (REQ-2812). Both facts are read from tables
// this store's caller does NOT write: the newest occurrence (written by the runner's shadow activity)
// and the session count in the same window (written by the runner's triage record). If the seam
// between them breaks, the two disagree and the cron refuses its pass — the dead-judge lesson.
func (s *OpClassCandidateStore) Liveness(ctx context.Context, window time.Duration) (time.Time, int, error) {
	var newest *time.Time
	err := s.p.QueryRow(ctx, `SELECT max(observed_at) FROM opclass_candidate_occurrence`).Scan(&newest)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, 0, fmt.Errorf("newest occurrence: %w", err)
	}
	var sessions int
	if err := s.p.QueryRow(ctx,
		`SELECT count(*) FROM session_triage WHERE created_at > now() - $1::interval`,
		window.String()).Scan(&sessions); err != nil {
		return time.Time{}, 0, fmt.Errorf("session volume: %w", err)
	}
	var n time.Time
	if newest != nil {
		n = *newest
	}
	return n, sessions, nil
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
