package db

import (
	"context"
	"fmt"
	"time"
)

// PriorJailbreaks counts the durably recorded jailbreak-polled classifications for an incident host at
// or after `since` (TG-80 P2-6, the repeat-offender half of the hostile disposition). The subject is
// the SESSION'S host (session_triage — the ingest-validated alerted device), joined from the audit row
// by external_ref, because session_risk_audit itself records no host and the action target is
// LLM-expressed and unstable across proposals for one fault (the TG-124 lesson). Read-only, bound
// parameters (INV-03); an absent table reads as zero — history begins when the spine does.
func (s *SessionReadStore) PriorJailbreaks(ctx context.Context, host string, since time.Time) (int, error) {
	var n int
	err := s.p.QueryRow(ctx, `
		SELECT count(*)
		  FROM session_risk_audit a
		  JOIN session_triage st ON st.external_ref = a.external_ref
		 WHERE st.host = $1
		   AND a.created_at >= $2
		   AND a.signals_json ->> 'poll_reason' = 'jailbreak-detected'`,
		host, since).Scan(&n)
	if err != nil {
		if isUndefinedTable(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("db: prior jailbreaks for %s: %w", host, err)
	}
	return n, nil
}
