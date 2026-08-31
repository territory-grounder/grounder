package db

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/territory-grounder/grounder/core/correlate"
)

// AlertClusterStore hands out the DURABLE cluster identity a correlated cascade needs (alert_cluster,
// migration 0085). It is the fix for the TG-385 defect: before it, every member of a storm recomputed its
// OWN +/-span window at its OWN arrival instant, so the "cluster" was a function of when a subject arrived
// and 157 alerts opened 157 sessions. Now every member's correlation stage JOINs one row keyed by the
// storm's first-seen alert, so all members of one storm resolve to the SAME id and the collapse (TG-376)
// can elect ONE of them to investigate.
//
// It performs no correlation and no election — it only mints/returns the identity. Bound INSERT + SELECT,
// every parameter bound ($1) (INV-03). The cluster key derivation is pure and lives in core/correlate
// (ClusterAnchor / ClusterBucket) so the identity is testable without a database.
type AlertClusterStore struct{ p *Pool }

// NewAlertClusterStore returns the Postgres-backed cluster-identity store.
func NewAlertClusterStore(p *Pool) *AlertClusterStore { return &AlertClusterStore{p: p} }

// Join returns the durable cluster id for the storm identified by (windowBucket, firstSeenRef), inserting
// the row the first time that storm is seen and returning the SAME id on every later member. It is an
// upsert-returning: ON CONFLICT DO UPDATE (a no-op self-assignment) so a concurrent member — the normal case,
// since every alert of a storm runs its correlation activity on its own worker — BLOCKS on the row lock and
// then reads back the one canonical id, rather than racing to insert a duplicate or getting nothing back.
//
// firstSeenRef must be non-empty (it is the storm's anchor identity; the CHECK constraint enforces it too).
// A zero id is never returned without an error. Read/write of ONE operational identity row — it never
// touches the estate and authorizes nothing (the collapse decision is made in the workflow off this id).
func (s *AlertClusterStore) Join(ctx context.Context, windowBucket int64, firstSeenRef string, firstSeenAt time.Time, span time.Duration) (int64, error) {
	if firstSeenRef == "" {
		return 0, fmt.Errorf("db: alert_cluster: empty first_seen_ref (a cluster identity must name its anchor)")
	}

	// STRADDLE-TOLERANT FOLD (TG-465). A storm WIDER than the correlation span FRAGMENTS: a straggler whose
	// symmetric +/-span window no longer reaches the storm's true-earliest alert anchors on a LATER alert, so
	// its (window_bucket, first_seen_ref) key differs and the plain upsert below would mint a SECOND cluster
	// row for the ONE storm — which the cascade-collapse then reads as two separate investigations. Before
	// inserting, probe the adjacent buckets for an EARLIER cluster whose REAL temporal window
	// [first_seen_at, first_seen_at + span_seconds] CONTAINS this anchor's arrival, and JOIN it instead.
	//
	// This changes cluster MEMBERSHIP only, never the collapse decision: that stays causal-gated downstream by
	// core/correlate.IsCausalRule (it collapses only on an estate-indegree / runs_on parent-fanout election), so
	// even a mistaken fold cannot silence a non-causally-related incident. The fold itself is deliberately
	// conservative — it demands REAL temporal containment and fails safe to a new row when the probe is
	// ambiguous — so it never over-merges two genuinely-separate storms.
	//
	// THAT "CANNOT SILENCE" GUARANTEE IS CONTINGENT, AND THE CONTINGENCY IS LOAD-BEARING. It holds ONLY because
	// core/correlate.Elect and Assess recompute the collapse decision from a LIVE per-subject window and consume
	// clusterID solely as a boolean (id > 0), never reading this persisted alert_cluster membership. A future
	// change that couples the election to the DB-joined cluster identity would make foldTarget's containment
	// predicate SAFETY-CRITICAL and MUST re-examine it here; the invariant is pinned by
	// temporal/runner.TestCorrelateActivity_CollapseDecisionIndependentOfClusterIDValue, which breaks the moment
	// the collapse decision starts depending on clusterID's VALUE.
	if id, ok, err := s.foldTarget(ctx, windowBucket, firstSeenRef, firstSeenAt, span); err != nil {
		return 0, err
	} else if ok {
		return id, nil
	}

	var id int64
	err := s.p.QueryRow(ctx, `
		INSERT INTO alert_cluster (window_bucket, first_seen_ref, first_seen_at, span_seconds)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (window_bucket, first_seen_ref)
		  DO UPDATE SET span_seconds = alert_cluster.span_seconds
		RETURNING id`,
		windowBucket, firstSeenRef, firstSeenAt, int(span/time.Second)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: alert_cluster join (%d,%s): %w", windowBucket, firstSeenRef, err)
	}
	return id, nil
}

// foldTarget probes the adjacent window buckets — down to `span`-worth of buckets earlier, the widest an
// earlier anchor whose window can still reach firstSeenAt may sit — for an EXISTING cluster, anchored on a
// DIFFERENT alert, whose real temporal window [first_seen_at, first_seen_at + span_seconds] CONTAINS
// firstSeenAt. It returns (id, true) for the EARLIEST such cluster, so a fragmented straggler converges onto
// the one storm anchor. It returns (0, false) — meaning "insert a new cluster" — in two cases:
//
//   - nothing's real window contains this arrival (the common, non-straddle path, including the storm's own
//     first member); or
//   - the earliest containment is AMBIGUOUS: two distinct clusters share the earliest first_seen_at, so which
//     storm this belongs to cannot be told apart. A fold must NEVER GUESS a merge, so an ambiguous probe fails
//     safe to a new row rather than risk grouping two genuinely-separate incidents (the causal-gated collapse
//     downstream is the real silencing backstop).
//
// The predicate is REAL containment — BOTH bounds on the existing row's stored span_seconds — never merely
// "an adjacent bucket has a cluster": over-merging two non-overlapping storms is the exact failure this must
// avoid. Excluding the caller's own first_seen_ref keeps the normal (and idempotent-retry) path on the
// upsert's ON CONFLICT below, so the concurrent-member self-assignment is untouched.
func (s *AlertClusterStore) foldTarget(ctx context.Context, windowBucket int64, firstSeenRef string, firstSeenAt time.Time, span time.Duration) (int64, bool, error) {
	widthSeconds := int64(correlate.ClusterBucketWidth / time.Second)
	if widthSeconds <= 0 {
		widthSeconds = 1
	}
	spanSeconds := int64(span / time.Second)
	if spanSeconds < 0 {
		spanSeconds = 0
	}
	// ceil(span / width): the most buckets earlier an anchor can sit and still have its window reach
	// firstSeenAt. A containing cluster's first_seen_at <= firstSeenAt, so its bucket <= windowBucket; the scan
	// only needs to look downward from windowBucket by this many buckets.
	spanBuckets := (spanSeconds + widthSeconds - 1) / widthSeconds
	bucketLo := windowBucket - spanBuckets

	// NOT TRANSACTIONAL with the fallback INSERT in Join: the probe SELECT and the upsert are two statements, so
	// two concurrent stragglers for the same storm that both probe BEFORE either commits can each miss the other
	// and mint two rows. That is a benign degradation to the exact pre-fix fragmentation (one storm reads as 2
	// clusters), never an over-merge, and the causal-gated collapse downstream is unaffected — a fallback, not a
	// regression, and not worth a serializable transaction on this hot triage path.
	//
	// count(*) OVER () reports the TRUE number of containing candidates even though only the earliest two rows
	// are read: LIMIT 2 is all the decision needs — the earliest to fold onto, plus the runner-up to detect a tie.
	rows, err := s.p.Query(ctx, `
		SELECT id, first_seen_at, count(*) OVER () AS total
		FROM alert_cluster
		WHERE window_bucket BETWEEN $1 AND $2
		  AND first_seen_ref <> $3
		  AND first_seen_at <= $4
		  AND first_seen_at + make_interval(secs => span_seconds) >= $4
		ORDER BY first_seen_at ASC, id ASC
		LIMIT 2`,
		bucketLo, windowBucket, firstSeenRef, firstSeenAt)
	if err != nil {
		return 0, false, fmt.Errorf("db: alert_cluster fold-probe (%d,%s): %w", windowBucket, firstSeenRef, err)
	}
	defer rows.Close()

	type candidate struct {
		id int64
		at time.Time
	}
	var cands []candidate
	var total int64
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.at, &total); err != nil {
			return 0, false, fmt.Errorf("db: alert_cluster fold-probe scan (%d,%s): %w", windowBucket, firstSeenRef, err)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("db: alert_cluster fold-probe rows (%d,%s): %w", windowBucket, firstSeenRef, err)
	}

	if len(cands) == 0 {
		return 0, false, nil // nothing's window contains this arrival — insert a new cluster
	}
	if len(cands) >= 2 {
		// MULTI-CANDIDATE: two or more distinct correlated storms overlap this arrival within span — the
		// residual case. Logged on BOTH arms so operators can see how often it happens.
		if cands[1].at.Equal(cands[0].at) {
			// AMBIGUOUS: the two earliest share first_seen_at — which storm this belongs to cannot be told
			// apart, so never guess a merge. Fail safe to a new row.
			log.Printf("db: alert_cluster fold-probe (%d,%s): %d clusters contain this arrival, earliest tie %d,%d → ambiguous, new row (no merge)",
				windowBucket, firstSeenRef, total, cands[0].id, cands[1].id)
			return 0, false, nil
		}
		log.Printf("db: alert_cluster fold-probe (%d,%s): %d clusters contain this arrival → folding onto earliest %d (runner-up %d)",
			windowBucket, firstSeenRef, total, cands[0].id, cands[1].id)
	}
	return cands[0].id, true, nil // fold onto the EARLIEST containing cluster
}
