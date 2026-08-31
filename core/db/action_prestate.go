package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/actuate"
)

// ActionPreStateStore is the pgx-backed durable sink for TG-58 pre-mutation state captures (migration 0102): the
// snapshot the actuation chokepoint takes at the last pre-effect instant, persisted so a future Phase-2
// applied-undo executor has a concrete state to restore TO (the recorded inverse in execution_log, INV-07, says
// HOW to undo). It satisfies actuate.PreStateSink. LATEST-WINS on re-capture (action_id PK): action_id is
// content-addressed over the operation shape, so a repeated remediation overwrites — an undo targets the most
// recent execution of a shape, matching ActionExecutionStore.LatestExecution.
type ActionPreStateStore struct{ p *Pool }

// NewActionPreStateStore returns a Postgres-backed pre-mutation state recorder (satisfies actuate.PreStateSink).
func NewActionPreStateStore(p *Pool) *ActionPreStateStore { return &ActionPreStateStore{p: p} }

var _ actuate.PreStateSink = (*ActionPreStateStore)(nil)

// RecordPreState persists the snapshot for actionID, LATEST-WINS (a re-captured shape overwrites its prior
// pre-state). The interceptor calls this only after a confirmed real mutation, bound to the executed action_id.
func (s *ActionPreStateStore) RecordPreState(ctx context.Context, actionID string, st actuate.PreState) error {
	if strings.TrimSpace(actionID) == "" {
		return fmt.Errorf("db: RecordPreState requires an action_id")
	}
	_, err := s.p.Exec(ctx, `
		INSERT INTO action_prestate (action_id, kind, data, captured_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (action_id) DO UPDATE
		  SET kind = EXCLUDED.kind, data = EXCLUDED.data, captured_at = EXCLUDED.captured_at`,
		actionID, st.Kind, st.Data)
	if err != nil {
		return fmt.Errorf("db: record pre-state %s: %w", actionID, err)
	}
	return nil
}

// PreStateFor returns the captured pre-mutation snapshot for actionID — the future applied-undo executor's
// restore point. found=false means none was captured (distinct from an error, e.g. the action ran before this
// seam was armed). It is a READ ONLY projection and authorizes nothing.
func (s *ActionPreStateStore) PreStateFor(ctx context.Context, actionID string) (actuate.PreState, bool, error) {
	if strings.TrimSpace(actionID) == "" {
		return actuate.PreState{}, false, fmt.Errorf("db: PreStateFor requires an action_id")
	}
	var st actuate.PreState
	err := s.p.QueryRow(ctx, `SELECT kind, data FROM action_prestate WHERE action_id = $1`, actionID).
		Scan(&st.Kind, &st.Data)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return actuate.PreState{}, false, nil
		}
		return actuate.PreState{}, false, fmt.Errorf("db: pre-state for %s: %w", actionID, err)
	}
	return st, true, nil
}
