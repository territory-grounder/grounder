package db

// The pgx read side of the governance monitors (spec/004 REQ-305/307, TG-222). Before this file the ONLY
// implementations of governance.SessionStore / JudgmentStore / PairSource in the tree were test fakes, so
// the monitors were code-complete and unreachable.
//
// JUDGE-INDEPENDENCE (REQ-305) is the property this file has to preserve and is easy to lose: the
// DENOMINATOR must come from columns the judge process holds no write grant on. `session_triage` is written
// by the Runner at a session's terminal outcome — except for `judged`, which MarkJudged sets. So
// RecentlyEnded reads `external_ref` and `created_at` and never `judged`; a dead judge therefore cannot
// shrink its own eligibility set by simply not marking anything.
//
// Parameters are always bound ($1) — no string-built SQL (INV-03).

import (
	"context"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/governance"
)

// GovernanceReadStore serves both governance monitors from one pool.
type GovernanceReadStore struct {
	p *Pool
	// Window bounds how far back a session is drawn as "recently ended". The monitor applies its own
	// recency/lag bounds on top; this one only keeps the query cheap.
	Window time.Duration
	// Limit caps one read.
	Limit int
}

// NewGovernanceReadStore returns the pgx-backed governance reader over a trailing window.
func NewGovernanceReadStore(p *Pool, window time.Duration, limit int) *GovernanceReadStore {
	if window <= 0 {
		window = 24 * time.Hour
	}
	if limit <= 0 {
		limit = 500
	}
	return &GovernanceReadStore{p: p, Window: window, Limit: limit}
}

func secondsInterval(d time.Duration) string { return fmt.Sprintf("%d seconds", int(d.Seconds())) }

// RecentlyEnded returns the recently-ended sessions from the judge-INDEPENDENT columns of session_triage.
//
// `created_at` is the terminal-outcome stamp (RecordTriage writes the row when the session ends), so it IS
// the ended-at instant for this purpose.
//
// Synthetic now reads the STRUCTURAL marker (session_triage.synthetic, migration 0069) rather than being
// hardcoded false. This comment used to say "when a synthetic marker is introduced this reads it" — TG-190
// introduced it, so it does. The refusal it recorded still stands and is why the marker is a COLUMN: a name
// convention "would silently drop real sessions out of the denominator on a naming coincidence, which
// inflates the judged fraction and hides a dead judge". A column cannot be collided with.
//
// COALESCE, not a bare read: a database that predates 0069 has no such column on a replica mid-migration,
// and the conservative answer there is FALSE — every session stays IN the liveness denominator, which is the
// direction that can only ever report the judge as less healthy than it is.
func (s *GovernanceReadStore) RecentlyEnded(ctx context.Context) ([]governance.Session, error) {
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, created_at, COALESCE(synthetic, false)
		FROM session_triage
		WHERE created_at > now() - $1::interval
		ORDER BY created_at DESC
		LIMIT $2`, secondsInterval(s.Window), s.Limit)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: recently-ended sessions: %w", err)
	}
	defer rows.Close()
	var out []governance.Session
	for rows.Next() {
		var sess governance.Session
		if err := rows.Scan(&sess.SessionID, &sess.EndedAt, &sess.Synthetic); err != nil {
			return nil, fmt.Errorf("db: scan recently-ended session: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// HasRealJudgment reports whether a REAL local judgment exists for a session.
//
// "Real" is `score > 0`, the convention every other judged-score reader in this tree already uses
// (skillstore_trial.armScoresForDim, skillstore_flywheel.DimensionMeans, axis_read.Aggregate). TG never
// writes the predecessor's `-1` unscored row — JudgeBatchActivity OMITS a dimension the judge did not score
// rather than fabricating one — so "the judge did not score this" is an ABSENT row here, and the existence
// query is the whole test. A read error reports FALSE: an unreadable numerator must depress the judged
// fraction toward "dead", never inflate it toward "healthy".
func (s *GovernanceReadStore) HasRealJudgment(ctx context.Context, sessionID string) bool {
	var exists bool
	err := s.p.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM session_judgment WHERE external_ref = $1 AND score > 0)`,
		sessionID).Scan(&exists)
	if err != nil {
		return false
	}
	return exists
}

// RecentForCrossCheck returns a sample of recently-ended sessions with the local judge's mean score, for the
// frontier cross-check. The LEFT JOIN is load-bearing: a session the local judge scored not at all must
// still appear (LocalScored=false) — those rows ARE the DEATH signal, and an inner join would silently
// delete the exact evidence the anchor exists to find.
func (s *GovernanceReadStore) RecentForCrossCheck(ctx context.Context, limit int) ([]governance.CrossCheckRow, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.p.Query(ctx, `
		SELECT t.external_ref, t.host, t.alert_rule, t.band, t.outcome, t.proposed, t.op, t.conclusion, t.prediction,
		       COALESCE(j.n, 0) AS scored_dims, COALESCE(j.mean_score, 0) AS mean_score
		FROM session_triage t
		LEFT JOIN (
			SELECT external_ref, count(*) AS n, avg(score) AS mean_score
			FROM session_judgment WHERE score > 0 GROUP BY external_ref
		) j ON j.external_ref = t.external_ref
		WHERE t.created_at > now() - $1::interval
		ORDER BY t.created_at DESC
		LIMIT $2`, secondsInterval(s.Window), limit)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: cross-check sample: %w", err)
	}
	defer rows.Close()
	var out []governance.CrossCheckRow
	for rows.Next() {
		var js governance.CrossCheckRow
		var scoredDims int
		if err := rows.Scan(&js.ExternalRef, &js.Host, &js.AlertRule, &js.Band, &js.Outcome, &js.Proposed,
			&js.Op, &js.Conclusion, &js.Prediction, &scoredDims, &js.LocalMean); err != nil {
			return nil, fmt.Errorf("db: scan cross-check row: %w", err)
		}
		js.LocalScored = scoredDims > 0
		out = append(out, js)
	}
	return out, rows.Err()
}

// compile-time proof the pgx reader satisfies both governance ports.
var (
	_ governance.SessionStore          = (*GovernanceReadStore)(nil)
	_ governance.JudgmentStore         = (*GovernanceReadStore)(nil)
	_ governance.CrossCheckSampleStore = (*GovernanceReadStore)(nil)
)
