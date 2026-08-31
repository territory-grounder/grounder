package db

// forensic_timeline_read.go — THE BOUND, READ-ONLY WINDOW READER BEHIND THE CROSS-INCIDENT TIMELINE
// (TG-168).
//
// core/forensic holds the pure merge; this holds the only thing it cannot: getting the rows. Five bound
// SELECTs over the corpora that are actually populated, each restricted to a half-open [from, to) window
// and an optional host, each with a hard row cap.
//
// READ-ONLY BY CONSTRUCTION. Five SELECTs, no mutation, every parameter bound ($1..) and never
// string-built (INV-03). Nothing here writes, and nothing here resolves a credential — the projections
// are the non-secret columns the console and the trace already expose (INV-13).
//
// THE CAP IS NOT A DETAIL. governance_ledger alone holds 9,719 rows and agent_step 18,736; a forensic
// question over a wide window would otherwise pull the corpus into memory to sort it. Each query is
// bounded, and the reader REPORTS whether a cap bit — because a silently truncated narrative and a
// complete one are the same object, and this whole package exists so an operator can trust position and
// completeness. A truncated window is a legitimate answer; an undeclared one is not.

import (
	"context"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/forensic"
)

// ForensicStore is the pgx-backed window reader.
type ForensicStore struct{ p *Pool }

// NewForensicStore returns the Postgres-backed cross-incident window reader.
func NewForensicStore(p *Pool) *ForensicStore { return &ForensicStore{p: p} }

// DefaultForensicCapPerCorpus bounds each corpus's contribution to one reconstruction. Deliberately
// per-corpus rather than global: a global cap would let the chattiest corpus (agent_step, 18,736 rows)
// crowd out every other lane and produce a narrative that looks like an agent transcript rather than an
// account of what happened.
const DefaultForensicCapPerCorpus = 2000

// ForensicRead is one window's worth of events plus the honesty about what it did not return.
type ForensicRead struct {
	Events []forensic.Event
	// Truncated names every corpus whose cap bit. Empty means the window was returned in full. This is
	// the field that keeps a partial reconstruction from reading as a complete one.
	Truncated []string
	// Dropped counts events the merge refused because they carried no usable timestamp.
	Dropped int
}

// Window reads every configured corpus for [w.From, w.To) and returns the merged, deterministically
// ordered stream.
//
// An invalid window is REFUSED rather than widened. `forensic.Window.Valid` rejects a zero From, because
// an unbounded reconstruction over these corpora is a data dump the caller almost certainly did not mean
// to request — and quietly substituting a default window would answer a different question from the one
// asked.
func (s *ForensicStore) Window(ctx context.Context, w forensic.Window, host string, cap int) (ForensicRead, error) {
	var out ForensicRead
	if s == nil || s.p == nil {
		return out, fmt.Errorf("db: forensic window read with no pool")
	}
	if !w.Valid() {
		return out, fmt.Errorf("db: forensic window [%s, %s) is not a bounded window — an unbounded "+
			"reconstruction over these corpora is a dump, not an answer; set From (and To for a closed range)",
			w.From.Format(time.RFC3339), w.To.Format(time.RFC3339))
	}
	if cap <= 0 {
		cap = DefaultForensicCapPerCorpus
	}
	// A zero To means "everything since From". Pass a far-future bound so the SQL stays one shape rather
	// than branching — a second query text is a second thing to keep correct.
	to := w.To
	if to.IsZero() {
		to = time.Now().UTC().Add(24 * time.Hour)
	}

	type lane struct {
		name  string
		query string
		src   forensic.Source
	}
	// hostFilter is applied as `($3 = '' OR host = $3)` INSIDE each statement rather than by appending
	// SQL, so there is exactly one query text per lane and no string-built predicate (INV-03).
	lanes := []lane{
		{"ingest_alert", `
			SELECT received_at, COALESCE(host,''), COALESCE(external_ref,''), '', COALESCE(alert_rule,''), COALESCE(summary,'')
			FROM ingest_alert
			WHERE received_at >= $1 AND received_at < $2 AND ($3 = '' OR host = $3)
			ORDER BY received_at LIMIT $4`, forensic.SourceIngest},
		// governance_ledger carries no host column: its rows are about an ACTION, not a machine. The host
		// filter is therefore a no-op here rather than silently excluding the whole lane — an operator
		// asking "what happened on web01" still wants the decisions that concerned it, and those are
		// reachable through the subject ref, not through a column that does not exist.
		{"governance_ledger", `
			SELECT created_at, '', COALESCE(action_id,''), '', COALESCE(decision,''), COALESCE(reason,'')
			FROM governance_ledger
			WHERE created_at >= $1 AND created_at < $2 AND ($3 = '' OR $3 <> '')
			ORDER BY created_at LIMIT $4`, forensic.SourceLedger},
		{"agent_step", `
			SELECT created_at, '', COALESCE(external_ref,''), '',
			       'agent-cycle:' || COALESCE(NULLIF(tool,''),'(no tool)'), COALESCE(outcome,'')
			FROM agent_step
			WHERE created_at >= $1 AND created_at < $2 AND ($3 = '' OR $3 <> '')
			ORDER BY created_at LIMIT $4`, forensic.SourceAgentStep},
		// resolved_user is the ATTRIBUTED identity, not a credential — the same non-secret projection the
		// trace already exposes (INV-13). target is the resource the identity was resolved FOR.
		{"credential_resolution", `
			SELECT created_at, COALESCE(target,''), COALESCE(external_ref,''), COALESCE(resolved_user,''),
			       'credential-resolve:' || COALESCE(outcome,''), COALESCE(plane,'')
			FROM credential_resolution
			WHERE created_at >= $1 AND created_at < $2 AND ($3 = '' OR target = $3)
			ORDER BY created_at LIMIT $4`, forensic.SourceCredential},
		// exec_class_decision stamps decided_at, NOT created_at, and carries no host. Both were wrong in
		// the first draft of this file and were caught by reading the live schema rather than assuming it.
		{"exec_class_decision", `
			SELECT decided_at, '', COALESCE(external_ref,''), '', COALESCE(exec_class,''), COALESCE(reason,'')
			FROM exec_class_decision
			WHERE decided_at >= $1 AND decided_at < $2 AND ($3 = '' OR $3 <> '')
			ORDER BY decided_at LIMIT $4`, forensic.SourceExecClass},
	}

	groups := make([][]forensic.Event, 0, len(lanes))
	for _, l := range lanes {
		evs, err := s.readLane(ctx, l.query, l.src, w.From, to, host, cap)
		if err != nil {
			return ForensicRead{}, fmt.Errorf("db: forensic lane %s: %w", l.name, err)
		}
		if len(evs) >= cap {
			out.Truncated = append(out.Truncated, l.name)
		}
		groups = append(groups, evs)
	}

	out.Events, out.Dropped = forensic.Merge(groups...)
	return out, nil
}

func (s *ForensicStore) readLane(ctx context.Context, q string, src forensic.Source, from, to time.Time, host string, cap int) ([]forensic.Event, error) {
	rows, err := s.p.Query(ctx, q, from, to, host, cap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []forensic.Event
	for rows.Next() {
		var e forensic.Event
		if err := rows.Scan(&e.At, &e.Host, &e.SubjectRef, &e.Actor, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		e.Source = src
		out = append(out, e)
	}
	return out, rows.Err()
}
