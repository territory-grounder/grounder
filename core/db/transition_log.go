package db

import (
	"context"
	"log"
	"time"

	"github.com/territory-grounder/grounder/core/httpapi"
)

// TransitionLogStore is the pgx-backed DURABLE recovery-transition log (ingest_transition, migration 0034):
// the front door's retained evidence of LibreNMS's own recovery assertions, so the workflow's confirmed-clear
// check can be driven by TG's OWN captured observation rather than a lagging re-pull that races the poller
// (spec/012 clear-confirm). Append-only by table grant (the runtime role holds no UPDATE/DELETE, REQ-2016).
// Bound params only, never string-built.
type TransitionLogStore struct{ p *Pool }

// NewTransitionLogStore returns the Postgres-backed durable recovery-transition log.
func NewTransitionLogStore(p *Pool) *TransitionLogStore { return &TransitionLogStore{p: p} }

// compile-time proof it satisfies the front-door recorder seam the ingest handler writes through.
var _ httpapi.TransitionRecorder = (*TransitionLogStore)(nil)

// Append records one transition observation. Best-effort by contract (the ingest path must never block on the
// log): a write failure is logged, never propagated. Unlike the alert log there is NO ON CONFLICT — a fault
// can recover, re-fire, and recover again, so many transitions legitimately share one external_ref, each an
// independent timestamped observation.
func (s *TransitionLogStore) Append(ctx context.Context, rec httpapi.TransitionRecord) {
	kind := rec.Kind
	if kind == "" {
		kind = "recovery"
	}
	var observed any // nullable — a zero ObservedAt writes SQL NULL, never a spurious epoch
	if !rec.ObservedAt.IsZero() {
		observed = rec.ObservedAt
	}
	received := rec.ReceivedAt
	if received.IsZero() {
		received = time.Now().UTC()
	}
	if _, err := s.p.Exec(ctx, `
		INSERT INTO ingest_transition
		  (external_ref, kind, host, site, alert_rule, observed_at, received_at, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1)`,
		rec.ExternalRef, kind, rec.Host, rec.Site, rec.AlertRule, observed, received); err != nil {
		log.Printf("db: ingest_transition append %s failed (non-blocking): %v", rec.ExternalRef, err)
	}
}

// RecoveredSince reports whether a recovery transition for host was observed at or after `since` — the
// belt-and-braces lost-signal read for the workflow clear-confirm loop (a provider-asserted recovery TG
// durably captured, never the model's word, INV-11). Bound params; a query error returns (false, err) so the
// caller fails CLOSED (not recovered ⇒ hold To Verify), never a spurious clear.
func (s *TransitionLogStore) RecoveredSince(ctx context.Context, host string, since time.Time) (bool, error) {
	var n int
	if err := s.p.QueryRow(ctx, `
		SELECT count(*) FROM ingest_transition
		WHERE host = $1 AND kind = 'recovery' AND received_at >= $2`, host, since).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}
