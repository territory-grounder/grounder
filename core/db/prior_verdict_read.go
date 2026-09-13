package db

import (
	"context"
	"fmt"
	"log"
	"time"
)

// PriorVerdictStore is the READ-ONLY window over the durable ACTUATION-verdict ledger that feeds the
// classifier's prior-verdict band (spec/001 REQ-015, TG-223): the mechanical verdicts recorded for a target
// host inside a recency window, each tagged with the alert rule the session that produced it fired under.
//
// WHICH LEDGER, AND WHY NOT prediction_verdict. Migration 0042 split two populations that mean different
// things and whose pooling produced a rate describing neither:
//
//	action_verdict / action_execution — "TG DID X; did the estate change the way the prediction said?"  ACTUATION accuracy.
//	prediction_verdict               — "TG PREDICTED Y; did it happen?"  WORLD-MODEL accuracy, nothing was actuated.
//
// The band rule this store feeds is TG's own "a deviation can never auto-resolve again" (the graduation
// ladder's demote-on-deviation, core/policy/graduation.go) applied one step EARLIER, at classification. That
// rule is about what TG DID to a machine, so this store reads the ACTUATION lane only. Reading the
// propose-path lane here would re-commit exactly the error migration 0042 documents: measured at the split,
// 23 of 24 deviations were propose-path — a world model being wrong about an estate TG never touched — and
// wiring them into the band would park most hosts on POLL_PAUSE for a reason that names no mis-actuation.
// The exclusion is SAFE-DIRECTION-neutral: the branch is inert today, so a narrower source is never laxer
// than current behavior; it is simply honest about which evidence the rule is entitled to.
//
// TWO SOURCES, BOTH ACTUATION. action_execution (migration 0043) is the per-OCCURRENCE record — one row per
// execution, carrying external_ref and this run's own freshly-observed verdict — and is therefore the source
// where RECENCY is meaningful. action_verdict (migration 0004) is the durable per-SHAPE ledger, PK action_id
// first-wins, and is included for the pre-0043 era; its rows are restricted to genuinely EXECUTED actions by
// the same documented anti-join axis_read.go uses (a real execution always has an interceptor_gate_verdict
// row with gate='execute' AND verdict='pass'), so the ~49 legacy propose-path rows that predate the split
// cannot leak into an actuation-lane read.
//
// RULE-FAMILY SCOPING DELIBERATELY DOES NOT HAPPEN HERE. The family authority is core/knowledge.CanonicalRule
// (Go, case-insensitive), and pushing an alias list into SQL would re-create the two-vocabulary drift the
// recovery belt already paid for — the same reason IncidentHistoryStore folds in its caller. This store
// returns the host's recent verdicts WITH their alert rule; the caller folds by family.
//
// Read-only by construction: bound parameters ($1..$3), never string-built (INV-03).
type PriorVerdictStore struct {
	p *Pool
	// verify, when armed, checks the Ed25519 provenance signature on SIGNED action_verdict rows
	// (TG-81 b3): a signed row that fails verification is DROPPED from the result — treated as an
	// absent verdict (evidence removed, review raised), because it was not written through the
	// interceptor's VerdictSink by a seed-holding process. Unsigned rows ('' — every pre-0108 row and
	// every unarmed-writer row) always pass: pre-feature history stays evidence. nil ⇒ byte-identical
	// pre-b3 reads.
	verify VerdictVerifyFunc
}

// VerdictVerifyFunc reports whether signature is valid over the canonical verdict tuple.
type VerdictVerifyFunc func(actionID, planHash, verdict, targetHost, site, signature string) bool

// NewPriorVerdictStore returns the Postgres-backed prior-verdict reader.
func NewPriorVerdictStore(p *Pool) *PriorVerdictStore { return &PriorVerdictStore{p: p} }

// WithVerifier arms signature checking (chainable); nil is ignored — the seam cannot un-arm.
func (s *PriorVerdictStore) WithVerifier(f VerdictVerifyFunc) *PriorVerdictStore {
	if f != nil {
		s.verify = f
	}
	return s
}

// PriorVerdictRow is one durable actuation verdict as the ledger recorded it: the mechanical verdict, the
// alert rule of the session that produced it (the key the caller folds to a rule FAMILY), and when it was
// recorded. Every field is a recorded observation.
type PriorVerdictRow struct {
	Verdict   string // match | partial | deviation (the 0001 enum, rendered as text)
	AlertRule string // the alert rule the producing session fired under ('' when the session left no triage row)
	At        time.Time
}

// RecentForHost returns the durable actuation verdicts recorded at or after `since` for a host, newest first,
// bounded by limit (<=0 clamps to 50). The host predicate matches EITHER host expression — the verdict row's
// own target_host (the LLM-expressed action target) or the producing session's host (the ingest-validated
// alerted device) — because the two alternate across proposals for the same fault (TG-124); matching one only
// would silently drop half the evidence.
//
// An ABSENT judge/verdict table yields no rows and no error (honest: history begins when the spine does — the
// same degradation IncidentHistoryStore applies), so a deployment whose migrations have not reached 0043 reads
// as "no prior verdict" rather than as an outage the caller would have to interpret.
func (s *PriorVerdictStore) RecentForHost(ctx context.Context, host string, since time.Time, limit int) ([]PriorVerdictRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.p.Query(ctx, `
		SELECT v.verdict, v.alert_rule, v.at, v.action_id, v.plan_hash, v.target_host, v.site, v.signature
		FROM (
			-- Per-EXECUTION outcomes (migration 0043): one row per run, carrying THIS run's freshly-observed
			-- verdict and the external_ref that joins it to its session. A NULL verdict is executed-but-
			-- unverifiable (TG-182) and is deliberately NOT returned: it is not an adverse verdict, it is the
			-- absence of one, and the caller must treat absence as "unchanged", never as a deviation.
			-- This arm carries no signature (per-occurrence rows are unsigned; TG-81 b3 signs the per-shape
			-- ledger), so its empty tuple always passes the verify filter as pre-feature history.
			SELECT e.verdict::text AS verdict, st.alert_rule AS alert_rule, e.executed_at AS at,
			       '' AS action_id, '' AS plan_hash, '' AS target_host, '' AS site, '' AS signature
			  FROM action_execution e
			  JOIN session_triage st ON st.external_ref = e.external_ref
			 WHERE e.verdict IS NOT NULL
			   AND e.executed_at >= $2
			   AND (e.target_host = $1 OR st.host = $1)
			UNION ALL
			-- The durable per-SHAPE ledger (migration 0004), EXECUTED rows only via the documented anti-join.
			-- infragraph_prediction carries the external_ref (migration 0026) that bridges an action_id to the
			-- session whose alert_rule scopes it; kind='action' avoids the (plan_hash, kind) duplicate row.
			SELECT av.verdict::text, st.alert_rule, av.created_at,
			       av.action_id, av.plan_hash, av.target_host, COALESCE(av.site, ''), av.signature
			  FROM action_verdict av
			  JOIN infragraph_prediction ip
			    ON ip.action_id = av.action_id AND ip.kind = 'action' AND ip.external_ref <> ''
			  JOIN session_triage st ON st.external_ref = ip.external_ref
			 WHERE av.created_at >= $2
			   AND (av.target_host = $1 OR st.host = $1)
			   AND EXISTS (SELECT 1 FROM interceptor_gate_verdict g
			                WHERE g.action_id = av.action_id
			                  AND g.gate = 'execute' AND g.verdict = 'pass')
		) v
		ORDER BY v.at DESC
		LIMIT $3`, host, since, limit)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: prior verdicts for host %s: %w", host, err)
	}
	defer rows.Close()
	var out []PriorVerdictRow
	dropped := 0
	for rows.Next() {
		var r PriorVerdictRow
		var actionID, planHash, targetHost, site, signature string
		if err := rows.Scan(&r.Verdict, &r.AlertRule, &r.At, &actionID, &planHash, &targetHost, &site, &signature); err != nil {
			return nil, fmt.Errorf("db: scan prior verdict: %w", err)
		}
		// TG-81 b3: a SIGNED row must verify or it is not evidence — it was written around the
		// interceptor's VerdictSink. Unsigned rows ('') are pre-feature history and always pass; an
		// unarmed reader (nil verify) is byte-identical to pre-b3.
		if s.verify != nil && signature != "" && !s.verify(actionID, planHash, r.Verdict, targetHost, site, signature) {
			dropped++
			continue
		}
		out = append(out, r)
	}
	if dropped > 0 {
		log.Printf("db: prior-verdict read for %s DROPPED %d signed row(s) with INVALID signatures — verdict rows written around the VerdictSink; treating as absent evidence (TG-81 b3)", host, dropped)
	}
	return out, rows.Err()
}
