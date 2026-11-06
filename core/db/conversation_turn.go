package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ConversationStore is the pgx store for the cross-session conversation memory (TG-80 P2-8, migration
// 0109): per incident LINEAGE (canonical rule family + host), the digest of what each prior session
// concluded, TTL-bounded (the purgeable operational body, INV-14 — every row re-derivable from
// session_triage). Append via the triage terminal recorder; read at seed assembly; expiry deletion only
// through the reap_conversation_turn SECURITY DEFINER function (the append-only defaults leave the
// runtime role without DELETE).
type ConversationStore struct{ p *Pool }

// NewConversationStore returns the Postgres-backed conversation memory.
func NewConversationStore(p *Pool) *ConversationStore { return &ConversationStore{p: p} }

// ConversationTurn is one prior session's terminal digest on a lineage.
type ConversationTurn struct {
	ExternalRef string
	Content     string
	CreatedAt   time.Time
}

// DefaultConversationTTL bounds a turn's life: long enough that a weekly-recurring fault still sees its
// history, short enough that the hot tier never becomes an archive (the archive is session_triage).
const DefaultConversationTTL = 14 * 24 * time.Hour

// DefaultConversationReapBatch bounds one reap call, the evidence-reap convention.
const DefaultConversationReapBatch = 500

// Append records one terminal digest on a lineage. Degenerate inputs are refused loudly — an unkeyed
// turn could never be read back and would only leak rows past every Recent query.
func (s *ConversationStore) Append(ctx context.Context, key, ref, content string, ttl time.Duration) error {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(ref) == "" || strings.TrimSpace(content) == "" {
		return fmt.Errorf("db: conversation append requires key/ref/content")
	}
	if ttl <= 0 {
		ttl = DefaultConversationTTL
	}
	_, err := s.p.Exec(ctx, `
		INSERT INTO conversation_turn (conversation_key, external_ref, content, expires_at)
		VALUES ($1, $2, $3, now() + $4::interval)`,
		key, ref, content, ttl.String())
	if err != nil {
		return fmt.Errorf("db: conversation append %s: %w", key, err)
	}
	return nil
}

// Recent returns the lineage's newest live turns (newest first), excluding the asking session's own ref
// so a retried terminal never reads itself. An absent table reads as no history (the deployment's
// migrations have not reached 0109) rather than an error the seed path would have to interpret.
func (s *ConversationStore) Recent(ctx context.Context, key, excludeRef string, limit int) ([]ConversationTurn, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, content, created_at
		  FROM conversation_turn
		 WHERE conversation_key = $1 AND external_ref <> $2 AND expires_at > now()
		 ORDER BY created_at DESC
		 LIMIT $3`, key, excludeRef, limit)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: conversation recent %s: %w", key, err)
	}
	defer rows.Close()
	var out []ConversationTurn
	for rows.Next() {
		var t ConversationTurn
		if err := rows.Scan(&t.ExternalRef, &t.Content, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan conversation turn: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Reap deletes up to batch EXPIRED turns through the SECURITY DEFINER function — the only deletion path
// the runtime role holds, and it cannot name a row.
func (s *ConversationStore) Reap(ctx context.Context, batch int) (int, error) {
	if batch <= 0 {
		batch = DefaultConversationReapBatch
	}
	var n int
	if err := s.p.QueryRow(ctx, `SELECT reap_conversation_turn($1)`, batch).Scan(&n); err != nil {
		return 0, fmt.Errorf("db: conversation reap: %w", err)
	}
	return n, nil
}
