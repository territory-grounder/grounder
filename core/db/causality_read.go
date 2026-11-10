package db

// Causality evidence readers (TG-662; owner prompt 2026-11-07 §2.D): TG's own durable records of OTHER restore
// actors over a heal's interval — the fault injector's undo/re-injection rows and the provider recovery
// transitions. Read-only, positive-only: a count is evidence, an error is "unreadable" (the caller marks the
// causality indeterminate), never a silent zero.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/knowledge"
)

// CausalityStore reads injected_fault and ingest_transition for the terminus's causality classification.
type CausalityStore struct{ p *Pool }

// NewCausalityStore binds the readers to a pool.
func NewCausalityStore(p *Pool) *CausalityStore { return &CausalityStore{p: p} }

// InjectorActivity counts the injector's restores on any of hosts in [restoreFrom, to] and its injections in
// [injectFrom, to]. A restore inside the interval means the synthetic fault was put back by the injector; an
// injection inside it means a second fault overlapped the heal. Empty hosts read zero (nothing to match).
func (s *CausalityStore) InjectorActivity(ctx context.Context, hosts []string, restoreFrom, injectFrom, to time.Time) (restores, injections int, err error) {
	clean := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if h = strings.TrimSpace(h); h != "" {
			clean = append(clean, h)
		}
	}
	if len(clean) == 0 {
		return 0, 0, nil
	}
	if err := s.p.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE restored_at IS NOT NULL AND restored_at >= $2 AND restored_at <= $4),
		       count(*) FILTER (WHERE injected_at >= $3 AND injected_at <= $4)
		  FROM injected_fault
		 WHERE host = ANY($1)`, clean, restoreFrom, injectFrom, to).Scan(&restores, &injections); err != nil {
		return 0, 0, fmt.Errorf("db: injector activity on %v: %w", clean, err)
	}
	return restores, injections, nil
}

// RecoveriesBetween counts the provider recovery transitions captured for (host, rule family) in [from, to].
// A recovery BEFORE the heal executed means the fault was already clearing on its own or by another hand. An
// empty rule or an empty alias expansion is an ERROR (the reader cannot answer), never a zero — fail closed.
func (s *CausalityStore) RecoveriesBetween(ctx context.Context, host, alertRule string, from, to time.Time) (int, error) {
	if strings.TrimSpace(alertRule) == "" {
		return 0, fmt.Errorf("db: recoveries-between with empty alert rule — the reader cannot scope")
	}
	aliases := knowledge.RuleFamilyAliases(alertRule)
	if len(aliases) == 0 {
		return 0, fmt.Errorf("db: recoveries-between: rule %q expands to no aliases", alertRule)
	}
	var n int
	if err := s.p.QueryRow(ctx, `
		SELECT count(*) FROM ingest_transition
		 WHERE host = $1 AND alert_rule = ANY($2) AND kind = 'recovery' AND received_at >= $3 AND received_at <= $4`,
		host, aliases, from, to).Scan(&n); err != nil {
		return 0, fmt.Errorf("db: recoveries between for %s/%s: %w", host, alertRule, err)
	}
	return n, nil
}
