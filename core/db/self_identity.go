package db

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/manifest"
)

// SelfIdentityStore is the published self-identity projection (migration 0129, TG-684): the actuation plane appends
// the identity it derives from the credential it holds, the triage plane reads the newest per domain.
type SelfIdentityStore struct{ p *Pool }

// NewSelfIdentityStore binds the store to a pool.
func NewSelfIdentityStore(p *Pool) *SelfIdentityStore { return &SelfIdentityStore{p: p} }

// Publish appends (domain, actor, plane) unless the newest row for the domain already names the same actor — a
// reboot with the same key adds nothing, a rotated key adds a row.
func (s *SelfIdentityStore) Publish(ctx context.Context, domain, actor, plane string) (bool, error) {
	domain, actor, plane = strings.TrimSpace(domain), strings.TrimSpace(actor), strings.TrimSpace(plane)
	if domain == "" || actor == "" || plane == "" {
		return false, fmt.Errorf("db: self identity needs a domain, an actor and a plane")
	}
	cur, ok, err := s.Latest(ctx, domain)
	if err != nil {
		return false, err
	}
	if ok && cur == actor {
		return false, nil
	}
	if _, err := s.p.Exec(ctx, `INSERT INTO self_identity (domain, actor, plane) VALUES ($1, $2, $3)`, domain, actor, plane); err != nil {
		return false, fmt.Errorf("db: publish self identity for %s: %w", domain, err)
	}
	return true, nil
}

// Latest returns the newest published actor for a domain; ok=false when none has been published.
func (s *SelfIdentityStore) Latest(ctx context.Context, domain string) (string, bool, error) {
	rows, err := s.p.Query(ctx, `SELECT actor FROM self_identity WHERE domain = $1 ORDER BY published_at DESC, seq DESC LIMIT 1`, domain)
	if err != nil {
		return "", false, fmt.Errorf("db: read self identity for %s: %w", domain, err)
	}
	defer rows.Close()
	if rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return "", false, err
		}
		return a, true, rows.Err()
	}
	return "", false, rows.Err()
}

// ExecutionAt is one TG-recorded execution: when it ran, and the op-class its sealed manifest names ("" when the
// manifest carries none).
type ExecutionAt struct {
	At      time.Time
	OpClass string
}

// ExecutionsOn returns every TG-recorded execution on host inside [from, to] with the op-class of its sealed
// manifest — the counter-signature the journal reader checks a use of TG's own key against (TG-684). The op-class
// is what lets the caller keep only executions whose effect went over SSH: a Proxmox start never logs into the
// guest, so it cannot explain an sshd login there. Host matching is case-insensitive, as the journal reader
// lower-cases its subject. action_manifest is keyed by action_id, so the join adds no rows.
func (s *ActionExecutionStore) ExecutionsOn(ctx context.Context, host string, from, to time.Time) ([]ExecutionAt, error) {
	rows, err := s.p.Query(ctx, `
		SELECT e.executed_at, coalesce(m.action->>'op_class', '')
		FROM action_execution e LEFT JOIN action_manifest m ON m.action_id = e.action_id
		WHERE e.executed_at >= $2 AND e.executed_at <= $3 AND lower(e.target_host) = lower($1)
		ORDER BY e.executed_at`, strings.TrimSpace(host), from, to)
	if err != nil {
		return nil, fmt.Errorf("db: executions on %s: %w", host, err)
	}
	defer rows.Close()
	var out []ExecutionAt
	for rows.Next() {
		var e ExecutionAt
		if err := rows.Scan(&e.At, &e.OpClass); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ExecutionSince is one recorded execution with the sealed action of its manifest (TG-664). Action is the zero value
// with Target = the execution's target host when the manifest row is missing, so a caller deriving a resource path
// falls back to the WHOLE host — the widest, most conservative resource.
type ExecutionSince struct {
	ID          int64
	ExternalRef string
	At          time.Time
	Action      manifest.Action
}

// ExecutionsSince returns the executions recorded strictly after `after` that COULD touch host — the execution's
// target host, its sealed action's target, or its params.guest names host, case-insensitively, or starts with
// "<host>." (an FQDN spelling) — oldest first, at most limit rows (TG-664: the commit-confirm window's "has a later
// execution moved my resource?" read; the caller applies the exact containment test). Every verdict counts,
// including `indeterminate`: an execution that may have landed has to be assumed to have landed. The host filter
// keeps the read to the window's own host, so a busy estate cannot fill the page with unrelated executions.
func (s *ActionExecutionStore) ExecutionsSince(ctx context.Context, after time.Time, host string, limit int) ([]ExecutionSince, error) {
	if limit <= 0 {
		limit = 500
	}
	h := strings.ToLower(strings.TrimSpace(host))
	rows, err := s.p.Query(ctx, `
		SELECT e.id, e.external_ref, e.executed_at, e.target_host, m.action
		FROM action_execution e LEFT JOIN action_manifest m ON m.action_id = e.action_id
		WHERE e.executed_at > $1
		  AND (lower(e.target_host) = $2 OR starts_with(lower(e.target_host), $2 || '.')
		       OR lower(m.action->>'target') = $2 OR starts_with(lower(m.action->>'target'), $2 || '.')
		       OR lower(m.action->'params'->>'guest') = $2 OR starts_with(lower(m.action->'params'->>'guest'), $2 || '.'))
		ORDER BY e.executed_at, e.id
		LIMIT $3`, after, h, limit)
	if err != nil {
		return nil, fmt.Errorf("db: executions since %s: %w", after.UTC().Format(time.RFC3339), err)
	}
	defer rows.Close()
	var out []ExecutionSince
	for rows.Next() {
		var e ExecutionSince
		var host string
		var raw []byte
		if err := rows.Scan(&e.ID, &e.ExternalRef, &e.At, &host, &raw); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			if uerr := json.Unmarshal(raw, &e.Action); uerr != nil {
				log.Printf("db: execution %d: sealed action unreadable (%v) — counted as its whole host %q (TG-664)", e.ID, uerr, host)
				e.Action = manifest.Action{Target: host}
			}
		} else {
			e.Action = manifest.Action{Target: host}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
