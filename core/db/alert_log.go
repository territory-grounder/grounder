package db

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/knowledge"
)

// AlertLogStore is the pgx-backed DURABLE alert log (ingest_alert, migration 0033): the alert front door's own
// record of every accepted, normalized envelope, keyed by external_ref. It replaces the bounded in-memory
// MemAlertLog so the /v1/alerts view survives restart AND the decision-tracer can read the ingest boundary for
// any session. Append-only by table grant (the runtime role holds no UPDATE/DELETE, REQ-2016). Bound SELECTs
// only, every parameter bound ($1) — never string-built.
type AlertLogStore struct{ p *Pool }

// NewAlertLogStore returns the Postgres-backed durable alert log.
func NewAlertLogStore(p *Pool) *AlertLogStore { return &AlertLogStore{p: p} }

// compile-time proof it satisfies the read/write seam the ingest handler + alerts view depend on.
var _ httpapi.AlertLog = (*AlertLogStore)(nil)

// Append records one accepted, normalized alert. Best-effort by contract (AlertLog.Append returns no error —
// the ingest path must never block on the log); a write failure is logged, never propagated. INV-15: only an
// already-accepted envelope reaches here.
// nullIfEmpty renders an optional non-secret string for a NULLABLE column: "" becomes SQL NULL rather than
// an empty literal. Used for subject_ip, whose type is inet — an empty string is not a valid address, and
// storing one would fail the cast rather than record "no address supplied".
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// occurrenceRetryDelay is the pause before Append's single failure-path re-attempt. A package variable so
// the DB-gated oracles can size their windows; production never changes it.
var occurrenceRetryDelay = 150 * time.Millisecond

// occurrenceRetrySlots caps the async retries in flight. Append is called SYNCHRONOUSLY in the webhook
// request goroutine (core/httpapi/ingest.go) before the 202, so the retry must never run inline — the
// close review measured that an inline sleep+re-attempt would add 150ms–5s to every ack exactly during a
// DB blip in an alert storm, the precise pathology never-block-ingest forbids. It runs in a goroutine
// instead, and this buffered channel bounds how many can exist at once: under a persistent outage an
// alert storm would otherwise mint one goroutine per failed delivery without limit. A full pool downgrades
// the loss to LOST-without-retry — logged as its own state, never silently.
var occurrenceRetrySlots = make(chan struct{}, 64)

func (s *AlertLogStore) Append(ctx context.Context, rec httpapi.AlertRecord) {
	labels := rec.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	lj, err := json.Marshal(labels)
	if err != nil {
		lj = []byte("{}")
	}
	var observed any // nullable — a zero ObservedAt writes SQL NULL, never a spurious epoch
	if !rec.ObservedAt.IsZero() {
		observed = rec.ObservedAt
	}
	// TG-427: canonical + occurrence ride ONE transaction (BEGIN + 2×INSERT + COMMIT — two round trips
	// more than the old pair of Execs on the clean path, the stated price of killing the divergence
	// class: canonical-committed-without-its-first-occurrence was PERMANENT, because the source's
	// redelivery hits ON CONFLICT DO NOTHING and the first occurrence is gone forever. With the tx it is
	// both-or-neither, and "neither" on a first delivery is healed by the source's own redelivery).
	//
	// A failed transaction gets ONE bounded re-attempt OFF this goroutine: the caller is the live webhook
	// handler, and its ack must cost the same on the failure path as on the clean one. The retry runs
	// cancel-detached (the request completing cannot abort the durability lane) after the usual delay,
	// against the transient class — a pool blip, a statement-timeout moment. Nothing is ever propagated;
	// the three terminal states are distinct, greppable log lines that never share a call site:
	// "recovered on retry" / "LOST after retry" / "LOST without retry (pool exhausted)".
	firstErr := s.appendTx(ctx, rec, string(lj), observed)
	if firstErr == nil {
		return
	}
	select {
	case occurrenceRetrySlots <- struct{}{}:
	default:
		log.Printf("db: ingest append %s LOST without retry (retry pool exhausted, non-blocking): %v", rec.ExternalRef, firstErr)
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	go func() {
		defer func() { cancel(); <-occurrenceRetrySlots }()
		time.Sleep(occurrenceRetryDelay)
		if retryErr := s.appendTx(rctx, rec, string(lj), observed); retryErr != nil {
			log.Printf("db: ingest append %s LOST after retry (non-blocking): first=%v retry=%v", rec.ExternalRef, firstErr, retryErr)
			return
		}
		log.Printf("db: ingest append %s recovered on retry (non-blocking): first=%v", rec.ExternalRef, firstErr)
	}()
}

// appendTx writes the canonical front-door row and this delivery's occurrence row atomically.
//
// Canonical: ON CONFLICT (external_ref) DO NOTHING — a re-delivered webhook for an already-admitted alert
// is a no-op; the FIRST acceptance is the canonical record, and DO NOTHING needs only INSERT, never the
// revoked UPDATE. Occurrence (TG-399): a row per accepted delivery, no ON CONFLICT — each delivery IS a
// distinct occurrence, so count / first-seen / last-seen are derivable without rewriting the canonical
// row. Occurrence count remains a FLOOR under a persistent DB outage (never-block-ingest is deliberately
// chosen over exactness); what TG-427 removed is the silent single-leg divergence and the transient-blip
// loss, not the contract.
func (s *AlertLogStore) appendTx(ctx context.Context, rec httpapi.AlertRecord, labelsJSON string, observed any) error {
	tx, err := s.p.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
	if _, err := tx.Exec(ctx, `
		INSERT INTO ingest_alert
		  (external_ref, source_type, source_id, alert_rule, severity, host, site, summary, labels_json, observed_at, received_at, workflow_id, delivery_peer, delivery_host, subject_ip, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12, $13, $14, $15, 1)
		ON CONFLICT (external_ref) DO NOTHING`,
		rec.ExternalRef, rec.SourceType, rec.SourceID, rec.AlertRule, rec.Severity, rec.Host, rec.Site,
		rec.Summary, labelsJSON, observed, rec.ReceivedAt, rec.WorkflowID,
		rec.DeliveryPeer, rec.DeliveryHost,
		// inet is NULLABLE: an incident whose subject was NAMED has no address, and '' is not an address.
		// Passing the empty string would fail the inet cast; nil writes SQL NULL, which is the honest value.
		nullIfEmpty(rec.SubjectIP)); err != nil {
		return fmt.Errorf("canonical: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ingest_alert_occurrence
		  (external_ref, alert_rule, severity, host, site, observed_at, received_at, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1)`,
		rec.ExternalRef, rec.AlertRule, rec.Severity, rec.Host, rec.Site, observed, rec.ReceivedAt); err != nil {
		return fmt.Errorf("occurrence: %w", err)
	}
	return tx.Commit(ctx)
}

// AlertOccurrence is the re-fire history of one correlation key: how many accepted deliveries the front door
// recorded (first + every re-fire), and the first/last arrival. A never-seen ref reports Count 0 with zero
// times. (TG-399: ingest_alert keeps only the first occurrence, so this is the durable source of re-fire
// volume, and the LastSeen a dedup stage needs to tell a still-open incident from a resolved-and-refired one.)
//
// Count is a DELIVERY count: it counts accepted front-door deliveries, which includes any transport
// re-delivery of the same firing (a webhook retried after a slow 202). DistinctFirings (TG-427) is the
// provider-refire measure: distinct observed_at values — a provider re-FIRE carries a new observation
// time while a transport re-DELIVERY repeats the one it already sent. Deliveries whose source supplied no
// observation time cannot be told apart and count toward Count only, so DistinctFirings ≤ Count and both
// are floors under a persistent DB outage (see Append) — stated here rather than discovered downstream.
type AlertOccurrence struct {
	ExternalRef     string
	Count           int
	DistinctFirings int
	FirstSeen       time.Time
	LastSeen        time.Time
}

// Occurrences reports the delivery history for one correlation key from the append-only occurrence log.
// Authority is enforced upstream at the authenticated route; this reads the committed log only. Bound SELECT,
// $1-parameterized.
func (s *AlertLogStore) Occurrences(ctx context.Context, externalRef string) (AlertOccurrence, error) {
	occ := AlertOccurrence{ExternalRef: externalRef}
	var first, last *time.Time
	// count(DISTINCT observed_at) ignores NULLs by SQL semantics, which is the honest reading: an
	// unknown-time delivery is countable as a delivery but indistinguishable as a firing.
	if err := s.p.QueryRow(ctx, `
		SELECT count(*), count(DISTINCT observed_at), min(received_at), max(received_at)
		FROM ingest_alert_occurrence WHERE external_ref = $1`, externalRef).Scan(&occ.Count, &occ.DistinctFirings, &first, &last); err != nil {
		return AlertOccurrence{}, fmt.Errorf("db: alert occurrences %s: %w", externalRef, err)
	}
	if first != nil {
		occ.FirstSeen = *first
	}
	if last != nil {
		occ.LastSeen = *last
	}
	return occ, nil
}

// Recent returns up to limit accepted alerts, newest first. Authority is enforced upstream at the
// authenticated route; this reads the committed log only.
func (s *AlertLogStore) Recent(ctx context.Context, _ auth.Principal, limit int) ([]httpapi.AlertRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, source_type, source_id, alert_rule, severity, host, site, summary,
		       COALESCE(labels_json, '{}'::jsonb), observed_at, received_at, workflow_id
		FROM ingest_alert
		ORDER BY received_at DESC, id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]httpapi.AlertRecord, 0, limit)
	for rows.Next() {
		rec, err := scanAlertRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Counts reports the accepted-alert population behind the bounded page Recent returns. One pass with a
// FILTER rather than two round trips, so the badge never costs more than the page it annotates.
func (s *AlertLogStore) Counts(ctx context.Context, _ auth.Principal) (httpapi.AlertCounts, error) {
	var c httpapi.AlertCounts
	err := s.p.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE received_at >= now() - interval '24 hours')
		FROM ingest_alert`).Scan(&c.Total, &c.Last24h)
	if err != nil {
		return httpapi.AlertCounts{}, fmt.Errorf("db: alert counts: %w", err)
	}
	return c, nil
}

// ActiveHosts reports which of the given hosts have a RECENT alert (received_at >= since) — the LIVE evidence
// the runner's common-cause corroboration uses to tell an ISOLATED host-down (its co-tenants are quiet) from a
// shared-parent failure (many co-tenants alerting at once). Bound SELECT; the host set rides in as a single $1
// array so it is never string-built. Read-only. An empty host set short-circuits without a query.
func (s *AlertLogStore) ActiveHosts(ctx context.Context, hosts []string, since time.Time) (map[string]bool, error) {
	out := map[string]bool{}
	if len(hosts) == 0 {
		return out, nil
	}
	rows, err := s.p.Query(ctx, `
		SELECT DISTINCT host FROM ingest_alert WHERE host = ANY($1) AND received_at >= $2`, hosts, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out[h] = true
	}
	return out, rows.Err()
}

// CollateralAlert is one (host, alert_rule) that FIRST surfaced inside a collateral observation window —
// the TG-483 terminus re-check's unit of evidence.
type CollateralAlert struct {
	Host      string
	AlertRule string
}

// collateralPreexistLookback bounds the "was this pair already firing BEFORE the heal?" exclusion scan.
// A pair with any delivery in [since-lookback, since) is pre-existing noise, not collateral — the heal
// cannot have caused what was already alerting. Bounded so the NOT EXISTS never walks unbounded history.
const collateralPreexistLookback = 24 * time.Hour

// CollateralOpenedSince reports the (host, alert_rule) pairs that FIRST surfaced on any of `hosts` at or
// after `since` — the TG-483 collateral question: "did OUR heal down a sibling inside the settle window?".
// Reads TG's own durable per-delivery capture (ingest_alert_occurrence, migration 0074), never a live
// provider pull, so the answer is replay-stable and INV-11-clean (a provider push TG recorded, not the
// model's word).
//
// Three exclusions are load-bearing:
//   - the incident's OWN (excludeHost, rule-family) — that alert is the thing being healed, and its
//     re-fire is the flap/ConfirmedClear machinery's business, not collateral;
//   - the family expansion mirrors RecoveredSince (one condition, several provider spellings) so the
//     incident's own alert under a sibling spelling cannot masquerade as collateral;
//   - pairs with any delivery in the pre-existing lookback window — already-firing noise the heal cannot
//     have caused. A REDELIVERY inside the window therefore never counts; only a genuine first-sight does.
//
// Empty hosts ⇒ (nil, nil): there is nothing to ask, and the CALLER decides what absence means (the
// activity reports unknown, never a fabricated all-clear — a check that cannot report "nothing to check"
// is how vacuous greens happen). Bound params; LIMIT bounds a storm.
func (s *AlertLogStore) CollateralOpenedSince(ctx context.Context, hosts []string, excludeHost, excludeRule string, since time.Time) ([]CollateralAlert, error) {
	if len(hosts) == 0 {
		return nil, nil
	}
	aliases := knowledge.RuleFamilyAliases(excludeRule)
	if len(aliases) == 0 {
		aliases = []string{excludeRule} // defensive: exclusion must never silently widen to "exclude nothing of this host"
	}
	rows, err := s.p.Query(ctx, `
		SELECT DISTINCT o.host, o.alert_rule
		FROM ingest_alert_occurrence o
		WHERE o.host = ANY($1) AND o.received_at >= $2
		  AND NOT (o.host = $3 AND o.alert_rule = ANY($4))
		  AND NOT EXISTS (
		    SELECT 1 FROM ingest_alert_occurrence p
		    WHERE p.host = o.host AND p.alert_rule = o.alert_rule
		      AND p.received_at < $2 AND p.received_at >= $5)
		ORDER BY o.host, o.alert_rule
		LIMIT 32`,
		hosts, since, excludeHost, aliases, since.Add(-collateralPreexistLookback))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CollateralAlert
	for rows.Next() {
		var c CollateralAlert
		if err := rows.Scan(&c.Host, &c.AlertRule); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// scanAlertRow scans one ingest_alert row into the non-secret AlertRecord projection (shared by Recent and the
// tracer's ByRef read). observed_at is nullable (pgx scans SQL NULL into a nil *time.Time); labels degrade to nil.
func scanAlertRow(row interface{ Scan(...any) error }) (httpapi.AlertRecord, error) {
	var (
		rec        httpapi.AlertRecord
		labelsJSON []byte
		observed   *time.Time
	)
	if err := row.Scan(&rec.ExternalRef, &rec.SourceType, &rec.SourceID, &rec.AlertRule, &rec.Severity,
		&rec.Host, &rec.Site, &rec.Summary, &labelsJSON, &observed, &rec.ReceivedAt, &rec.WorkflowID); err != nil {
		return rec, err
	}
	if observed != nil {
		rec.ObservedAt = *observed
	}
	if len(labelsJSON) > 0 {
		_ = json.Unmarshal(labelsJSON, &rec.Labels) // best-effort; a bad labels blob degrades to nil, not a failure
	}
	return rec, nil
}
