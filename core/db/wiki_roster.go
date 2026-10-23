package db

import (
	"context"
	"fmt"
	"time"
)

// WikiHostRoster returns every host TG has actually recorded a triage session against, ordered.
//
// WHY A ROSTER READ AT ALL. The console currently derives its host list from the ESTATE GRAPH
// (deploy/console/v2/modules/_live/js.txt: `est.nodes`), which means a host that TG has triaged but that
// discovery has never registered gets no page whatsoever — the machine with incidents against it is the
// one most worth reading about, and it is precisely the one that falls out. Compiling from the incident
// spine inverts that: a host earns a page by having been dealt with, not by being in a graph.
//
// The estate graph still contributes to a page (dependencies/dependents); it just no longer decides who
// gets one.
//
// Absent judge tables degrade to an empty roster rather than an error — the same rule PriorSessions
// applies at incident_history_read.go, and for the same reason: history begins when the spine does, and a
// pre-migration database is an honest nothing rather than an outage.
func (s *IncidentHistoryStore) WikiHostRoster(ctx context.Context) ([]string, error) {
	rows, err := s.p.Query(ctx, `
		SELECT DISTINCT host
		FROM session_triage
		WHERE host IS NOT NULL AND host <> ''
		ORDER BY host`)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: wiki host roster: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("db: scan wiki host roster: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// WikiSourceCounts reports the denominators a compile actually saw, for the envelope's Sources block. They
// are read in ONE round trip with the roster so the published numbers describe one consistent moment; two
// counts taken seconds apart can disagree and produce a page that contradicts itself.
func (s *IncidentHistoryStore) WikiSourceCounts(ctx context.Context) (sessions, hosts int, err error) {
	if err := s.p.QueryRow(ctx, `
		SELECT count(*), count(DISTINCT host) FILTER (WHERE host IS NOT NULL AND host <> '')
		FROM session_triage`).Scan(&sessions, &hosts); err != nil {
		if isUndefinedTable(err) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("db: wiki source counts: %w", err)
	}
	return sessions, hosts, nil
}

// WikiRuleSession is one triage row as the per-rule wiki compiler needs it.
type WikiRuleSession struct {
	ExternalRef    string
	Host           string
	AlertRule      string
	Outcome        string
	OpClass        string
	Mutated        bool
	ConfirmedClear bool
	CreatedAt      time.Time
}

// WikiRuleSessions returns the newest triage rows carrying an alert rule, newest first, bounded.
//
// ONE query rather than one per rule: the per-HOST pages read per host because a host page is naturally
// scoped that way and the roster is small, but rule pages group ACROSS hosts — 78 rule families over
// 3,202 sessions in production — so 78 round trips would buy nothing. The grouping happens in the pure
// compiler, which is also where it can be tested.
//
// The caller folds AlertRule into its family (knowledge.CanonicalRule) before compiling; this read stays
// deliberately dumb about vocabulary so the alias authority has exactly one home.
//
// Absent judge tables degrade to no rows, the same rule PriorSessions applies.
func (s *IncidentHistoryStore) WikiRuleSessions(ctx context.Context, limit int) ([]WikiRuleSession, error) {
	if limit <= 0 || limit > 20000 {
		limit = 5000
	}
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, COALESCE(host,''), alert_rule, COALESCE(outcome,''), COALESCE(op_class,''),
		       mutated, confirmed_clear, created_at
		FROM session_triage
		WHERE alert_rule IS NOT NULL AND alert_rule <> ''
		ORDER BY created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: wiki rule sessions: %w", err)
	}
	defer rows.Close()
	var out []WikiRuleSession
	for rows.Next() {
		var r WikiRuleSession
		if err := rows.Scan(&r.ExternalRef, &r.Host, &r.AlertRule, &r.Outcome, &r.OpClass,
			&r.Mutated, &r.ConfirmedClear, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan wiki rule session: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// WikiDecisionTally is one governance-decision kind with its counts.
type WikiDecisionTally struct {
	Decision string
	Total    int
	Withheld int
	Newest   time.Time
}

// WikiDecisionTallies aggregates the governance ledger by decision kind — the digest behind the wiki's
// decisions page.
//
// Aggregated in SQL rather than by reading 8,570 rows into the compiler: the page reports counts, the
// counts are what the database is for, and streaming the whole hash-chained ledger through a markdown
// renderer to produce twelve numbers would be the wrong shape at any size.
//
// Absent tables degrade to no rows, the same rule the rest of this file follows.
func (s *IncidentHistoryStore) WikiDecisionTallies(ctx context.Context) ([]WikiDecisionTally, int, error) {
	var total int
	if err := s.p.QueryRow(ctx, `SELECT count(*) FROM governance_ledger`).Scan(&total); err != nil {
		if isUndefinedTable(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("db: wiki decision total: %w", err)
	}
	rows, err := s.p.Query(ctx, `
		SELECT decision, count(*), count(*) FILTER (WHERE withheld), max(created_at)
		FROM governance_ledger
		WHERE decision IS NOT NULL AND decision <> ''
		GROUP BY decision
		ORDER BY count(*) DESC, decision ASC`)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("db: wiki decision tallies: %w", err)
	}
	defer rows.Close()
	var out []WikiDecisionTally
	for rows.Next() {
		var t WikiDecisionTally
		if err := rows.Scan(&t.Decision, &t.Total, &t.Withheld, &t.Newest); err != nil {
			return nil, 0, fmt.Errorf("db: scan wiki decision tally: %w", err)
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}
