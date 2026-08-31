package db

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/learn"
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
	var id int64
	if err := s.p.QueryRow(ctx, `
		INSERT INTO ingest_transition
		  (external_ref, kind, source_id, host, site, alert_rule, observed_at, received_at, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1)
		RETURNING id`,
		rec.ExternalRef, kind, nullIfEmpty(rec.SourceID), rec.Host, rec.Site, rec.AlertRule, observed, received).Scan(&id); err != nil {
		log.Printf("db: ingest_transition append %s failed (non-blocking): %v", rec.ExternalRef, err)
		return
	}
	// A captured RECOVERY is a TERMINAL signal, not just clear-evidence for a resident workflow: reconcile it
	// to the incident's still-open work AT INGEST TIME so a recovery that arrives after the workflow is gone
	// (timed out, completed, or lost to a worker restart) still closes the session and obsoletes the proposal
	// it opened — the belt the LIVE-workflow rechecks (temporal/runner) cannot reach (TG-387). Best-effort and
	// non-blocking exactly like the append above: a reconcile error is logged, never propagated to the ingest
	// path. Scoped to kind='recovery' — no other transition kind carries a "the incident is over" meaning.
	if kind == "recovery" {
		s.reconcileRecovery(ctx, id, rec.Host, rec.AlertRule, received)
	}
}

// reconcileRecovery makes a just-captured recovery transition close the incident's still-open work, on the
// NATURAL KEY (host, alert_rule) — the join that actually links a recovery to its incident. external_ref
// CANNOT: LibreNMS mints a NEW alert-log id when an alert clears, so a recovery's external_ref never equals
// the fault's (measured 2026-08-06: external_ref → 0 matches, the natural key → 2,464 of 2,472). This is the
// ingest-time complement to the LIVE-workflow belts (temporal/runner: the vote-wait obsolete recheck and the
// clear-confirm loop), which only fire while the session's workflow is still resident — so a recovery that
// arrives after the workflow has gone reconciled nothing before this existed (TG-387).
//
// TWO SINGLE ATOMIC UPDATES, ONE PER TABLE — no read-then-write, so a recovery racing a live triage or vote
// cannot corrupt state: each UPDATE scopes to rows that are OPEN as of its own WHERE and the row-level lock
// serialises it against the workflow's own writer. Both are strictly the SAFE DIRECTION — they only CLOSE
// resolved work / WITHDRAW a proposal for a recovered incident; neither can reopen, re-actuate, or re-open a
// resolved row. OBSERVABILITY ONLY — confirmed_clear and the projected pending_decision re-enter no gate.
//
// The alert_rule is matched through knowledge.RuleFamilyAliases — the ONE rule-family authority the novelty
// signature, the recovery belt (RecoveredSince) and the verdict author already share. Matching with raw string
// equality here would re-open the exact defect that authority exists to close: modules/ingest/pveliveness
// raises under TG's own label "Device-Down" while every captured recovery carries a LibreNMS spelling
// ("Devices-up/down", …), so an estate-common class of incident would never reconcile. A rule in NO family
// expands to just itself, so an ordinary rule keeps EXACT (host, alert_rule) matching — a recovery for a
// genuinely different condition never closes this incident. An empty expansion scopes to nothing and returns
// without touching a row (fail closed, never an unscoped wildcard), mirroring RecoveredSince.
func (s *TransitionLogStore) reconcileRecovery(ctx context.Context, transitionID int64, host, alertRule string, recoveredAt time.Time) {
	if host == "" {
		return // no host to scope by ⇒ cannot answer ⇒ reconcile nothing (fail closed)
	}
	aliases := knowledge.RuleFamilyAliases(alertRule)
	if len(aliases) == 0 {
		return // empty/absent rule ⇒ an unscoped wildcard would false-close every host — never
	}

	// 1) CLOSE still-open triage sessions for this (host, rule-family) whose incident PREDATES the recovery,
	//    and stamp the closing transition. Guards, each load-bearing:
	//      NOT confirmed_clear  — only OPEN/un-cleared rows; a row already terminal is never re-closed or
	//                             re-stamped (IDEMPOTENT: a re-delivered recovery finds it clear and skips it),
	//                             and this preserves confirmed_clear's monotone false->true contract (MarkCleared).
	//      created_at < $4      — the incident must be OLDER than the recovery; a NEWER incident (a re-fire on
	//                             the same host+rule after this recovery) is left open — the recovery cannot
	//                             speak for an incident that opened after it (the safe-direction timestamp guard).
	if tag, err := s.p.Exec(ctx, `
		UPDATE session_triage
		   SET confirmed_clear = true, closed_by_transition_id = $1
		 WHERE host = $2 AND alert_rule = ANY($3)
		   AND NOT confirmed_clear
		   AND created_at < $4`,
		transitionID, host, aliases, recoveredAt); err != nil {
		log.Printf("db: reconcile recovery %d close-sessions (host=%s) failed (non-blocking): %v", transitionID, host, err)
	} else if n := tag.RowsAffected(); n > 0 {
		log.Printf("db: recovery transition %d closed %d open triage session(s) for %s/%s", transitionID, n, host, alertRule)
	}

	// 2) OBSOLETE still-open POLL_PAUSE proposals for the same (host, rule-family) that OPENED before the
	//    recovery. pending_decision carries no host/alert_rule, so it is scoped through its session_triage row
	//    (shared external_ref — the poll opens then records its triage row one activity later, workflow.go).
	//    status='open' is the atomic guard: a vote resolving concurrently flips it to 'resolved' and this UPDATE
	//    simply misses it (and vice-versa), so the two writers never clobber. opened_at < $1 is the same
	//    safe-direction timestamp guard — a poll opened after the recovery is a newer incident's, untouched.
	//    The outcome is DISTINCT from 'human:timeout' on purpose (cf. ReapAbandoned): the poll did not run its
	//    course unanswered — its subject recovered, which is a fact about the estate, not about the operator.
	if tag, err := s.p.Exec(ctx, `
		UPDATE pending_decision p
		   SET status = 'resolved', outcome = 'obsolete:subject-recovered', resolved_at = $1
		 WHERE p.status = 'open' AND p.opened_at < $1
		   AND EXISTS (
		       SELECT 1 FROM session_triage t
		        WHERE t.external_ref = p.external_ref
		          AND t.host = $2 AND t.alert_rule = ANY($3))`,
		recoveredAt, host, aliases); err != nil {
		log.Printf("db: reconcile recovery %d obsolete-proposals (host=%s) failed (non-blocking): %v", transitionID, host, err)
	} else if n := tag.RowsAffected(); n > 0 {
		log.Printf("db: recovery transition %d obsoleted %d open proposal(s) for %s/%s", transitionID, n, host, alertRule)
	}
}

// RecoveredSince reports whether a recovery transition for THIS (host, alertRule) was observed at or after
// `since` — the belt-and-braces lost-signal read for the workflow clear-confirm loop (a provider-asserted
// recovery TG durably captured, never the model's word, INV-11). Bound params; a query error returns
// (false, err) so the caller fails CLOSED (not recovered ⇒ hold To Verify), never a spurious clear.
//
// The alertRule predicate is load-bearing, not a refinement. Without it the belt answered "did ANYTHING on
// this host recover", so an unrelated flapping rule recovering elsewhere on the same host confirmed an
// incident whose OWN rule was still firing — a heal TG did not achieve, counted into the A3 heal numerator,
// auto-closing the ticket under an AUTO band, and writing a precedent that de-novels (host, rule) so the next
// occurrence loses its first-sight human review. The column has always been stored; it simply was not read.
// An EMPTY alertRule matches nothing and returns false — fail closed, never a wildcard.
// SCOPED TO THE RULE FAMILY, NOT TO ONE SPELLING (2026-07-30). String equality made a whole class of incident
// impossible to confirm as recovered. modules/ingest/pveliveness raises under TG's OWN label "Device-Down";
// every captured recovery transition carries a LibreNMS name ("Devices-up/down", "Device-Down-SNMP-unreachable",
// "Device-Down-Due-to-no-ICMP-response."). Measured live: 429/429/428 recovery rows under those three names and
// ZERO under "Device-Down", against 374 sessions raised with it. The vocabularies never intersect, so this belt
// answered "not recovered" forever, `obsoleted` never fired for a liveness-sourced incident, and the poll parked
// until its vote window expired — 16 open decisions whose target guests were all verified already running.
//
// This does NOT reopen the fail-open the rule predicate closes. knowledge.RuleFamilyAliases expands only within
// a deliberately narrow, git-reviewed family — rules denoting the SAME condition AND warranting the same
// remediation — and it explicitly EXCLUDES "TargetDown" (a scrape target down while the host is UP) and
// "Device-rebooted" (a reboot, not a persistent down). A rule in no family still matches EXACTLY, byte for byte
// as before. So a sibling alias's recovery is genuinely this incident's recovery, while an unrelated rule
// flapping on the same host still cannot confirm it.
func (s *TransitionLogStore) RecoveredSince(ctx context.Context, host, alertRule string, since time.Time) (bool, error) {
	if strings.TrimSpace(alertRule) == "" {
		return false, nil // no rule to scope by ⇒ the belt cannot answer ⇒ not recovered (fail closed)
	}
	aliases := knowledge.RuleFamilyAliases(alertRule)
	if len(aliases) == 0 {
		return false, nil // defensive: an empty expansion must never become an unscoped wildcard
	}
	var n int
	if err := s.p.QueryRow(ctx, `
		SELECT count(*) FROM ingest_transition
		WHERE host = $1 AND alert_rule = ANY($2) AND kind = 'recovery' AND received_at >= $3`,
		host, aliases, since).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// recoveryFeedCap bounds one RecoveryEventsSince pull so a backlog (first tick after a long gap, or a
// recovery storm) cannot balloon one feed pass; the cursor advances only to what was READ, so the next
// tick continues where this one stopped rather than skipping the remainder.
const recoveryFeedCap = 2048

// RecoveryCursor is the recovery feed's resume point: strictly after (At, ID) in (received_at, id) order.
// The id TIEBREAKER is load-bearing, not decoration: received_at defaults to now(), which Postgres
// evaluates ONCE per statement, so a bulk INSERT during a mass-recovery event stamps MANY rows with an
// identical received_at. A timestamp-only watermark that lands inside such a tied group at the cap
// boundary would permanently exclude the unread remainder (they can never satisfy `> watermark` again) —
// silent data loss, not delay. (Fresh-eyes review finding on this MR; the tied-batch oracle pins it.)
type RecoveryCursor struct {
	At time.Time
	ID int64
}

// RecoveryEventsSince returns the recovery transitions STRICTLY AFTER the cursor, in (received_at, id)
// order, as the co-occurrence learner's clear observations (TG-188 organic recovery: onset→clear pairing).
// The second return is the new cursor — the last row read, or the input unchanged when there were none —
// so the caller's next pull never re-feeds a row (ObserveClear is not idempotent: a re-fed clear could
// re-pair with a NEW onset of the next episode) and never loses one (the id tiebreaker resumes INSIDE a
// tied-timestamp group). Row-value comparison + bound params only.
func (s *TransitionLogStore) RecoveryEventsSince(ctx context.Context, cur RecoveryCursor) ([]learn.ClearObservation, RecoveryCursor, error) {
	rows, err := s.p.Query(ctx, `
		SELECT id, host, received_at FROM ingest_transition
		WHERE kind = 'recovery' AND (received_at, id) > ($1, $2)
		ORDER BY received_at ASC, id ASC
		LIMIT $3`, cur.At, cur.ID, recoveryFeedCap)
	if err != nil {
		return nil, cur, fmt.Errorf("db: recovery events: %w", err)
	}
	defer rows.Close()
	var out []learn.ClearObservation
	next := cur
	for rows.Next() {
		var id int64
		var c learn.ClearObservation
		if err := rows.Scan(&id, &c.Host, &c.At); err != nil {
			return nil, cur, fmt.Errorf("db: recovery event scan: %w", err)
		}
		out = append(out, c)
		next = RecoveryCursor{At: c.At, ID: id} // rows arrive in cursor order; the last one IS the resume point
	}
	if err := rows.Err(); err != nil {
		return nil, cur, fmt.Errorf("db: recovery events rows: %w", err)
	}
	return out, next, nil
}
