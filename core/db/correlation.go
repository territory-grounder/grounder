package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/correlate"
)

// CorrelationStore is the EVIDENCE READER behind the incident correlation stage (TG-169): the alerts TG
// itself admitted around an incident, read straight out of the front-door ledger (ingest_alert, migration
// 0033).
//
// WHY THIS LEDGER AND NOT SOMETHING NEW. Same discipline as CascadeLatencyStore: the correlator has to
// answer "what else was going on", and ingest_alert is the one durable, append-only record of what the
// front door actually admitted. It needs no new table, no new write on the ingest path, and no change to
// the ingest boundary — and it is the SAME stream every other learned signal (cascade latency,
// co-occurrence confidence, sibling corroboration) is derived from, so the correlator cannot end up
// reasoning over a different history than the rest of the platform.
//
// WHAT IT DOES NOT DO. It performs no correlation. It returns observations; core/correlate.Assess decides,
// purely and deterministically, so the RULE is testable without a database and a Temporal replay reaches
// the same verdict as an after-the-fact review. Bound SELECTs only, every parameter bound ($1) — never
// string-built (INV-03). Read-only.
type CorrelationStore struct{ p *Pool }

// NewCorrelationStore returns the Postgres-backed correlation-window reader.
func NewCorrelationStore(p *Pool) *CorrelationStore { return &CorrelationStore{p: p} }

// MaxWindowRows bounds a single window read. The window is minutes wide and the estate is small, so this
// is generous headroom rather than a working limit — but a correlation read runs on the triage hot path
// BEFORE any expensive context is built, and an unbounded SELECT there would turn one broken, storming
// source into a slow front door for every other incident.
//
// BE HONEST ABOUT WHICH WAY IT FAILS. The cap is on ROWS while the rules count DISTINCT hosts and sources,
// so hitting it can only ever UNDER-count breadth — i.e. route an incident to a cheaper class than its
// evidence deserved, which is the wrong direction. That is acceptable only because the number is far above
// what this estate produces in a ten-minute window (~3,000 admitted alerts total), and because the ordering
// keeps the most recent rows rather than an arbitrary page. If an ingest source ever makes this cap
// reachable, the correct fix is a DISTINCT-on-(host,source) read, not a bigger number.
const MaxWindowRows = 500

// Window returns the alerts admitted within span of `at`, newest first, bounded by MaxWindowRows.
//
// received_at IS THE CLOCK, on both the filter and the returned observation time. observed_at is the
// PROVIDER's clock and is nullable; the window is a claim about what TG saw together, and provider clocks
// skew independently of one another. Two sources whose clocks disagree by minutes would otherwise fall out
// of each other's windows precisely when the cross-source rule is the one that matters.
//
// The bound is SYMMETRIC (at ± span) even though a live triage read can only ever see the past: the same
// reader is what an after-the-fact review of a persisted routing decision would use, and a review asking
// the asymmetric question would silently get a different cluster than the one that was routed on.
func (s *CorrelationStore) Window(ctx context.Context, at time.Time, span time.Duration) ([]correlate.Observation, error) {
	if span <= 0 || at.IsZero() {
		return nil, nil
	}
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, source_type, host, alert_rule, severity, received_at
		FROM ingest_alert
		WHERE received_at >= $1 AND received_at <= $2
		ORDER BY received_at DESC, id DESC
		LIMIT $3`, at.Add(-span), at.Add(span), MaxWindowRows)
	if err != nil {
		return nil, fmt.Errorf("db: correlation window: %w", err)
	}
	defer rows.Close()
	out := make([]correlate.Observation, 0, 16)
	for rows.Next() {
		var o correlate.Observation
		if err := rows.Scan(&o.ExternalRef, &o.SourceType, &o.Host, &o.AlertRule, &o.Severity, &o.At); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ExecClassStore is the append-only writer for the routing decision (exec_class_decision, migration 0058)
// — the audit trail the topology decision never had. See the migration for why the classifier INPUTS are
// persisted alongside the class, and why 'window-unavailable' is kept distinct from 'isolated'.
type ExecClassStore struct{ p *Pool }

// NewExecClassStore returns the Postgres-backed routing-decision recorder.
func NewExecClassStore(p *Pool) *ExecClassStore { return &ExecClassStore{p: p} }

// Record appends one routing decision. ON CONFLICT (external_ref) DO NOTHING: the stage runs once per
// incident but its activity retries like any other, and a retry must never accumulate duplicate rows on a
// table whose DELETE is revoked. DO NOTHING needs only INSERT, never the revoked UPDATE.
//
// An empty external_ref or class is refused here rather than at the CHECK constraint, so a caller with a
// half-built decision gets a message naming the field instead of a Postgres constraint error in a log.
func (s *ExecClassStore) Record(ctx context.Context, d correlate.Decision) error {
	if d.ExternalRef == "" {
		return fmt.Errorf("db: exec_class_decision: empty external_ref (a routing decision must name its session)")
	}
	if d.ExecClass == "" {
		return fmt.Errorf("db: exec_class_decision: empty exec_class for %s (a decision with no class is not a decision)", d.ExternalRef)
	}
	inputs, err := json.Marshal(d.Inputs)
	if err != nil {
		return fmt.Errorf("db: exec_class_decision: marshal inputs: %w", err)
	}
	// The evidence blob is the NON-SECRET identifier lists only — the same projection ingest_alert already
	// serves. Bounded upstream (correlate.MaxMembers) so a wide cascade cannot write an unbounded blob.
	evidence, err := json.Marshal(map[string]any{
		"hosts":   d.Verdict.Hosts,
		"sources": d.Verdict.Sources,
		"members": d.Verdict.Members,
	})
	if err != nil {
		return fmt.Errorf("db: exec_class_decision: marshal evidence: %w", err)
	}
	decidedAt := d.DecidedAt
	if decidedAt.IsZero() {
		decidedAt = time.Now().UTC()
	}
	_, err = s.p.Exec(ctx, `
		INSERT INTO exec_class_decision
		  (external_ref, exec_class, correlated, reason, degraded, window_seconds, distinct_hosts,
		   distinct_sources, member_count, inputs_json, evidence_json, decided_at, schema_version,
		   cluster_id, elected_ref, runner_up_ref, elect_rule)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12, 1, $13, $14, $15, $16)
		ON CONFLICT (external_ref) DO NOTHING`,
		d.ExternalRef, string(d.ExecClass), d.Verdict.Correlated, d.Verdict.Reason, d.Verdict.Degraded,
		int(d.Verdict.Span/time.Second), len(d.Verdict.Hosts), len(d.Verdict.Sources), d.Verdict.MemberCount,
		string(inputs), string(evidence), decidedAt,
		d.ClusterID, d.Election.Elected, d.Election.RunnerUp, d.Election.Rule)
	if err != nil {
		return fmt.Errorf("db: exec_class_decision insert %s: %w", d.ExternalRef, err)
	}
	return nil
}
