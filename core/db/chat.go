package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/schema"
)

// ChatStore is the pgx-backed chat-event stream over chat_events + chat_cursor (migration 0004). Its
// load-bearing property is INV-12 IDEMPOTENCY: an event is inserted UNIQUE(source_id, event_id), so a
// redelivered event is a no-op — closing the predecessor's cross-room approval-misattribution and
// silent-event-loss class (H-04). The cursor is per-(source_id, room_id) with NO global cursor, so one
// lagging room never advances another's position and no event is skipped across rooms.
type ChatStore struct{ p *Pool }

// NewChatStore returns a Postgres-backed chat-event store.
func NewChatStore(p *Pool) *ChatStore { return &ChatStore{p: p} }

// Record inserts one chat event idempotently. It returns inserted=false when the (source_id, event_id) was
// already recorded — the caller can safely process each event exactly once. An empty source/event id is
// rejected (an event with no identity cannot be deduplicated).
func (s *ChatStore) Record(ctx context.Context, sourceID, eventID, roomID, payloadJSON string) (bool, error) {
	if sourceID == "" || eventID == "" {
		return false, errors.New("db: chat event requires source_id and event_id (INV-12 idempotency key)")
	}
	if payloadJSON == "" {
		payloadJSON = "{}"
	}
	v, err := schema.Stamp(schema.TableChatEvents)
	if err != nil {
		return false, err
	}
	tag, err := s.p.Exec(ctx, `
		INSERT INTO chat_events (source_id, event_id, room_id, payload, schema_version)
		VALUES ($1, $2, $3, $4::jsonb, $5)
		ON CONFLICT (source_id, event_id) DO NOTHING`,
		sourceID, eventID, roomID, payloadJSON, int(v))
	if err != nil {
		return false, fmt.Errorf("db: record chat event %s/%s: %w", sourceID, eventID, err)
	}
	return tag.RowsAffected() == 1, nil // 1 ⇒ newly inserted; 0 ⇒ a redelivery (idempotent no-op)
}

// AdvanceCursor moves the per-(source_id, room_id) cursor to lastEventID (upsert). NO global cursor exists.
func (s *ChatStore) AdvanceCursor(ctx context.Context, sourceID, roomID, lastEventID string) error {
	_, err := s.p.Exec(ctx, `
		INSERT INTO chat_cursor (source_id, room_id, last_event_id, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (source_id, room_id) DO UPDATE SET last_event_id = EXCLUDED.last_event_id, updated_at = now()`,
		sourceID, roomID, lastEventID)
	if err != nil {
		return fmt.Errorf("db: advance chat cursor %s/%s: %w", sourceID, roomID, err)
	}
	return nil
}

// Cursor returns the last processed event id for a (source_id, room_id), or "" when the room has no cursor.
func (s *ChatStore) Cursor(ctx context.Context, sourceID, roomID string) (string, error) {
	var last string
	err := s.p.QueryRow(ctx, "SELECT last_event_id FROM chat_cursor WHERE source_id = $1 AND room_id = $2", sourceID, roomID).Scan(&last)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("db: chat cursor %s/%s: %w", sourceID, roomID, err)
	}
	return last, nil
}
