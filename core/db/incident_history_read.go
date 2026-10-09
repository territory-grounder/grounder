package db

import (
	"context"
	"fmt"
	"time"
)

// IncidentHistoryStore is the READ-ONLY session_triage window behind the get-incident-history agent tool
// (modules/observability/incidenthistory): the prior triage sessions recorded for one host, newest first.
// It serves RECOGNITION — "has TG seen this host fail this way before, and how did that end?" — the single
// biggest correct_diagnosis lever the predecessor had and TG lacked (every recurrence was re-derived from
// scratch). Rule-FAMILY scoping deliberately does NOT happen here: the family authority is
// core/knowledge.CanonicalRule (Go, case-insensitive), and pushing an alias list into SQL would re-create
// the exact two-vocabulary drift the recovery belt already paid for — so this store returns the host's
// sessions and the tool folds them by family. Parameterized SQL only; read-only by construction.
type IncidentHistoryStore struct{ p *Pool }

// NewIncidentHistoryStore returns the Postgres-backed prior-incident reader.
func NewIncidentHistoryStore(p *Pool) *IncidentHistoryStore { return &IncidentHistoryStore{p: p} }

// PriorTriage is one prior session's compact terminal record, projected for recognition: what fired
// (AlertRule), how the session ended (Outcome — the orchestrator's terminal string, e.g. "proposed",
// "escalated:budget-exceeded", "proposal timeout — stood down without mutation"), what it proposed
// (OpClass, "" for a no-proposal stop), whether TG actually actuated (Mutated) and whether the condition
// was re-observed clear afterwards (ConfirmedClear — the fail-closed A3 heal signal, migration 0039), and
// the session's own conclusion text. Every field is a recorded observation; nothing here re-enters a gate.
type PriorTriage struct {
	ExternalRef    string
	AlertRule      string
	Outcome        string
	OpClass        string
	Proposed       bool
	Mutated        bool
	ConfirmedClear bool
	Conclusion     string
	CreatedAt      time.Time
}

// PriorSessions returns the newest prior triage sessions recorded for a host, newest first, bounded by
// limit (<=0 clamps to 50). Absent judge tables ⇒ no rows (honest: history begins when the spine does —
// the same degradation DimensionMeans applies), never an error the tool would misread as an outage.
func (s *IncidentHistoryStore) PriorSessions(ctx context.Context, host string, limit int) ([]PriorTriage, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, alert_rule, outcome, op_class, proposed, mutated, confirmed_clear, conclusion, created_at
		FROM session_triage
		WHERE host = $1
		ORDER BY created_at DESC
		LIMIT $2`, host, limit)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: prior sessions for host %s: %w", host, err)
	}
	defer rows.Close()
	var out []PriorTriage
	for rows.Next() {
		var r PriorTriage
		if err := rows.Scan(&r.ExternalRef, &r.AlertRule, &r.Outcome, &r.OpClass, &r.Proposed,
			&r.Mutated, &r.ConfirmedClear, &r.Conclusion, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan prior session: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
